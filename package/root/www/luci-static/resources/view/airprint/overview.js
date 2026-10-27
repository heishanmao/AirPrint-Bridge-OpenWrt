'use strict';
'require view';
'require uci';
'require form';
'require fs';
'require poll';
'require ui';

var DEFAULTS = {
	enabled: '1',
	dpi: '300',
	printer_uri: 'ipps://192.168.31.6:631/ipp/print',
	queue_name: 'EPSON_L3250',
	listen_port: '6310',
	service_name: 'EPSON L3250 Series',
	service_uuid: '0ea66e10-844a-3a74-4c05-5b17c7daffa8',
	note: '',
	auto_recover: '1',
	offline_guard: '1',
	notify_url: '',
	notify_kind: 'auto',
	notify_events: 'start,complete,error,recover',
	history_max: '20',
	debug: '0'
};

var LEVEL_TEXT = { ok: '运行正常', warn: '注意', bad: '异常', idle: '空闲' };
var EVENT_LABELS = [
	[ 'start', '开始打印' ],
	[ 'complete', '打印完成' ],
	[ 'error', '打印失败' ],
	[ 'recover', '队列恢复' ]
];

function cfg(key) {
	var value = null;
	try {
		value = uci.get('airprint', 'main', key);
	} catch (e) {
		value = null;
	}
	return (value === null || value === undefined || value === '') ? DEFAULTS[key] : value;
}

function enabledValue() {
	try {
		return uci.get('airprint', 'main', 'enabled') === '0' ? '0' : '1';
	} catch (e) {
		return '1';
	}
}

function statusUrl() {
	var port = cfg('listen_port') || '6310';
	return 'http://' + window.location.hostname + ':' + port + '/status.json';
}

function fetchStatus() {
	return fetch(statusUrl(), { cache: 'no-store' }).then(function(resp) {
		if (!resp.ok)
			throw new Error('HTTP ' + resp.status);
		return resp.json();
	});
}

function ago(sec) {
	sec = Math.max(0, parseInt(sec || 0, 10));
	if (sec < 60)
		return sec + ' 秒';
	if (sec < 3600)
		return Math.floor(sec / 60) + ' 分 ' + (sec % 60) + ' 秒';
	return Math.floor(sec / 3600) + ' 时 ' + Math.floor((sec % 3600) / 60) + ' 分';
}

function shortTime(value) {
	if (!value)
		return '-';
	return value.slice(5, 16).replace('T', ' ');
}

function stateClass(state) {
	return state === 9 ? 'ok' : ((state === 7 || state === 8) ? 'bad' : 'warn');
}

function pill(text, cls) {
	return E('span', { 'class': 'ap-pill ' + (cls || '') }, text);
}

function kv(key, value) {
	return E('div', { 'class': 'ap-kv-item' }, [
		E('span', {}, key),
		E('span', {}, String(value))
	]);
}

function field(label, id, input, help) {
	return E('div', { 'class': 'ap-field' }, [
		E('label', { 'class': 'ap-label', 'for': id }, label),
		input,
		help ? E('div', { 'class': 'ap-help' }, help) : ''
	]);
}

function checkbox(id, label, checked) {
	return E('label', { 'class': 'ap-flag', 'for': id }, [
		E('input', { 'type': 'checkbox', 'id': id, 'class': 'cbi-input-checkbox',
			'checked': checked ? true : null }),
		E('span', {}, label)
	]);
}

function fv(id) {
	var node = document.getElementById(id);
	return node ? String(node.value || '').trim() : '';
}

function fc(id) {
	var node = document.getElementById(id);
	return node && node.checked ? '1' : '0';
}

