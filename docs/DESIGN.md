# orbit 设计文档(基线)

项目代号 **orbit(轨道)**。面向公网陌生用户的私有网络产品。
本文件为 M1 起点的设计基线, 后续实现以本文件为纲, 变更需在此更新。

## 0. 与 ssvpn 的关系

orbit 是 ssvpn 的**独立继任项目**, 位于 `/root/project/orbit`, 与 `ssvpn` 同级。
- **不修改/不依赖 ssvpn 代码**(`github.com/...` 无相互 import)。
- 帧格式、数据面思路同源(见 §4), 但按"公网用户产品"的需求重新分层:
  ssvpn 偏"内网自用 + P2P 探索", orbit 偏"控制面/账户/计费先行"。
- 一段过渡期内 ssvpn 保持在线(供对照); 产品化能力全部在 orbit 演进。

## 1. 目标与产品形态

- 目标用户: **公网陌生用户**(免运维的注册即用)。
- 出口节点形态: **任意装了客户端的终端都可以开放为出口**(众包出口)。
- 部署形态: M1 单机自建漏斗, 设计上预留多区域/多账户隔离。
- 商业模式: 先免费内测; 服务器中枢精确计量, 预留收费档位。

三种客户端模式(共用同一引擎, 只是配置组合):

| 模式 | 行为 | 现有引擎要素 |
|---|---|---|
| `simple` 简单 | 仅虚拟网卡, 与同网络设备互通 | TUN + VLAN 中继 + 虚拟 IP |
| `smart` 智能 | 按域名/IP 走指定出口; 可开放本机为出口 | fake-IP 分流 + 规则表 + IP-in-IP 出口 + SO_MARK |
| `global` 全局 | 全部流量经出口节点 | `0.0.0.0/0 via exit` + keep-local 排除 + 全量 DNS 接管 |

## 2. 信任模型与安全(公网产品的红线)

公网注册 + 众包出口, 必须从前端就明确信任边界:

1. **设备凭证优先**: 客户端用 `device_id + token` 登录(替代裸密码), token 可撤销。
   M1 兼容 Password 模式仅作迁移/自托管小规模用。
2. **账户隔离**: 每个账户独立虚拟网络(`resolver=account`), 陌生用户互不可见
   ("简单模式与其他客户端互通" = **显式加好友/入群**后的行为, 默认不互通)。
3. **出口授权**: 设备需要 `egress_enabled` 开关且配置 `egress_acl`(允许消费我的账号列表)
   才可作为出口; 本机流量可直接用本人设备, 其余一律查 ACL。
4. **出口透明性声明**: 出口节点负责拨号到目标地址, 对目的地**可见明文**。产品内测页
   必须声明"使用他人出口 = 信任该设备", 并给出口者"仅限信任对象"的授权手段。
5. **滥用防护**: 登录/注册失败限流(防爆破); 公网出口 **默认只给受限配额**;
   内测用邀请码(白名单)开放注册。
6. **并发与配额**: `QuotaEnforcer` 每次会话/每次使用出口前校验; 超限即停该出口。

## 3. 控制面 / 数据面划分(为多区域预留)

- **控制面(Control Plane)**: 账户、设备、租约、配额、计量、出口 ACL —— 全局唯一,
  未来多区域时仍单点或加主从复制, **不与转发平面耦连**。
- **数据面(Data Plane)**: 一条客户端隧道 = 中继转发。未来加区域 = 加一个 orbitd
  数据面实例(独立监听、独立网络段), 通过配置指向同一控制面存储。
- 目录划分: `internal/account`(控制面模型/存储)、`internal/server`(orbitd: 控制面 API +
  待落地的数据面)、`internal/transport`(传输抽象: Relay / Direct)。

> 现骨架中 `Config.Networks` 的 key 即"网络(组)ID"; 预留扩展为 `region:network` 两级。

## 4. 协议(与 ssvpn 同源, 独立演进)

帧: `[type:1][len:2][payload]`; `FrameData`(原始 IP 包)、`FrameCtrl`(json)、
`FramePing/Pong`。`MaxPayload = 16MB`。

