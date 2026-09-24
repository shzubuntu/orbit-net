#!/bin/bash
# 中继吞吐量测量: 经 mesh(orbitd 中继)在两端间打双向 TCP/UDP 流量
# 依赖(两端任选其一): iperf3 (推荐), 否则退回 nc+dd
# 用法:
#   A 端(server):  sudo bash relay_throughput.sh server <端口>
#   B 端(client):  sudo bash relay_throughput.sh client <A的meshIP> <端口> [时长秒]
set -e

MODE="$1"; ARG2="$2"; ARG3="$3"; DUR="${4:-10}"

if [ "$MODE" = "server" ]; then
  PORT="${ARG2:-5201}"
  if command -v iperf3 >/dev/null; then
    echo "== iperf3 -s -p $PORT (监听中) =="; iperf3 -s -p "$PORT"
  else
    echo "== nc 监听 $PORT (dumb 吸流) =="; nc -lu -p "$PORT" > /dev/null
  fi
elif [ "$MODE" = "client" ]; then
  PEER="$ARG2"; PORT="${ARG3:-5201}"
  [ -n "$PEER" ] || { echo "usage: $0 client <A的meshIP> [端口] [时长]" >&2; exit 2; }
  if command -v iperf3 >/dev/null; then
    echo "== iperf3 -c $PEER -p $PORT -t $DUR --len 1400 =="
    iperf3 -c "$PEER" -p "$PORT" -t "$DUR" --len 1400
  else
    echo "== nc+dd 上限测速(dumb) =="
    dd if=/dev/zero bs=1400 count=0 2>/dev/null
    timeout "$((DUR+2))" nc -u "$PEER" "$PORT" < /dev/zero | wc -c
  fi
else
  echo "用法: $0 (server|client) ..."; exit 2
fi