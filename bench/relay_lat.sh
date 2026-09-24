#!/bin/bash
# 中继延迟测量: 在两台已入网的 mesh 设备间(经 orbitd 中继)打 ICMP/RTT
# 用法: sudo bash relay_lat.sh <对端mesh IP> [次数]
#   - Linux  本端出接口默认用路由选择(本机 TUN 为 orbit0)
#   - Windows 需以管理员跑, 对端 IP 用 windows B 端 IP
set -e
PEER="$1"; CNT="${2:-20}"
[ -n "$PEER" ] || { echo "用法: $0 <对端meshIP> [次数]" >&2; exit 2; }

echo "== 中继延迟测量: -> $PEER ($CNT 次 ping) =="
ping -c "$CNT" "$PEER" 2>&1 | tee >(grep -E 'min/avg|rtt|timeout|100%' >/dev/null) | tail -3