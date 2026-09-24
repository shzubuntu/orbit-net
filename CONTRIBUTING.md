# 贡献指南

欢迎贡献 orbit-net（项目代号 orbit）。本仓许可为 AGPL-3.0，提交即视为同意该许可条款。

## 开发流程

1. Fork 本仓，从 `main` 开功能分支（`feat/xxx`、`fix/xxx`）。
2. 本地完成并自测：
   - `gofmt -l .` 无输出
   - `go vet ./...` 无告警
   - `go test ./...` 全绿
3. 提交信息遵循：`type(scope): 描述`，如 `fix(quota): egress 泳道回填改用 EgressLaneTotals`。
4. 开 PR，说明改动动机与验证方式；涉及数据面/计量语义的改动必须附回归测试。

## 代码约定

- 控制面模型与计量在 `internal/account`；服务端在 `internal/server`；客户端在 `internal/client`。
- 计量语义（发送方计费 / 账户日池 / 每设备出口泳道 / IPIP 判别）见 `README.md`，
  改动前先读 `docs/DESIGN.md`——**计量是计费唯一事实来源，改动需测试同步更新**。
- 涉及公网暴露面的新增 API 需写明鉴权方式（设备 token / admin token）。
- 新库依赖需说明理由；gVisor 固定伪版本号（见 `go.mod` 注释）不要随意升级。

## 问题与安全

- 普通问题开 issue；安全问题走 `SECURITY.md` 的渠道，不要在 issue 里披露。
- 公开仓不提交任何证书/密钥/真实 token（见 `.gitignore`）。