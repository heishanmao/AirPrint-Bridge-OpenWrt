# AirPrint Bridge for OpenWrt / ImmortalWrt

在路由器上给**不支持 AirPrint** 的 IPP 打印机补上 AirPrint：iPhone / iPad 直接发现、打印，并能在系统里看到任务状态。桥同时负责能力代理、离线保护、任务历史和推送通知。

本项目在 Xiaomi AX3000T（MT7981，ImmortalWrt 25.12.2）+ EPSON L3250 上开发并长期验证；其他「CUPS 能驱动、打印机支持 PWG Raster」的组合理论上可用，需自备 PPD。

## 为什么需要这一层

- EPSON L3250 只有 IPP/IPPS，没有 URF 文档格式，也没有 AirPrint 广播。
- CUPS 2.3.3 的 `Get-Printer-Attributes` 不返回 `urf-supported`，iOS 会按 600 dpi 发送，数据量是 300 dpi 的 4 倍，小内存路由器容易 OOM。
- CUPS 完成的任务 `job-state-reasons` 仍是 `processing-to-stop-point`，iOS 会一直显示“打印中”。
- 打印机刚唤醒时 TCP 已通、TLS/IPP 还没就绪，直接提交会得到 `Filter failed` 并让队列暂停。

桥在 mDNS 与 CUPS 之间做一层透明 IPP 代理，解决以上问题，同时不改动 CUPS 的打印链路。

## 架构

```
iPhone / iPad
   │  mDNS  _ipp._tcp → 路由器:6310
   ▼
Avahi（airprint-avahi 生成的 airprint.service）
   ▼
airprint-proxy（Go 静态二进制，0.0.0.0:6310）
   │  IPP 透传 · 能力注入 · URI 改写 · 离线拦截 · 状态监控 · 历史 · 推送
   ▼
CUPS（127.0.0.1:631，队列 EPSON_L3250）
   │  image/urf → urftopwg      URF → CUPS Raster（600 → 300 dpi）
   │  raster    → rastertopwg  CUPS Raster → PWG Raster
   ▼
ipps 后端（逐页 PWG，IPPS）
   ▼
EPSON L3250（192.168.31.6:631/ipp/print）
```

## 功能

- **AirPrint 能力代理**：注入 `urf-supported`、`ipp-features-supported`、`job-impressions-supported`、`job-media-sheets-supported`、`printer-mandatory-job-attributes` 等，让 iOS 按配置的 dpi 发送。
- **状态改写**：完成/取消/中止的任务返回标准 `job-state-reasons`。
- **离线保护**：提交前做 TCP + TLS 探测，打印机未就绪时直接返回 IPP `0x0503`，避免队列被 `Filter failed` 停掉。
- **自动恢复**：队列被暂停时可自动 `cupsenable` / `cupsaccept`（带冷却）。
- **任务历史**：JSONL 持久化，条数可配。
- **推送**：Bark / ntfy / 通用 JSON Webhook。
- **LuCI 页面**：`服务 → AirPrint 打印桥`，状态、当前任务、历史、配置和日志。
- **光猫管理入口（可选）**：`服务 → 光猫管理入口`，把上游光猫的网页安全地开放给局域网（不改变 PPPoE 拨号）。
- **URF 抓包**：可选保留最近的 iPhone 原始 URF（用于调试，默认限量，失败不影响打印）。

## 环境要求

- OpenWrt 24.10+ / ImmortalWrt（apk-tools 3.x）
- Release 预编译包架构：`aarch64_cortex-a53`（常见 ARM64 路由器）；其他架构请从源码构建
- CUPS 2.3+（含 `cups-filters` 的 `rastertopwg`）
- `avahi-daemon`、`python3`（辅助脚本与 CUPS 过滤器使用）
- 打印机支持 **IPPS + PWG Raster**；EPSON L3250 / L3150 系列已验证
- 建议 ≥64 MB 可用 flash、≥128 MB 内存；600 dpi 会显著放大瞬时内存占用

## 安装（Release APK）

1. 安装依赖并启用服务：

```sh
apk update
apk add cups cups-filters avahi-daemon python3
/etc/init.d/avahi-daemon enable
/etc/init.d/avahi-daemon start
```

2. 调整 `/etc/cups/cupsd.conf`（关键项）：

```
Listen 127.0.0.1:631
Browsing No
BrowseLocalProtocols none
MaxJobs 10
PreserveJobFiles No
PreserveJobHistory Yes
LogLevel warn
```

`Listen 127.0.0.1:631` 让 CUPS 只对本机开放，避免局域网里出现第二个打印机条目。改完重启 cupsd。

