'use strict';
'require view';
'require fs';
'require poll';
'require ui';

function runStatus() {
	return fs.exec('/usr/sbin/ontaccess-setup', [ 'status' ]).then(function(res) {
		var text = ((res && res.stdout) || '').trim();
		var data;
		try {
			data = JSON.parse(text || '{}');
		} catch (e) {
			throw new Error(text || (res && res.stderr) || '状态解析失败');
		}
		return data;
	});
}

function pill(label, state, detail) {
	var cls = state === 'ok' ? 'ok' : (state === 'warn' ? 'warn' : 'bad');
	return E('div', { 'class': 'oa-pill ' + cls }, [
		E('span', { 'class': 'oa-dot' }),
		E('span', { 'class': 'oa-label' }, label),
		E('span', { 'class': 'oa-detail' }, detail || (state === 'ok' ? '正常' : '未生效'))
	]);
}

return view.extend({
	load: function() {
		return runStatus().catch(function(err) {
			return { load_error: err.message };
		});
	},

	render: function(data) {
		var self = this;
		data = data || {};
		this.data = data;

		this.badge = E('span', { 'class': 'oa-badge idle' }, '读取中');
		this.notice = E('div', { 'class': 'oa-notice' });
		this.statusBox = E('div', { 'class': 'oa-status' });

		this.enabledInput = E('input', { 'type': 'checkbox', 'class': 'cbi-input-checkbox',
			'checked': data.enabled ? true : null });
		this.ontInput = E('input', { 'type': 'text', 'class': 'cbi-input-text',
			'value': data.ont_ip || '192.168.1.1' });
		this.prefixInput = E('input', { 'type': 'number', 'class': 'cbi-input-text',
			'min': '8', 'max': '30', 'value': data.prefix || '24' });
		this.localInput = E('input', { 'type': 'text', 'class': 'cbi-input-text',
			'value': data.local_ip || '192.168.1.2' });
		this.routeInput = E('input', { 'type': 'checkbox', 'class': 'cbi-input-checkbox',
			'checked': data.dhcp_route !== false ? true : null });

		var page = E('div', { 'class': 'oa-page' }, [
			E('style', {}, [
				'.oa-page{max-width:900px}',
				'.oa-head{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin:10px 0 16px}',
				'.oa-head .oa-spacer{flex:1}',
				'.oa-badge{display:inline-flex;align-items:center;padding:4px 11px;border-radius:999px;font-size:12px;font-weight:600;border:1px solid rgba(127,127,127,.3)}',
				'.oa-badge.ok{color:#16a34a;background:rgba(22,163,74,.1)}',
				'.oa-badge.bad{color:#dc2626;background:rgba(220,38,38,.1)}',
				'.oa-badge.idle{color:#64748b;background:rgba(100,116,139,.1)}',
				'.oa-notice:empty{display:none}',
				'.oa-notice{padding:8px 10px;border-radius:6px;background:rgba(220,38,38,.08);color:#dc2626;font-size:12.5px;margin-bottom:10px;line-height:1.5}',
				'.oa-notice.info{background:rgba(100,116,139,.1);color:#475569}',
				'.oa-status{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:8px}',
				'.oa-pill{display:flex;align-items:center;gap:7px;background:rgba(127,127,127,.06);border:1px solid rgba(127,127,127,.18);border-left:3px solid #64748b;border-radius:7px;padding:8px 10px;font-size:12.5px}',
				'.oa-pill.ok{border-left-color:#16a34a}.oa-pill.warn{border-left-color:#d97706}.oa-pill.bad{border-left-color:#dc2626}',
				'.oa-dot{width:7px;height:7px;border-radius:50%;background:#64748b;flex:none}',
				'.oa-pill.ok .oa-dot{background:#16a34a}.oa-pill.warn .oa-dot{background:#d97706}.oa-pill.bad .oa-dot{background:#dc2626}',
				'.oa-label{font-weight:600;white-space:nowrap}',
				'.oa-detail{opacity:.72;margin-left:auto;text-align:right;overflow-wrap:anywhere;min-width:0;font-size:11.5px}',
				'.oa-note{font-size:12px;line-height:1.6;opacity:.75;margin-top:10px}',
				'.oa-check{display:flex;align-items:center;gap:6px}'
			].join('\n')),

			E('h2', {}, '光猫管理入口'),
			E('p', { 'class': 'cbi-section-descr' },
				'在 WAN 口增加一个管理地址，通过 SNAT 让局域网电脑直接打开上游光猫的网页。PPPoE 拨号方式不变。'),

			E('div', { 'class': 'oa-head' }, [
				this.badge,
				E('span', { 'class': 'oa-spacer' }),
				E('button', { 'class': 'cbi-button cbi-button-neutral',
					'click': ui.createHandlerFn(this, 'handleRefresh') }, '重新检测')
			]),

			E('div', { 'class': 'cbi-section' }, [
				E('h3', {}, '状态'),
				this.notice,
				this.statusBox
			]),

			E('div', { 'class': 'cbi-section' }, [
				E('h3', {}, '设置'),
				E('div', { 'class': 'cbi-value' }, [
					E('label', { 'class': 'cbi-value-title' }, '启用'),
					E('div', { 'class': 'cbi-value-field' }, [
						E('label', { 'class': 'oa-check' }, [
							this.enabledInput,
							E('span', {}, '允许局域网访问光猫管理页')
						])
					])
				]),
				E('div', { 'class': 'cbi-value' }, [
					E('label', { 'class': 'cbi-value-title' }, '光猫地址'),
					E('div', { 'class': 'cbi-value-field' }, [
						this.ontInput,
						E('div', { 'class': 'cbi-value-description' }, '光猫的管理 IP，通常是 192.168.1.1')
					])
				]),
				E('div', { 'class': 'cbi-value' }, [
					E('label', { 'class': 'cbi-value-title' }, '子网前缀'),
					E('div', { 'class': 'cbi-value-field' }, [
						this.prefixInput,
						E('div', { 'class': 'cbi-value-description' }, '光猫管理网段的掩码长度，常见为 24')
					])
				]),
				E('div', { 'class': 'cbi-value' }, [
					E('label', { 'class': 'cbi-value-title' }, '本机地址'),
					E('div', { 'class': 'cbi-value-field' }, [
						this.localInput,
						E('div', { 'class': 'cbi-value-description' }, '路由器在光猫网段使用的地址，需与光猫同网段')
					])
				]),
				E('div', { 'class': 'cbi-value' }, [
					E('label', { 'class': 'cbi-value-title' }, '推送路由'),
					E('div', { 'class': 'cbi-value-field' }, [
						E('label', { 'class': 'oa-check' }, [
							this.routeInput,
							E('span', {}, '通过 DHCP option 121 告知客户端如何访问光猫网段')
						]),
						E('div', { 'class': 'cbi-value-description' }, '关闭后需要客户端手动添加路由')
					])
				]),
				E('div', { 'class': 'cbi-page-actions' }, [
					E('button', { 'class': 'cbi-button cbi-button-apply',
						'click': ui.createHandlerFn(this, 'handleApply') }, '保存并应用')
				]),
				E('div', { 'class': 'oa-note' }, [
					E('p', {}, '所有局域网设备访问光猫时都使用同一个源地址，光猫通常只保留一个管理会话，多台设备同时登录可能互相踢下线。'),
					E('p', {}, '修改后客户端需要重新获取 DHCP 租约；使用 VPN/代理接管默认路由的设备可能需要额外添加一条到光猫网段的路由。')
				])
			])
		]);

		if (data.load_error)
			this.renderStatus({ error: data.load_error, enabled: false });
		else
			this.renderStatus(data);

		poll.add(function() {
			return self.refresh();
		}, 10);

		return page;
	},

	setFields: function(data) {
		if (data.ont_ip)
			this.ontInput.value = data.ont_ip;
		if (data.prefix)
			this.prefixInput.value = data.prefix;
		if (data.local_ip)
			this.localInput.value = data.local_ip;
		this.enabledInput.checked = !!data.enabled;
		this.routeInput.checked = data.dhcp_route !== false;
	},

	renderStatus: function(data) {
		data = data || {};
		this.badge.textContent = data.enabled ? '已启用' : '未启用';
		this.badge.className = 'oa-badge ' + (data.enabled ? 'ok' : 'idle');
		this.notice.textContent = '';
		this.notice.className = 'oa-notice';

		if (data.error) {
			this.notice.className = 'oa-notice';
			this.notice.textContent = data.error;
		}
		else if (!data.enabled) {
			this.notice.className = 'oa-notice info';
			this.notice.textContent = '未启用：保存并应用后会创建管理接口、防火墙规则和 DHCP 路由。';
		}

		this.statusBox.textContent = '';
		[
			pill('管理接口', data.local_present ? 'ok' : (data.enabled ? 'bad' : 'warn'),
				data.local_present ? ((data.local_ip + '/' + data.prefix) + ' 已生效') : '未添加'),
			pill('光猫连通', data.reachable ? 'ok' : (data.enabled ? 'bad' : 'warn'),
				data.reachable ? (data.ms + ' ms') : '不可达'),
			pill('管理页面', data.http ? 'ok' : (data.enabled ? 'bad' : 'warn'),
				data.http ? 'HTTP 正常' : '未响应'),
			pill('NAT / 转发', (data.masq && data.forwarding) ? 'ok' : (data.enabled ? 'bad' : 'warn'),
				(data.masq && data.forwarding) ? '已配置' : '未配置'),
			pill('局域网路由', data.route_pushed ? 'ok' : (data.dhcp_route ? (data.enabled ? 'bad' : 'warn') : 'warn'),
				data.route_pushed ? ((data.network || '') + ' 经 ' + (data.lan_ip || 'LAN')) : (data.dhcp_route ? '未下发' : '已关闭'))
		].forEach(function(node) {
			this.statusBox.appendChild(node);
		}, this);
	},

	refresh: function() {
		var self = this;
		return runStatus().then(function(data) {
			self.renderStatus(data);
		}).catch(function(err) {
			self.renderStatus({ error: err.message, enabled: false });
		});
	},

	handleRefresh: function() {
		return this.refresh().then(function() {
			ui.addNotification(null, E('p', {}, '状态已刷新'), 'info');
		});
	},

	handleApply: function(ev) {
		var self = this;
		var button = ev.currentTarget;
		var enabled = this.enabledInput.checked ? '1' : '0';
		var ontIp = (this.ontInput.value || '').trim();
		var prefix = (this.prefixInput.value || '').trim();
		var localIp = (this.localInput.value || '').trim();
		var route = this.routeInput.checked ? '1' : '0';

		button.disabled = true;
		var oldText = button.textContent;
		button.textContent = '应用中…';

		return fs.exec('/usr/sbin/ontaccess-setup',
			[ 'set', enabled, ontIp, prefix, localIp, route ]).then(function(res) {
			var text = ((res && res.stdout) || '').trim();
			var out;
			try {
				out = JSON.parse(text || '{}');
			} catch (e) {
				out = { ok: false, error: text || (res && res.stderr) || '应用失败' };
			}
			if (res && res.code !== 0 && !out.error)
				out.error = (res.stderr || '').trim() || '应用失败';
			if (out.ok === false)
				throw new Error(out.error || '应用失败');
			self.renderStatus(out);
			self.setFields(out);
			ui.addNotification(null, E('p', {}, out.enabled ? '光猫管理入口已启用' : '光猫管理入口已关闭'), 'info');
		}).catch(function(err) {
			ui.addNotification(null, E('p', {}, '应用失败：' + (err.message || err)), 'error');
		}).finally(function() {
			button.disabled = false;
			button.textContent = oldText;
		});
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
