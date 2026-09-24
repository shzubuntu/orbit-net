# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。版本号由
`git describe --tags` 注入构建（`internal/version`）。

## [未发布]

### Added
- 服务端自包含：内嵌管理 UI（admin 监听根路径 `/`）、`orbitd genconfig`（自签 CA +
  服务端证书，纯 Go）、`install-orbitd.sh` 一键安装、公共注册 HTTPS 监听（`public.addr`，
  默认 `0.0.0.0:4431`，含 `/ca.pem` 与注册/自助 API）。
- 开源外壳：AGPL-3.0 LICENSE、CONTRIBUTING、SECURITY、README 重写（数据面已实现）、
  GitHub Actions（ci 单测/交叉构建 + release 装配）、`bench/` 压测脚本 + `docs/benchmarks.md`。
- 2026-09-24 公开发布公开仓 **orbit-net**（AGPL-3.0）。
  地址：<https://github.com/shzubuntu/orbit-net>

### Changed
- 入口拆分：注册/自助 API（device 鉴权）与 admin API 分离，admin 保持仅本机。
- 客户端自助注册脚本（register.sh / register.ps1）改为**询问服务端地址**并自动下载
  该服务器的 `/ca.pem`，不再烤死固定服务器。

### Fixed
- 修复消费设备出口泳道重启后被清空的隐患（回填改用 `EgressLaneTotals`）。
- 修复 portal「不限 / 1.0 GB」用量显示错乱（`hs()` 归零语义）。

## [M3-snapshot / 2026-09]
- 计费档位 `tiers`、用量持久化（字符串键 `account|device|egress|hour`）、
  出口归因按消费者设备双向泳道（IPIP 判别）、管理 UI 与完整 admin API。