3. 从 [Releases](https://github.com/heishanmao/AirPrint-Bridge-OpenWrt/releases) 下载 APK，上传到路由器后安装：

```sh
apk add --allow-untrusted --no-network --force-non-repository /tmp/airprint-bridge-1.1.0-r12.apk
```

4. 修改 `/etc/config/airprint`（至少改这几项）：

```sh
uci set airprint.main.queue_name='EPSON_L3250'
uci set airprint.main.printer_uri='ipps://192.168.31.6:631/ipp/print'
uci set airprint.main.service_name='EPSON L3250 Series'
uci set airprint.main.service_uuid="$(cat /proc/sys/kernel/random/uuid)"
uci set airprint.main.dpi='300'
uci commit airprint
```

> 仓库里的 `192.168.31.x`、`0ea66e10-…` 是作者环境的默认值，务必替换成你自己的地址和 UUID。

5. 应用配置（注册 MIME、创建/更新 CUPS 队列、生成 mDNS 记录、重启代理）：

```sh
airprint-setup apply
airprint-setup check
```

6. iPhone「添加打印机」里选择 `service_name`，打印时确认纸张和份数即可。

升级 APK 时配置、历史、CUPS 队列和 PPD 都会保留；队列里有未完成任务时安装会被拒绝。

## 光猫管理入口（可选）

如果路由器上游是**桥接**光猫（PPPoE 由路由器拨号），光猫的管理页通常仍在 `192.168.1.1`，只是局域网无法路由到它。这个模块在 WAN 口加一个同网段地址，并用 SNAT 让局域网访问：

- LuCI：`服务 → 光猫管理入口`，可启用/停用、修改光猫地址、子网前缀、本机地址，以及是否通过 DHCP option 121 推送路由。
- 命令行：`ontaccess-setup status | apply | disable`，配置在 `/etc/config/ontaccess`。
- 默认**关闭**；启用时创建 `network.ont`（`wan` 上的静态地址）、`firewall.ont`（`masq`）、`lan → ont` 转发和 DHCP option 121。停用会把这些配置移除。

注意：

- 需要光猫在链路上响应该管理地址；桥接后管理 VLAN 不通的机型不适用。
- 所有局域网设备共用同一个源地址，光猫通常只保留一个管理会话，多台设备同时登录可能互相踢下线。
- 客户端需要重新获取 DHCP 租约才能拿到路由；VPN/代理接管默认路由的设备可能需要手动加一条到光猫网段的路由。
- 只允许局域网访问，不会把光猫暴露到 WAN。

## 配置项

配置文件 `/etc/config/airprint` 的 `config airprint 'main'` 段：

| 选项 | 默认 | 说明 |
|---|---|---|
| `enabled` | `1` | 关闭后停止代理并撤掉 mDNS 记录 |
| `listen_port` | `6310` | 代理监听端口（mDNS 广播的端口） |
| `cups_host` / `cups_port` | `127.0.0.1` / `631` | 上游 CUPS |
| `queue_name` | `EPSON_L3250` | CUPS 队列名 |
| `printer_uri` | `ipps://192.168.31.6:631/ipp/print` | 打印机地址，`ipps` 或 `ipp` |
| `service_name` | `EPSON L3250 Series` | iOS 里显示的打印机名 |
| `service_uuid` | `0ea66e10-…` | mDNS UUID，需与 CUPS `printer-uuid` 一致，避免 iOS 出现重复条目 |
| `dpi` | `300` | `300` 或 `600`，通过 `urf-supported` 协商 |
| `pdl` | `image/urf,application/pdf,image/jpeg,image/png` | mDNS 广播的文档格式 |
| `note` | 空 | mDNS 备注 |
| `airprint_features` | `subscription-object,airprint-1.8…1.3` | 注入的 `ipp-features-supported` |
| `offline_guard` | `1` | 打印机 TCP/TLS 未就绪时拦截任务 |
| `auto_recover` | `1` | 队列暂停时自动恢复 |
| `notify_url` | 空 | 推送地址，留空关闭 |
| `notify_kind` | `auto` | `auto` / `bark` / `ntfy` / `generic` |
| `notify_events` | `start,complete,error,recover` | 推送事件 |
| `history_file` | `/var/log/airprint/jobs.jsonl` | 任务历史 |
| `history_max` | `200` | 历史条数上限 |
| `debug` | `0` | 代理调试日志 |

修改后执行 `airprint-setup apply`。

## 从源码构建

依赖：Go 1.24+、[zig](https://ziglang.org/)（编译 URF 过滤器）、apk-tools 3.x（含 `mkpkg`，可用 `apk.static`）。

```sh
git clone https://github.com/heishanmao/AirPrint-Bridge-OpenWrt.git
cd AirPrint-Bridge-OpenWrt

# 1) URF → CUPS Raster 过滤器（输出 filter/urftopwg.real）
sh filter/build.sh                      # 默认 aarch64-linux-musl

# 2) 打包 APK（会顺带交叉编译 Go 代理）
APK_STATIC=/path/to/apk.static VERSION=1.1.0-r12 sh package/build-apk.sh
```

产物：`package/build/airprint-bridge-<VERSION>.apk`。

- 换架构：`ARCH=arm_cortex-a7 GOARCH=arm ZIG_TARGET=arm-linux-musleabihf`（按目标平台填写 `apk` 的 arch 名）。
- `package/build-apk.sh` 接受 `APK=` 覆盖 `apk` 命令；宿主机自带 apk 3.x 且支持 `mkpkg` 时可直接用。
- Go 代理可单独构建：`cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o airprint-proxy .`

## 状态与排障

```sh
airprint-status                 # 链路状态 + 最近任务
airprint-status --logs          # 附带日志尾部
airprint-setup check            # 依赖 / 打印机 / CUPS / mDNS 自检
wget -qO- http://127.0.0.1:6310/status.json
wget -qO- http://127.0.0.1:6310/healthz
```

LuCI：`http://<路由器>/cgi-bin/luci/admin/services/airprint`。

日志与运行态：

| 路径 | 内容 |
|---|---|
| `/var/log/airprint/proxy.log` | 代理日志（自动轮转） |
| `/var/log/airprint/jobs.jsonl` | 任务历史 |
| `/tmp/airprint/backend.json` | 后端发送状态（sidecar） |
| `/tmp/airprint/backend.log` | 后端逐页发送日志 |
| `/tmp/airprint/urf/` | URF 样本（限量） |
| `/var/log/cups/error_log` | CUPS / 过滤器 |

常见问题：

- **队列 paused / printer disabled**：打印机离线或任务失败所致。恢复打印机后执行 `cupsenable EPSON_L3250 && cupsaccept EPSON_L3250`；开启 `auto_recover` 后代理会自动处理。
- **状态页显示「TCP 可达但 IPP 未就绪」**：打印机刚唤醒，TLS 栈还没起来；等十几次探测或重新上电。
- **iOS 出现两个打印机条目**：一个是 iOS 保存的 Known Printer，一个是实时 mDNS 记录。把 `service_uuid` 设成 CUPS 的 `printer-uuid` 可合并；iPhone 没有删除已保存打印机的入口。
- **iOS 27 看不到 Print Center / 灵动岛打印入口**：这是 iOS 自身行为，任务仍会正常打印，可在 LuCI 页面查看进度。
- **安装提示队列有任务**：先 `cancel -a EPSON_L3250` 再升级。

## 目录结构

```
go/                 Go 代理源码（stdlib，无第三方依赖）
filter/urftopwg.c   URF → CUPS Raster 过滤器（v13）
package/root/       APK 安装内容（init、代理辅助脚本、CUPS 后端/过滤器、PPD、LuCI 页面、光猫入口）
package/scripts/    APK 安装/升级脚本
package/build-apk.sh
```

`package/root/usr/lib/airprint/airprint-proxy.py` 是旧版 Python 代理，仅作参考/回滚；默认运行的是 Go 二进制。

## 已知限制

- PPD 针对 EPSON L3250 系列；其他机型需替换 `package/root/usr/share/airprint/EPSON_L3250.ppd`，并确认打印机接受 PWG Raster。
- 600 dpi 的 URF 单页数据约为 300 dpi 的 4 倍，内存紧张的路由器不建议开启。
- 代理与 CUPS 同机部署；CUPS 默认只监听 loopback。
- iOS 27 不提供 Print Center 入口，任务状态以 LuCI 页面为准。
- 光猫管理入口要求光猫在物理链路上响应管理地址；桥接后管理 VLAN 不通的机型无法使用。

## 许可证

- 本项目代码：MIT，见 [LICENSE](LICENSE)。
- `package/root/usr/share/airprint/EPSON_L3250.ppd`：来自 Seiko Epson ESC/P-R Linux 驱动，GPL-2.0，详见 [NOTICE](NOTICE)。
- Apple、AirPrint 是 Apple Inc. 的商标；Epson 是 Seiko Epson Corporation 的商标。本项目与两家公司无关联。