return view.extend({
	load: function() {
		return uci.load('airprint').then(function() {
			if (enabledValue() === '0')
				return { enabled: false };
			return fetchStatus().then(function(status) {
				return { enabled: true, status: status };
			}).catch(function(err) {
				return { enabled: true, error: err.message };
			});
		}).catch(function(err) {
			return { enabled: true, error: '无法读取 UCI 配置：' + err.message };
		});
	},

	render: function(loaded) {
		var self = this;
		this.enabled = loaded.enabled !== false;
		this.lastStatus = loaded.status || null;

		this.badge = E('span', { 'class': 'ap-badge ap-idle' }, '连接中');
		this.notice = E('div', { 'class': 'ap-notice' });
		this.stageBox = E('div', { 'class': 'ap-stages' });
		this.activeBox = E('div');
		this.lastBox = E('div');
		this.historyBox = E('div');
		this.summaryBox = E('div', { 'class': 'ap-kv' });
		this.logsBox = E('div');
		this.stamp = E('div', { 'class': 'ap-stamp' }, '等待数据');

		var configBox = E('div');
		var configPromise = this.renderConfig();

		var page = E('div', { 'class': 'ap-page' }, [
			E('style', {}, [ [
				'.ap-page{width:100%}',
				'.ap-head{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin:10px 0 16px}',
				'.ap-head .ap-spacer{flex:1}',
				'.ap-badge{display:inline-flex;align-items:center;padding:4px 11px;border-radius:999px;font-size:12px;font-weight:600;border:1px solid rgba(127,127,127,.3)}',
				'.ap-badge.ok{color:#16a34a;background:rgba(22,163,74,.1)}',
				'.ap-badge.warn{color:#d97706;background:rgba(217,119,6,.1)}',
				'.ap-badge.bad{color:#dc2626;background:rgba(220,38,38,.1)}',
				'.ap-badge.idle{color:#64748b;background:rgba(100,116,139,.1)}',
				'.ap-card{background:rgba(127,127,127,.06);border:1px solid rgba(127,127,127,.2);border-radius:10px;padding:14px 16px;margin-bottom:14px}',
				'.ap-card h3{margin:0 0 10px;font-size:15px}',
				'.ap-notice:empty{display:none}',
				'.ap-notice{padding:8px 10px;border-radius:6px;background:rgba(220,38,38,.08);color:#dc2626;font-size:12.5px;margin-bottom:10px}',
				'.ap-notice.idle{background:rgba(100,116,139,.1);color:#64748b}',
				'.ap-stages{display:grid;grid-template-columns:repeat(auto-fit,minmax(155px,1fr));gap:8px}',
				'.ap-stage{background:rgba(127,127,127,.06);border:1px solid rgba(127,127,127,.18);border-left:3px solid #64748b;border-radius:7px;padding:9px 11px;min-height:62px}',
				'.ap-stage.ok{border-left-color:#16a34a}.ap-stage.warn{border-left-color:#d97706}.ap-stage.bad{border-left-color:#dc2626}',
				'.ap-stage-head{display:flex;align-items:center;gap:6px;font-size:12px;opacity:.75}',
				'.ap-dot{width:7px;height:7px;border-radius:50%;background:#64748b;flex:none}',
				'.ap-stage.ok .ap-dot{background:#16a34a}.ap-stage.warn .ap-dot{background:#d97706}.ap-stage.bad .ap-dot{background:#dc2626}',
				'.ap-stage-detail{font-size:12.5px;margin-top:5px;word-break:break-word}',
				'.ap-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(250px,1fr));gap:10px 16px}',
				'.ap-field{display:flex;flex-direction:column;gap:4px}',
				'.ap-label{font-size:12px;font-weight:600;opacity:.8}',
				'.ap-help{font-size:11.5px;opacity:.6;line-height:1.45}',
				'.ap-flags{display:flex;gap:16px;flex-wrap:wrap;margin-top:10px}',
				'.ap-flag{display:flex;align-items:center;gap:6px;font-size:13px;margin:2px 0}',
				'.ap-actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:14px}',
				'.ap-kv{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:2px 18px;margin-top:10px}',
				'.ap-kv-item{display:flex;justify-content:space-between;gap:10px;border-bottom:1px dashed rgba(127,127,127,.2);padding:4px 0;font-size:12.5px}',
				'.ap-table{width:100%;border-collapse:collapse;font-size:12.5px}',
				'.ap-table th,.ap-table td{padding:6px 8px;border-bottom:1px solid rgba(127,127,127,.15);text-align:left;vertical-align:top}',
				'.ap-table th{font-weight:600;opacity:.7;white-space:nowrap}',
				'.ap-scroll{overflow-x:auto}',
				'.ap-job{display:flex;justify-content:space-between;gap:10px;flex-wrap:wrap;align-items:center}',
				'.ap-job-name{font-weight:600;font-size:14px;word-break:break-all}',
				'.ap-pill{display:inline-block;font-size:11.5px;padding:2px 8px;border-radius:999px;background:rgba(127,127,127,.14);white-space:nowrap}',
				'.ap-pill.ok{color:#16a34a}.ap-pill.warn{color:#d97706}.ap-pill.bad{color:#dc2626}',
				'.ap-bar{height:7px;background:rgba(127,127,127,.2);border-radius:99px;overflow:hidden;margin:8px 0 4px}',
				'.ap-bar>span{display:block;height:100%;width:0;background:#3b82f6;transition:width .4s}',
				'.ap-bar.indet>span{width:100%;background:linear-gradient(90deg,#3b82f6,#93c5fd,#3b82f6);background-size:200% 100%;animation:ap-mv 1.4s linear infinite}',
				'@keyframes ap-mv{to{background-position:-200% 0}}',
				'.ap-diag{margin-top:8px;font-size:13px}.ap-diag.ok{color:#16a34a}.ap-diag.warn{color:#d97706}.ap-diag.bad{color:#dc2626}',
				'.ap-log{max-height:220px;overflow:auto;background:rgba(127,127,127,.1);border-radius:6px;padding:8px;font-size:11.5px;white-space:pre-wrap;word-break:break-all;margin:6px 0 0}',
				'.ap-stamp{font-size:11.5px;opacity:.55;margin-top:6px}',
				'.ap-empty{font-size:13px;opacity:.6}',
				'@media(max-width:600px){.ap-hide{display:none}}'
			].join('\n') ]),

			E('h2', {}, 'AirPrint 打印桥'),
			E('p', { 'class': 'cbi-section-descr' },
				'iPhone 通过 mDNS 发现这里，代理补全 URF/AirPrint 能力后交给 CUPS，再由自定义后端逐页发送到 EPSON L3250。'),

			E('div', { 'class': 'ap-head' }, [
				this.badge,
				E('span', { 'class': 'ap-spacer' }),
				E('button', { 'class': 'cbi-button cbi-button-neutral', 'click': ui.createHandlerFn(this, 'handleRefresh') }, '重新检测'),
				E('button', { 'class': 'cbi-button cbi-button-neutral', 'click': ui.createHandlerFn(this, 'handleCheck') }, '运行自检')
			]),

			E('div', { 'class': 'ap-card' }, [
				E('h3', {}, '链路状态'),
				this.notice,
				this.stageBox,
				this.summaryBox
			]),

			E('div', { 'class': 'ap-card' }, [
				E('h3', {}, '当前任务'),
				this.activeBox
			]),

			E('div', { 'class': 'ap-card' }, [
				E('h3', {}, '上次结果'),
				this.lastBox
			]),

			E('div', { 'class': 'ap-card' }, [
				E('h3', {}, '任务历史'),
				this.historyBox
			]),

			configBox,

			E('div', { 'class': 'ap-card' }, [
				E('h3', {}, '日志'),
				this.logsBox
			]),

			this.stamp
		]);

		if (loaded.status)
			this.renderStatus(loaded.status);
		else
			this.renderOffline(loaded.error);

		poll.add(function() {
			return self.refresh();
		}, 3);

		return Promise.resolve(configPromise).then(function(node) {
			configBox.appendChild(node);
			return page;
		});
	},

	renderConfig: function() {
		var m = new form.Map('airprint');
		this.map = m;
		var s = m.section(form.NamedSection, 'main', 'airprint', '设置',
			'保存后会重新生成 Avahi 广播、同步 CUPS 并重启代理。配置升级时不会丢失。');
		s.addremove = false;

		var o;
		o = s.option(form.Flag, 'enabled', '启用 AirPrint 打印桥');
		o.default = o.enabled;
		o.rmempty = false;
		o = s.option(form.Flag, 'auto_recover', '队列停止时自动恢复');
		o.default = o.enabled;
		o = s.option(form.Flag, 'offline_guard', '打印机离线时拦截任务');
		o.default = o.enabled;
		o = s.option(form.Flag, 'debug', '调试日志');
		o.default = o.disabled;

		o = s.option(form.ListValue, 'dpi', '打印分辨率',
			'修改后 iPhone 发送的 URF 分辨率会同步变化。');
		o.value('300', '300 dpi（推荐）');
		o.value('600', '600 dpi（约 4 倍数据）');

		o = s.option(form.Value, 'printer_uri', '打印机 URI',
			'必须是 ipps:// 地址，后端使用 TLS 发送 PWG。');
		o.placeholder = 'ipps://192.168.31.6:631/ipp/print';

		o = s.option(form.Value, 'queue_name', 'CUPS 队列名',
			'队列不存在时会用内置 PPD 自动创建。');
		o.placeholder = 'EPSON_L3250';

		o = s.option(form.Value, 'listen_port', '监听端口',
			'默认 6310；改动后 iPhone 可能需要重新识别打印机。');
		o.datatype = 'port';
		o.placeholder = '6310';

		o = s.option(form.Value, 'service_name', '服务名称',
			'与 UUID 一起决定 iPhone 里显示的名称。');
		o.placeholder = 'EPSON L3250 Series';

		o = s.option(form.Value, 'note', '备注',
			'显示在 iPhone 打印机详情里；留空自动生成。');

		o = s.option(form.Value, 'history_max', '历史保留条数',
			'写入 /var/log/airprint/jobs.jsonl。');
		o.datatype = 'range(10,2000)';
		o.placeholder = '200';

		o = s.option(form.Value, 'notify_url', '推送地址', '留空关闭推送。');
		o.placeholder = 'https://api.day.app/KEY 或 https://ntfy.sh/TOPIC';

		o = s.option(form.ListValue, 'notify_kind', '推送类型',
			'auto：地址包含 api.day.app 时用 Bark，否则用 ntfy。');
		o.value('auto', '自动识别（Bark / ntfy）');
		o.value('bark', 'Bark');
		o.value('ntfy', 'ntfy');
		o.value('generic', '通用 JSON');

		o = s.option(form.Value, 'notify_events', '推送事件',
			'逗号分隔：start,complete,error,recover');
		o.placeholder = 'start,complete,error,recover';

		o = s.option(form.Value, 'service_uuid', '服务 UUID',
			'一般不要改；改动会让 iPhone 把它当成新的打印机。');

		return m.render();
	},

	setBadge: function(level, text) {
		this.badge.className = 'ap-badge ap-' + level;
		this.badge.textContent = text;
	},

	renderOffline: function(error) {
		var disabled = !this.enabled || enabledValue() === '0';
		this.notice.className = 'ap-notice' + (disabled ? ' idle' : '');
		if (disabled) {
			this.setBadge('idle', '已停用');
			this.notice.textContent = 'AirPrint 打印桥已停用。勾选“启用”并保存后，会重新广播并启动代理。';
		} else {
			this.setBadge('bad', '代理未响应');
			this.notice.textContent = '无法读取代理状态：' + (error || '连接失败') +
				'。请确认服务已启动（/etc/init.d/airprint status），或点击“运行自检”。';
		}
		if (!this.lastStatus) {
			this.stageBox.textContent = '';
			this.activeBox.textContent = '';
			this.activeBox.appendChild(E('div', { 'class': 'ap-empty' }, '暂无数据'));
			this.lastBox.textContent = '';
			this.lastBox.appendChild(E('div', { 'class': 'ap-empty' }, '暂无数据'));
			this.historyBox.textContent = '';
			this.historyBox.appendChild(E('div', { 'class': 'ap-empty' }, '暂无历史'));
			this.summaryBox.textContent = '';
			this.logsBox.textContent = '';
			this.stamp.textContent = disabled ? '已停用' : '等待代理';
		}
	},

	renderStatus: function(data) {
		this.lastStatus = data;
		this.enabled = true;
		var level = LEVEL_TEXT[data.level] ? data.level : 'warn';
		var badNames = (data.stages || []).filter(function(stage) {
			return stage.level === 'bad';
		}).map(function(stage) {
			return stage.name;
		});
		var badgeText = LEVEL_TEXT[level];
		if (badNames.length)
			badgeText += ' · ' + badNames.join(' / ');
		this.setBadge(level, badgeText);
		this.notice.className = 'ap-notice';
		this.notice.textContent = '';

		this.stageBox.textContent = '';
		(data.stages || []).forEach(function(stage) {
			this.stageBox.appendChild(E('div', { 'class': 'ap-stage ' + stage.level }, [
				E('div', { 'class': 'ap-stage-head' }, [
					E('span', { 'class': 'ap-dot' }),
					E('span', {}, stage.name)
				]),
				E('div', { 'class': 'ap-stage-detail' }, stage.detail)
			]));
		}, this);

		this.renderActive(data.active || []);
		this.renderLast(data.last);
		this.renderHistory(data.history || []);
		this.renderSummary(data);
		this.renderLogs(data.logs || {});
		this.stamp.textContent = '刷新于 ' + String(data.time || '').replace('T', ' ') + ' · v' + data.version;
	},

	renderActive: function(list) {
		var queued = parseInt((this.lastStatus || {}).queued || 0, 10);
		this.activeBox.textContent = '';
		if (!list.length && !queued) {
			this.activeBox.appendChild(E('div', { 'class': 'ap-empty' }, '当前没有打印任务'));
			return;
		}
		if (!list.length)
			this.activeBox.appendChild(E('div', { 'class': 'ap-empty' }, 'CUPS 队列中有 ' + queued + ' 个任务'));
		list.forEach(function(job) {
			var bar = E('div', { 'class': 'ap-bar' + (job.progress === null ? ' indet' : '') });
			var fill = E('span');
			if (job.progress !== null)
				fill.style.width = Math.max(3, job.progress) + '%';
			bar.appendChild(fill);
			var label = job.expected ? (job.pages + ' / ' + job.expected + ' 页') : (job.pages + ' 页已打印');
			this.activeBox.appendChild(E('div', {}, [
				E('div', { 'class': 'ap-job' }, [
					E('div', {}, [
						E('div', { 'class': 'ap-job-name' }, job.name || '未命名任务'),
						E('div', { 'class': 'ap-help' }, [ job.user ? ('用户 ' + job.user + ' · ') : '', '已用时 ' + ago(job.elapsed_s) ].join(''))
					]),
					pill(job.state_text || '处理中', 'warn')
				]),
				bar,
				E('div', { 'class': 'ap-help' }, label + (job.side_status === 'printing' ? ' · 正在发送到打印机' : '')),
				(job.reasons && job.reasons.length) ? E('div', { 'class': 'ap-help' }, job.reasons.join(', ')) : ''
			]));
		}, this);
		this.activeBox.appendChild(E('div', { 'class': 'ap-actions' },
			E('button', { 'class': 'cbi-button cbi-button-reset', 'click': ui.createHandlerFn(this, 'handleClearJobs') }, '清空队列')));
	},

	renderLast: function(last) {
		this.lastBox.textContent = '';
		if (!last) {
			this.lastBox.appendChild(E('div', { 'class': 'ap-empty' }, '还没有完成记录'));
			return;
		}
		var cls = stateClass(last.state);
		this.lastBox.appendChild(E('div', { 'class': 'ap-job' }, [
			E('div', { 'class': 'ap-job-name' }, last.name || '未命名任务'),
			pill(last.state_text || '已结束', cls)
		]));
		this.lastBox.appendChild(E('div', { 'class': 'ap-help',
			'style': 'margin-top:6px' }, [
			shortTime(last.end), ' · ', (last.pages || 0) + ' 页 · ', '用时 ' + ago(last.duration_s)
		].join('')));
		this.lastBox.appendChild(E('div', { 'class': 'ap-diag ' + cls }, last.diagnosis || ''));
		if (last.detail)
			this.lastBox.appendChild(E('div', { 'class': 'ap-help', 'style': 'margin-top:4px' }, '日志线索：' + last.detail));
	},

	renderHistory: function(rows) {
		this.historyBox.textContent = '';
		if (!rows.length) {
			this.historyBox.appendChild(E('div', { 'class': 'ap-empty' }, '暂无历史'));
			return;
		}
		var body = E('tbody');
		rows.slice(0, 10).forEach(function(rec) {
			var cls = stateClass(rec.state);
			body.appendChild(E('tr', {}, [
				E('td', {}, shortTime(rec.end)),
				E('td', {}, rec.name || '-'),
				E('td', { 'class': 'ap-hide' }, String(rec.pages || 0)),
				E('td', { 'class': 'ap-hide' }, ago(rec.duration_s)),
				E('td', {}, pill(rec.state_text || '-', cls)),
				E('td', { 'title': rec.detail || '' }, rec.diagnosis || '')
			]));
		});
		this.historyBox.appendChild(E('div', { 'class': 'ap-scroll' }, E('table', { 'class': 'ap-table' }, [
			E('thead', {}, E('tr', {}, [
				E('th', {}, '时间'), E('th', {}, '任务'),
				E('th', { 'class': 'ap-hide' }, '页数'),
				E('th', { 'class': 'ap-hide' }, '用时'),
				E('th', {}, '结果'), E('th', {}, '诊断')
			])),
			body
		])));
	},

	renderSummary: function(data) {
		var c = data.config || {};
		var s = data.system || {};
		this.summaryBox.textContent = '';
		[
			kv('分辨率', (c.dpi || '-') + ' dpi'),
			kv('队列', c.queue || '-'),
			kv('打印机', c.printer_uri || '-'),
			kv('推送', c.notify ? ((c.notify_kind || 'auto') + ' · ' + (c.notify_events || []).join(',')) : '未配置'),
			kv('离线保护', c.offline_guard ? '开启' : '关闭'),
			kv('自动恢复', c.auto_recover ? '开启' : '关闭'),
			kv('运行', s.uptime_text || '-'),
			kv('内存', (s.mem_avail_mb || 0) + ' / ' + (s.mem_total_mb || 0) + ' MB 可用'),
			kv('负载', s.load1 !== undefined ? s.load1 : '-'),
			kv('磁盘', (s.disk_free_mb || 0) + ' MB 可用')
		].forEach(function(node) {
			this.summaryBox.appendChild(node);
		}, this);
	},

	renderLogs: function(logs) {
		var open = {};
		this.logsBox.querySelectorAll('details').forEach(function(node) {
			open[node.getAttribute('data-k')] = node.open;
		});
		this.logsBox.textContent = '';
		[
			[ 'proxy', '代理日志' ],
			[ 'backend', '发送日志' ],
			[ 'urf', 'URF 捕获' ],
			[ 'cups', 'CUPS 错误日志' ]
		].forEach(function(item) {
			var det = E('details', { 'data-k': item[0] }, [
				E('summary', {}, item[1]),
				E('pre', { 'class': 'ap-log' }, logs[item[0]] || '（空）')
			]);
			if (open[item[0]])
				det.open = true;
			this.logsBox.appendChild(det);
		}, this);
	},

	refresh: function() {
		var self = this;
		if (enabledValue() === '0') {
			this.enabled = false;
			this.renderOffline(null);
			return Promise.resolve();
		}
		this.enabled = true;
		return fetchStatus().then(function(data) {
			self.renderStatus(data);
		}).catch(function(err) {
			self.renderOffline(err.message);
		});
	},

	handleRefresh: function() {
		return this.refresh().then(function() {
			ui.addNotification(null, E('p', {}, '状态已刷新'), 'info');
		});
	},

	handleCheck: function(ev) {
		var button = ev.currentTarget;
		button.disabled = true;
		return fs.exec('/usr/sbin/airprint-setup', [ 'check' ]).then(function(res) {
			var text = (res.stdout || '') + (res.stderr ? ('\n' + res.stderr) : '');
			ui.showModal('AirPrint 自检', [
				E('pre', { 'style': 'white-space:pre-wrap;max-height:60vh;overflow:auto' }, text || '无输出'),
				E('div', { 'class': 'right' }, E('button', { 'class': 'btn', 'click': ui.hideModal }, '关闭'))
			]);
		}).catch(function(err) {
			ui.addNotification(null, E('p', {}, '自检失败：' + err.message), 'error');
		}).finally(function() {
			button.disabled = false;
		});
	},

	handleClearJobs: function(ev) {
		var self = this;
		var button = ev.currentTarget;
		return ui.showModal('清空打印队列', [
			E('p', {}, '将取消队列中所有打印任务（包括正在打印的页面）。确定继续？'),
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'btn', 'click': ui.hideModal }, '取消'),
				E('button', { 'class': 'cbi-button cbi-button-reset', 'click': function() {
					ui.hideModal();
					button.disabled = true;
					return fs.exec('/usr/sbin/airprint-setup', [ 'cancel' ]).then(function(res) {
						var text = ((res.stdout || '') + (res.stderr || '')).trim();
						if (res.code !== 0)
							throw new Error(text || '取消失败');
						ui.addNotification(null, E('p', {}, text || '队列已清空'), 'info');
						setTimeout(function() { self.refresh(); }, 800);
					}).catch(function(err) {
						ui.addNotification(null, E('p', {}, '清空失败：' + (err.message || err)), 'error');
					}).finally(function() {
						button.disabled = false;
					});
				} }, '确定清空')
			])
		]);
	}
});