控制消息(`internal/protocol/control.go` 已含类型定义):
- `hello`(user/device_id/auth + hostname/os/egress/mode)
- `welcome`(virtual_ip/cidr/network_id/mtu/peers + session)
- `peer_join` / `peer_leave`(含 `Egress` 标记 → 客户端可视化"可用出口")
- `session` / `quota`(服务器下发档位与配额状态; 客户端据此提示/暂停)
- `reject`(原因)

扩展方向(向后兼容): M1 增加设备 token 握手; 未来增加 出口授权协商、租户/区域字段。

## 5. 数据模型(控制面存储)

| 模型 | 字段要点 | 落盘 |
|---|---|---|
| `Account` | id(预留区域前缀)/ name / tier(free|pro...) | accounts.json |
| `Device` | id / account_id / token_hash(bcrypt) / egress_enabled / egress_acl | 同上 |
| `IPLease` | network_id / owner_account / owner_device / ipv4(重启不漂移) | leases.json |
| `Usage` | key=(account, device, egress, 小时桶) → rx/tx | usage.json |

计量口径: 所有流量过 orbitd(星型中继) → 服务器计数即唯一事实来源,
免费内测配额与未来计费都只读 `Usage` 汇总, 无需客户端上报。

## 6. 出口链路(smart/global 共用)

`socket -> TUN -> (IP-in-IP 封装) -> orbitd -> 出口设备(解封装) -> gVisor netstack TCP/UDP Forwarder -> 目标`
- Linux 出口侧: SO_MARK + 策略路由表(避免向隧道回环), 与 ssvpn 的
  `ssvpn-egress-routes.service` 思路一致(orbit 独立实现)。
- 全局模式必须补: **UDP/ICMP 转发**(netstack 只 TCP 会丢弃 QUIC/ping)、
  keep-local 排除(服务器地址/虚拟网段/TUN 直连段)、全量 DNS 接管与 IPv6 泄漏抑制。
- M3: `transport.Transport` 接入 P2P 直连(打洞失败回退中继), 出口带宽不再依赖单机。

## 7. M1 范围(本次骨架之后的第一轮实现)

优先级 P0(雏形上线):
1. 账户/设备模型落库 + 邀请码注册 + 登录失败限流 + bcrypt token
2. 设备 token 握手(`hello.auth.mode=device`), 会话下发 `Session/Quota`
3. IP 租约持久化(`allocIP` → `LeaseStore.Allocate`)并绑定设备
4. 数据面: RelayTransport 中继 + TUN(Windows/Linux) + welcome/peer_join 广播
5. 出口: 设备 `egress_enabled` 开关 + `Egress` 字段下发给 peers; 出口流量按 ACL 放行
6. 计量: 握手通过后所有转发帧进 `UsageStore.Add`; 管理 API 暴露 用户/设备/用量
7. 模式预设: `mode` 配置展开 → 引擎开关(先 simple + smart; global 待 UDP/ICMP)

P1(内测体验): UDP/ICMP 出口、global 完整(keep-local/DNS)、免费档配额硬限、多设备管理、GUI。

配额硬限实现(2026-09 真机验证): `QuotaEnforcer`(internal/server/quota.go)在 Relay 写转发
前 `admit`(账户日流量 + 消费者出口日流量双泳道), 超限返回 `errQuotaBlocked`(拒收帧且数据面
静默不刷日志), 每连接一次性向被拦方下发 `MsgQuota` 控制帧; `UsageStore` 定期落盘(60s) +
退出时 flush, 重启后按当日已有用量回填续算。

## 8. 预留扩展点(现在留位, 不现在做)

- **多区域**: `Networks` key 升级 `region:network`; 数据面实例化 + 控制面统一存储。
- **多账户隔离**: 天然继承(§5 account 独立网络); 群组=控制面显式邀请。
- **计费**: `QuotaEnforcer` 接口 + `Usage` 聚合 → 换档位只加实现。
- **P2P**: `transport.Transport` 接口, 加 `Direct() Transport` 实现即可。

## 9. 里程碑

