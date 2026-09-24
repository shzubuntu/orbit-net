#!/bin/bash
# 部署新 orbitd 到 systemd: 编译 -> 原子替换 -> 切到 systemd 托管
set -e
cd /root/project/orbit

echo "== build =="
GOOS=linux go build -trimpath -ldflags "-s -w" -o /usr/local/bin/orbitd.new ./cmd/orbitd
mv /usr/local/bin/orbitd /usr/local/bin/orbitd.old
mv /usr/local/bin/orbitd.new /usr/local/bin/orbitd
chmod +x /usr/local/bin/orbitd

echo "== systemd unit =="
cp /root/project/orbit/deploy/orbitd.service /etc/systemd/system/orbitd.service
systemctl daemon-reload

OLDPID=$(pgrep -x orbitd | head -1 || true)
if [ -n "$OLDPID" ] && systemctl is-active orbitd >/dev/null 2>&1; then
  systemctl restart orbitd
else
  systemctl start orbitd
fi
systemctl enable orbitd >/dev/null 2>&1 || true

sleep 2
echo "== status =="
systemctl is-active orbitd
pgrep -x orbitd
curl -s http://127.0.0.1:18444/health || true
echo ""
echo "== stale process check =="
pgrep -x orbitd.old || echo "no old orbitd.old running"
rm -f /usr/local/bin/orbitd.old