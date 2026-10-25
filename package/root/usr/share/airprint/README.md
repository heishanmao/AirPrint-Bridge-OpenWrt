# AirPrint Bridge

iPhone AirPrint → CUPS → EPSON L3250，带能力代理、任务历史、离线保护、自动恢复和推送。

## 地址

- LuCI：`服务 → AirPrint 打印桥`（状态 + 配置）
- 状态 JSON：`http://192.168.31.1:6310/status.json`

## 常用命令

```sh
airprint-status            # 查看链路状态和历史
airprint-status --logs     # 附带日志
airprint-setup check       # 检查依赖、打印机、CUPS、mDNS
airprint-setup apply       # 修改 UCI 后应用（Avahi + CUPS + 重启代理）
```

## 配置

配置文件 `/etc/config/airprint`。升级不会覆盖它。

```sh
uci set airprint.main.dpi='600'                 # 300 或 600
uci set airprint.main.printer_uri='ipps://192.168.31.6:631/ipp/print'
uci set airprint.main.offline_guard='1'         # 打印机离线时拦截任务
uci set airprint.main.auto_recover='1'          # 队列停止后自动恢复
uci set airprint.main.notify_url='https://api.day.app/你的KEY'
uci set airprint.main.notify_events='start,complete,error,recover'
uci commit airprint
airprint-setup apply
```

`notify_url` 留空即关闭推送。`notify_kind` 支持 `auto`（Bark/ntfy 自动识别）、`bark`、`ntfy`、`generic`。

600 dpi 单页数据约为 300 dpi 的 4 倍，内存和 `/overlay` 空间紧张时不建议使用。

## 升级

```sh
apk add --allow-untrusted --force-non-repository /tmp/airprint-bridge-<版本>.apk
```

升级前如队列里有任务会拒绝安装。配置、历史、CUPS 队列、PPD 都会保留。

## 文件

- `/usr/sbin/airprint-proxy` 代理、监控、状态接口（Go 静态二进制，Python 版保留在 `/usr/lib/airprint/airprint-proxy.py`）
- `/usr/lib/cups/backend/ipps` 逐页 PWG 发送到打印机
- `/usr/lib/cups/filter/urftopwg` URF 捕获包装器
- `/var/log/airprint/jobs.jsonl` 任务历史
- `/var/log/airprint/proxy.log` 代理日志
- `/tmp/airprint/` 运行时日志、URF 样本、发送状态