- **M1(雏形)**: §7 全部 P0 → 邀请码内测。
- **M2**: UDP/ICMP + global 完整 + 免费档硬限 + 多设备管理。
  - 2026-09: UDP/ICMP ✅、global(含 Windows 默认路由接管)✅、免费档配额硬限 ✅(真机验证)、多设备管理自助化 ✅。
  - 设备自助管理 API(设备 token 鉴权, X-Orbit-Device/X-Orbit-Token):
    `GET /api/v1/devices`(本账户设备列表+在线态)、`POST /api/v1/devices`(老 token 加装新机)、
    `POST /api/v1/devices/rename`、`POST /api/v1/devices/revoke`(含自己,踢下线+权限即刻失效)、
    `POST /api/v1/devices/rotate-token`(旧 token 即刻失效,新明文仅一次)、`GET /api/v1/usage`(当日用量+配额)。
    越权防护: 目标设备须与调用者同账户, 跨账户一律 401。CLI 侧 `orbit-cli devices <list|usage|rename|revoke|token>`。
    nginx 侧 /orbit/api/v1/ 三段 location(register/usage/devices)反代到 orbitd admin。
  - 客户端可靠性加固(重连实战踩坑): ① Run 断线返回 nil 被主循环当正常退出→重连成死代码;
    ② 第二次 Run close(readyCh) panic; ③ wintun 会话 End 与 ReceivePacket/AllocateSendPacket
    并发访问冲突(0xc0000005)→sessionMu 串行化; ④ SIGTERM 后 os.Exit 早退导致 tun fd 泄漏、
    接口滞留 TUNSETIFF 永远 EBUSY→改优雅退出; ⑤ 主循环 recover 记栈到滚动日志继续退避重连。
- **M3**: P2P 直连、多区域骨架、计费档位插 `QuotaEnforcer`。
  - 2026-09 已落地: 计费档位 ✅。`tiers` 配置表(tier名 -> 当日配额), `Config.QuotaFor(tier)`
    与 `Hub.quotaSpecFor(accountID)` 统一解析, `quotaTrk` 按账户档位逐包裁决;
    welcome/Session 与 MsgQuota 通知限额按账户档位下发; 注册取 `default_tier`;
    `POST /api/v1/admin/accounts/tier {account_id,tier}` 切换后立即向该账户在线节点推新配额(无需重连)。
    管理账户列表返回 per-account `limit_bytes/egress_limit_bytes`。
    修复用量持久化 bug: `map[Bucket]` 结构体键被 json 新后端拒绝(对象成员名须为字符串)导致
    usage.json 恒为 `{}`、重启后配额回填失真 → 改字符串键 `account|device|egress|hour` 落盘, 新增
    `TestUsageRoundtrip` 回归。(对账: 配额 filesave 60s flush + 退出 flush 现均正常)
    门户管理页(agent 仓内 portal)✅: 登录态 `/orbit` 设备管理页, proxy 到 orbitd admin
    (accounts/devices/clients/invites/usage 只读 + 档位切换/加设备/吊销/出口授权/邀请码管理),
    `X-Admin-Token` 由 portal 持有, 公网不暴露 orbitd admin。
  - 待续: P2P 直连(`transport.Direct()`)、多区域(数据面实例化 + 控制面共享); GUI 客户端已交付(见 §12)。

## 10. 自包含部署与双入口(2026-09 开放内核化)

**产品形态转变**: orbit 从「私有 SaaS 内部件」演进为「单二进制自托管产品」(开源 AGPL-3.0 内核 + 私有托管层)。
orbitd 一个二进制即含协调器/数据面/管理面, 任何人在自家服务器一键拉起自成 mesh 服务端。

**双监听入口**:

- **数据面 `listen_addr`**(默认 0.0.0.0:28443): 老协议不变, 客户端 `server_addr` 接入。
- **公共 HTTPS `public.addr`**(默认关, 开则 0.0.0.0:4431): 产品化自助入口, 只做非敏感操作:
  - `GET /health`、`GET /ca.pem`(发放本服务器私钥自签的 CA, 证书链与数据面同根);
  - `POST /api/v1/register`(邀请码开户, 返回 account/device/token);
  - `GET /`(落地页)。**不暴露** admin/设备/用量等敏感面。
