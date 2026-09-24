#!/bin/bash
# Orbit 自助开户: 填邀请码 + 服务端地址 -> 下载该服务器 CA -> 调公网注册 -> 生成本机 orbit-cli.yaml
# 用法: bash register.sh
set -u
cd "$(dirname "$0")"

echo "== Orbit 自助开户 =="
read -rp "服务器地址(公共HTTPS入口, 形如 vpn.example.com:4431; 管理员提供): " SRVHOST
[ -n "$SRVHOST" ] || { echo "[ERR] 服务器地址不能为空(找管理员要 公网IP:4431)" >&2; exit 1; }
case "$SRVHOST" in
  http://*|https://*) BASE="${SRVHOST%/}" ;;
  *) BASE="https://$SRVHOST" ;;
esac
read -rp "数据面端口(默认 28443): " DST_PORT
[ -n "$DST_PORT" ] || DST_PORT=28443
read -rp "邀请码(由管理员发放): " INVITE
[ -n "$INVITE" ] || { echo "[ERR] 邀请码不能为空" >&2; exit 1; }
read -rp "账户名(回车默认 mm-$(whoami)): " ACC
[ -n "$ACC" ] || ACC="mm-$(whoami)"
read -rp "设备名(回车默认 $(hostname)): " DEV
[ -n "$DEV" ] || DEV="$(hostname)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "[ERR] 需要 python3 解析注册响应" >&2
  exit 5
fi

TMP_REQ=$(mktemp) ; TMP_RESP=$(mktemp)
trap 'rm -f "$TMP_REQ" "$TMP_RESP" "$TMP_REQ.err"' EXIT
cat > "$TMP_REQ" <<EOF
{"invite_code":"$INVITE","account_name":"$ACC","device_name":"$DEV"}
EOF

# 服务器为自签定制 CA: 先取 CA 证书(注册/隧道共用同张私有 CA), 再做注册。
# 链路保机制: HTTPS + 邀请码即闸; 管理员可用公网可信证书替换后去掉 -k。
echo "从 $BASE 下载服务器 CA 证书 ..."
if ! curl -kfsS -m 20 "$BASE/ca.pem" -o orbitd-cert.pem; then
  echo "[ERR] 下载 CA 失败(服务器公共HTTPS不可达? $BASE)" >&2
  exit 6
fi

if ! curl -kfsS -m 20 -X POST "$BASE/api/v1/register" \
     -H "Content-Type: application/json" -d @"$TMP_REQ" > "$TMP_RESP" 2>"$TMP_REQ.err"; then
  echo "[ERR] 注册请求失败: $(cat "$TMP_REQ.err" 2>/dev/null)"
  echo "[ERR] 服务端返回: $(cat "$TMP_RESP" 2>/dev/null)"
  exit 2
fi

read -r OK ACC DID DTOK < <(python3 - "$TMP_RESP" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(d.get('ok', ''), d.get('account_name', ''), d.get('device_id', ''), d.get('device_token', ''))
PY
)

if [ "$OK" != "True" ] && [ "$OK" != "true" ]; then
  echo "[ERR] 注册被拒: $(cat "$TMP_RESP")"
  exit 3
fi
[ -n "$DID" ] && [ -n "$DTOK" ] || { echo "[ERR] 响应缺少 device_id/token" >&2; exit 4; }

case "$SRVHOST" in
  *:*) HOST="${SRVHOST%:*}" ;;
  *)   HOST="${SRVHOST}" ;;
esac

echo ""
echo "注册成功: 账户 $ACC / 设备 $DID"
echo "设备令牌已写入配置(仅此一次展示, 请勿外泄): $DTOK"

cat > orbit-cli.yaml <<EOF
server_addr: $HOST:$DST_PORT
ca_cert_path: /opt/orbit-client/orbitd-cert.pem
account: $ACC
device_id: $DID
device_token: $DTOK
hostname: $DEV
tun_name: orbit0
mode: simple
egress: false
log_file: /var/log/orbit-cli.log
log_max_bytes: 4194304
log_keep: 2
self_api_base: $BASE/api/v1
EOF

echo ""
echo "已生成 orbit-cli.yaml + orbitd-cert.pem(与 install.sh 同目录)。下一步: sudo bash install.sh 安装并启动。"
echo "如需出口上网(获得管理员授权后)可把 mode 改为 smart 并配置 rules 指向出口设备。"