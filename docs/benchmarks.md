# Benchmarks

本文件记录 orbit 数据面的实测基准。方法与基线如下（真机 mesh 成员：`homepc`、`Songhz`，服务器 `srv`）。

## 拓扑与命令

- 服务器(orbitd/中继) : `wss://47.101.198.197:9443/ws` 上游不变，中继在服务器侧完成。
- 成员：`homepc`(win, 10.0.0.x) `Songhz`(win, 10.0.0.x) `homevm/srv-exit`(linux)。

延迟：
```bash
sudo bash bench/relay_lat.sh <对端meshIP> 20
```

吞吐（两端各开一终端）：
```bash
sudo bash bench/relay_throughput.sh server 5201        # A 端
sudo bash bench/relay_throughput.sh client <A的meshIP> 5201 30   # B 端
```

## 基线数据（2026-09 记录，持续更新）

| 链路 | 指标 | 实测 | 备注 |
|---|---|---|---|
| 公网 本机→服务器 | RTT | ~20-40ms | 各家 ISP 差异 |
| 中继 mesh 延迟 homepc↔homevm（home-mesh，via srv） | avg RTT | ~19-21ms（20/20，min 19 / max 60） | 2026-09-25 实测 |
| 中继 mesh 延迟 surface/Songhz↔srv-exit（win-beta，via srv） | avg RTT | ~13ms（20/20，min 12 / max 16） | 2026-09-25 实测 |
| 中继吞吐 iperf3 | Mbps | 待补 | B(dumb→A) |

> 说明：orbit 数据面当前核心诉求是「同城双点互联 + 最后一公里容灾」，不追求
> 大流量透传；吞吐量主要瓶颈在服务器带宽与发送方配额(bytes_per_day / egress泳道)。

## 附加测试点

- 客户端断线重连：`systemctl restart orbitd` 后各端自动回连（指数退避 2s→60s+抖动）。
- OTA：`ota <expr>` 后老进程 report→apply→重启，秒级回连。
- 出口泳道正确性：外层 IP 协议号 4 的代理链路记消费者泳道，直达 mesh 包只计账户池。