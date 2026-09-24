# orbit-net

面向公网用户的私有网络产品（项目代号：**orbit**，公开仓名：**orbit-net**）。

一台自己的云服务器，5 分钟搭好私有组网中枢：多台设备接入同一虚拟网络、按需经
出口节点上网——不依赖第三方服务、不开通公网端口到内网。

## 为什么是 orbit

- **单二进制自包含**：`orbitd` 一个进程 = 账户/设备控制面 + TLS 数据面中继 +
  管理 API + 内嵌管理 UI。数据面是 TCP-only 中继（不依赖 UDP 打洞），国内复杂
  NAT / 对称 NAT 环境下连通性优于 P2P-first 方案。
- **精确计量**：服务器星型中继是唯一事实来源，账户日流量池 + 每设备出口泳道双维
  计量，免费内测分档、未来按量收费都在同一套 `Usage` 之上。
- **邀请码自助注册**：发码 → 客户端自助注册开户 → 组网，免人工开通。
- **跨平台客户端**：Linux / Windows 命令行客户端（systemd / 计划任务托管，支持
  虚拟网卡、smart/global 出口模式）；Windows 客户端自愈型计划任务 + 自动 OTA。

## 组件

| 组件 | 说明 |
|---|---|
| `orbitd` | 服务端：控制面（账户/设备/租约/用量/邀请码）+ 数据面中继 + 管理 API + 内嵌 UI |
| `orbit-cli` | 命令行客户端（Linux/Windows），三种模式：`simple` 仅组网 / `smart` 按规则走出口 / `global` 全流量经出口 |
| `orbit-gui` | Windows 图形客户端（规划中，未接入） |

## 快速开始

### 1. 安装服务端（Ubuntu/Debian，约 3 分钟）

```bash
# 拿到 orbitd.linux-amd64 后：
sudo mkdir -p /opt/orbit && sudo cp orbitd.linux-amd64 /usr/local/bin/orbitd
sudo bash install-orbitd.sh --ip <服务器公网IP或域名> --token <管理密码> [--web-port 4431] [--listen 28443]
```

脚本会：生成 `orbitd.yaml` + 自签 CA/服务端证书（纯 Go，无 openssl 依赖）→
写入 systemd 服务并启动 → 打印管理地址与首个邀请码生成命令。

> 数据面监听 `:28443`（TLS，客户端 wss）；公共注册 API 监听 `--web-port`
> （默认 `4431`，HTTPS，同一张证书）；admin API 与内嵌管理 UI 默认只监听
> `127.0.0.1:18444`（配合 SSH 端口转发或本机访问，公网不暴露）。

### 2. 发邀请码

```bash
curl -X POST http://127.0.0.1:18444/api/v1/admin/invites \
  -H 'X-Admin-Token: <管理token>' -H 'Content-Type: application/json' \
  -d '{"n":3}'
```

或打开 `http://127.0.0.1:18444/`（管理 UI）里点“生成邀请码”。

### 3. 客户端接入（Linux / Windows）

下载 `orbit-client-linux.tar.gz` / `orbit-client-windows.zip`：

- **Linux**：`bash register.sh`（填邀请码 + 服务端地址）→ 生成 `orbit-cli.yaml` →
  `sudo bash install.sh` 装 systemd 服务并启动。
- **Windows**：双击 `orbit-setup.bat`（自动提升管理员 → 填邀请码 + 服务端地址 →
  注册开户 + 写 `orbit-cli.yaml` + 建计划任务开机自启 + 创建 `Orbit` 虚拟网卡）。

验证：`ip addr show orbit0`（或 `ipconfig /all` 找 `Orbit` 网卡）有 `10.0.0.x` 地址，
两台设备互相 `ping` 通即组网成功。

### 4. 出口上网（可选）

管理员在 UI 里给设备开出口授权；客户端把 `orbit-cli.yaml` 的 `mode` 改为 `smart`
并配置 `rules` 指向出口设备，按域名/IP 分流经出口上网。

## 配额与计量（服务端计费语义）

发送方计费、服务端星型中继计数唯一事实来源：

- **账户日流量池 `bytes_per_day`**：账户维度共享（mesh 直连 + 出口全算），打满后该
  账户整日停发。
- **设备出口泳道 `egress_bytes_per_day`**：按**消费者设备**独立计算，互不共享；
  打满只停该终端的出口流量（下载/上传都计入消费者泳道）。
- 出口判定 = **外层 IP 协议号 4（IP-in-IP 封装）**：代理链路双向都记到消费者；
  直达出口盒的 mesh 包（单层头）只计账户池、不入出口泳道。
- 档位：`free` / `pro` / `team`（或自定义 `tiers` 表），未命中回落 `default_quota`；
  管理 API `POST /api/v1/admin/accounts/tier` 切换后立刻推送新配额。

## 管理 API 摘要

- 公网助手：`POST /api/v1/register`（邀请码注册）、`GET /api/v1/devices`、
  `POST /api/v1/devices/{rename,revoke,rotate-token}`、`GET /api/v1/usage`。
- 管理（`X-Admin-Token`，仅 admin 监听）：`/api/v1/admin/{accounts,clients,usage(s),
  invites,devices,egress}` 等。内嵌 UI 见服务端根路径 `/`。

## 构建与测试

```bash
go build ./...          # 全量构建（客户端依赖 wintun.dll 运行时置于可执行文件旁）
go test ./...           # 单测（含配额/计量回归）
go vet ./...            # 静态检查
# 版本号构建时注入：
go build -ldflags "-X orbit/internal/version.Version=$(git describe --tags --always)" ./cmd/orbitd
```

## 安全模型

- 双层 TLS：客户端到 `orbitd` 是 TLS 隧道；出口拨号走 gVisor 用户态协议栈，
  经 IP-in-IP 封装转发，出口节点对目的地可见明文（默认只授权本人/信任账号消费）。
- 账户独立虚拟网络（`networkID = accountID`），跨账户天然隔离；出口授权需
  `egress_enabled` + `egress_acl`。
- admin 管理面默认只监听 127.0.0.1；移动设备 token 可随时轮换/吊销；注册/握手有限流。
- `Admin.Addr` 与 `Admin.Token` 务必只放本机；公网场景请用防火墙 + SSH 隧道访问管理面。

## 路线图

- [x] M1：邀请码注册、设备 token 握手、IP 租约持久化、TLS 中继数据面、出口授权、精确计量
- [x] M2：UDP/ICMP 出口、global 模式、免费档配额硬限、设备自助管理、客户端重启加固
- [x] M3：计费档位（tiers）、用量持久化修复、管理 UI、出口归因（消费者泳道）
- [ ] 待续：P2P 直连、多区域数据面、Windows 图形客户端、托管版（多区中继/账单/审计，闭源）

## License

AGPL-3.0（开源核心）。托管增值层（多区域中继、SLA、账单/审计、增强 UI）为闭源。

## 开发者

开发与部署基线见 `docs/DESIGN.md`；部署/运维细节见 `AGENTS.md`（内部使用，不进仓）。