- **admin `admin.addr`**(默认 127.0.0.1:18444): 内嵌 Web UI + admin API, 仅本机可
  达(`X-Admin-Token`)。自部署模式下经 `ssh -L` 隧道访问, 不直接挂公网。

证书: `orbitd genconfig` 用纯 Go 生成 ECDSA P-256 自签 CA + 服务端证书(SAN=主机IP/域名),
写入 `--dir`(默认 /opt/orbit): `certs/ca.pem|ca-key.pem|server-cert.pem|server-key.pem` + `orbitd.yaml`。
换公网可信证书时: 改 `tls:`+`public.ca_file` 路径即可(客户端注册时自动下载新 CA)。

**客户端注册参数化**(`orbit-cli register -server <host:port> -web <host:4431>`):
- `-server` 填数据面 `server_addr`; `-web` 指向公共 HTTPS 入口;
- 注册时先从 `<web>/ca.pem` 现场下载服务器 CA 存 `orbitd-cert.pem`, 免去打包预置证书;
- yaml 写 `self_api_base=<web>/api/v1`, 供 `orbit-cli devices*` 自助查询。

**运维/发布形态**: `deploy/install-orbitd.sh` 一键(root)装 systemd 服务 + 生成邀请码;
GitHub Actions `ci`(gofmt/vet/build/test 双平台)+ `release`(tag v* 出品 orbitd/orbit-cli 多平台资产)。
Windows 客户端的 wintun.dll 属 WireGuard LLC **Prebuilt Binaries 专有许可**, 不进入开源库,
由维护者在发布机装配(见 `deps/wintun-NOTICE.md`)。

## 11. 里程碑(2026-09 M4: 开放内核)

- **M4(开放内核)**: 双入口+自包含部署 ✅(genconfig/内嵌UI/公共注册/CA发放, 生产真机验证)。
  剩余: P2P 直连、多区域骨架、压测基线、README/文档形成对外口径。

## 12. Windows GUI 客户端（2026-09-25 交付, P0+P1 全量）

- **形态**: 原生 Go 托盘常驻 + walk 窗口(面板)。面向 Windows 桌面用户的管理壳,
  覆盖: 模式切换(simple/smart/global)、出口节点选择、egress/keep-local 编辑、开机自启
  与每小时自愈(计划任务)开关、**GUI 自身"登录时启动面板"开关(HKCU Run `OrbitGUI` 值,
  无需提权, 面板+托盘双入口)**、连接/网卡/设备状态、当日用量、日志末尾、邀请码注册向导。
- **底层分层**: GUI 只做壳, 一切管理能力走 `orbit-cli` 子命令层:
  - `status`(连接/模式/出口/自启/日志/设备/用量汇总 JSON, 供 GUI 直接渲染);
  - `set`(mode/egress/exit/keep-local 落盘 + 重启守护进程, 幂等);
  - `autostart`(查询/开关 OrbitClient 主任务 与 OrbitClientRelink 每小时自愈);
  - `usage`(当日用量+配额)。
  - 读操作由 GUI 直接子进程调用并解析; 写操作统一经 `ShellExecute("runas")` 提权调 CLI
    (改 ProgramData 配置/重启 SYSTEM 守护/建删计划任务均需管理员)。
- **打包**: `build-orbit-win.sh` 产出 zip 含 `orbit-gui.exe`(windowsgui 子系统)+ 新
  `orbit-cli.exe`; 老用户把新版 CLI 覆盖到 `C:\ProgramData\OrbitClient\` 后即可用 GUI。
- **构建注意**: GUI 目录不在公开仓(FOSS 发布只含内核+CLI); 构建前需用
  `rsrc` 从 `app.manifest`+`assets/orbit.ico` 生成 `rsrc_windows.syso`——
  无 comctl32 v6 manifest 时 64 位工具栏提示注册必崩(`TTM_ADDTOOL failed`)。