#!/bin/bash
# orbitd 一键安装(Ubuntu/Debian, 需 root):
#   1) 定位并安装 orbitd 二进制到 /usr/local/bin
#   2) orbitd genconfig: 自签 CA + 服务端证书 + orbitd.yaml(数据面/公共HTTPS/管理面)
#   3) 写 systemd 单元并启用启动
#   4) 健康检查 + 生成首批发 3 个邀请码 + 打印使用指引
#
# 用法:
#   sudo bash install-orbitd.sh --ip <服务器公网IP或域名> [--token X] [--listen 28443] [--web 4431] [--admin 127.0.0.1:18444] [--dir /opt/orbit]
#   orbitd 二进制: 同目录的 ./orbitd / ./orbitd.linux-amd64, 或环境变量 ORBITD_BIN
set -euo pipefail

DIR=/opt/orbit
IP=""
TOKEN=""
LISTEN=28443
WEB=4431
ADMIN=127.0.0.1:18444
UNIT=/etc/systemd/system/orbitd.service

while [ $# -gt 0 ]; do
  case "$1" in
    --ip) IP="$2"; shift 2;;
    --token) TOKEN="$2"; shift 2;;
    --listen) LISTEN="$2"; shift 2;;
    --web) WEB="$2"; shift 2;;
    --admin) ADMIN="$2"; shift 2;;
    --dir) DIR="$2"; shift 2;;
    -h|--help) sed -n '1,10p' "$0"; exit 0;;
    *) echo "[ERR] 未知参数: $1"; exit 2;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "[ERR] 需要 root: sudo bash install-orbitd.sh --ip ..."; exit 1; }
[ -n "$IP" ] || { echo "[ERR] 缺少 --ip <服务器公网IP或域名>"; exit 2; }

# 1) 定位二进制
BIN=""
for c in "${ORBITD_BIN:-}" ./orbitd ./orbitd.linux-amd64 ./orbitd.linux-x86_64 /usr/local/bin/orbitd; do
  if [ -n "$c" ] && [ -x "$c" ]; then BIN="$c"; break; fi
done
if [ -z "$BIN" ]; then
  echo "[ERR] 未找到 orbitd 二进制(放同目录或设 ORBITD_BIN)" >&2
  exit 3
fi
echo "[1/5] 安装二进制 $BIN -> /usr/local/bin/orbitd"
install -m 755 "$BIN" /usr/local/bin/orbitd

# 2) 生成配置与证书
echo "[2/5] genconfig: 目录 $DIR 主机 $IP (listen=$LISTEN web=$WEB admin=$ADMIN)"
mkdir -p "$DIR"
ARGS=(--host "$IP" --listen "$LISTEN" --web "$WEB" --admin "$ADMIN" --dir "$DIR")
if [ -n "$TOKEN" ]; then ARGS+=(--token "$TOKEN"); fi
/usr/local/bin/orbitd genconfig "${ARGS[@]}"
# 显示生成的管理 token(仅本次)
TOKEN=$(python3 -c 'import yaml;print(yaml.safe_load(open("'$DIR'/orbitd.yaml"))["admin"]["token"])' 2>/dev/null || \
        grep -A1 '^admin:' "$DIR/orbitd.yaml" | grep token | awk '{print $2}')
[ -n "$TOKEN" ] || TOKEN="<见$DIR/orbitd.yaml 的 admin.token>"

# 3) systemd 单元
echo "[3/5] 写 systemd 单元 $UNIT"
cat > "$UNIT" <<EOF
[Unit]
Description=Orbit mesh coordinator (data plane + admin)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/orbitd -config $DIR/orbitd.yaml
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable orbitd >/dev/null 2>&1 || true
systemctl restart orbitd
sleep 2
systemctl is-active orbitd || { echo "[ERR] orbitd 未启动, 看 journalctl -u orbitd"; exit 4; }

# 4) 健康检查 + 邀请码
echo "[4/5] 健康检查"
curl -fsS "http://$ADMIN/health" || { echo "[ERR] admin 健康检查失败"; exit 5; }
echo ""
echo "[5/5] 生成 3 个邀请码"
INV=$(curl -s -X POST "http://$ADMIN/api/v1/admin/invites?count=3" -H "X-Admin-Token: $TOKEN")
echo "$INV" | python3 -m json.tool 2>/dev/null || echo "$INV"

echo ""
echo "======================================================"
echo "orbitd 安装完成!"
echo "  数据面     : $IP:$LISTEN (客户端 server_addr = $IP:$LISTEN)"
echo "  公共 HTTPS : $IP:$WEB (注册/自助/CA 下载, 需在云控制台放行 $LISTEN/$WEB)"
echo "  管理面     : $ADMIN (UI/API 仅本机)"
echo "  管理 token : $TOKEN"
echo ""
echo "  管理 UI: ssh -L 18444:127.0.0.1:18444 root@$IP  然后浏览器开 http://127.0.0.1:18444/"
echo "  客户端指引: 下发 orbit-client 包, register 时填服务器地址 $IP"
echo "======================================================"
echo "安全提醒: 管理面默认仅本机; 如已预置 --admin 0.0.0.0 请确认有防火墙保护。"