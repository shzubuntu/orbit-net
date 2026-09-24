#!/bin/bash
# orbit-cli Linux 客户端打包(自助开户版):
#   不再烤死演示账号/令牌; 新用户先跑 register.sh 领配置, 再 install.sh
#   本机构建 + install/uninstall + tar.gz 到 /opt/orbit/www/
set -e
cd /root/project/orbit

rm -rf /tmp/linpkg && mkdir -p /tmp/linpkg

# 1. 本机构建 linux amd64
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/linpkg/orbit-cli ./cmd/orbit-cli

# 2. 自助开户脚本: 填邀请码+服务器地址 -> 下载该服务器CA -> 公网注册 -> 生成 orbit-cli.yaml
cp /root/project/orbit/deploy/linux-pkg/register.sh /tmp/linpkg/register.sh
chmod +x /tmp/linpkg/register.sh

# 4. install.sh / uninstall.sh(必须 root; LF)
cat > /tmp/linpkg/install.sh <<'EOF'
#!/bin/bash
set -e
cd "$(dirname "$0")"
[ "$(id -u)" = 0 ] || { echo "need root (sudo bash install.sh)"; exit 1; }
APP=/opt/orbit-client
for f in orbit-cli orbitd-cert.pem orbit-cli.yaml; do
  [ -f "$f" ] || { echo "[ERR] $f missing - 请先运行 register.sh 领配置(生成 orbit-cli.yaml), 再执行安装"; exit 1; }
done
mkdir -p "$APP"
install -m 755 orbit-cli "$APP/orbit-cli"
install -m 644 orbitd-cert.pem "$APP/orbitd-cert.pem"
install -m 600 orbit-cli.yaml "$APP/orbit-cli.yaml"
cat > /etc/systemd/system/orbit-client.service <<SVC
[Unit]
Description=Orbit VPN client (orbit-cli)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$APP/orbit-cli -config $APP/orbit-cli.yaml
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SVC
systemctl daemon-reload
systemctl enable --now orbit-client
sleep 3
systemctl --no-pager -l status orbit-client | head -10 || true
echo "Installed. Log: /var/log/orbit-cli.log"
EOF

cat > /tmp/linpkg/uninstall.sh <<'EOF'
#!/bin/bash
set -e
[ "$(id -u)" = 0 ] || { echo "need root (sudo bash uninstall.sh)"; exit 1; }
systemctl stop orbit-client 2>/dev/null || true
systemctl disable orbit-client 2>/dev/null || true
rm -f /etc/systemd/system/orbit-client.service
systemctl daemon-reload
pkill -TERM -f '^/opt/orbit-client/orbit-cli' 2>/dev/null || true
sleep 2
ip link delete orbit0 2>/dev/null || true
rm -rf /opt/orbit-client
rm -f /var/log/orbit-cli.log*
echo "Uninstalled."
EOF

chmod +x /tmp/linpkg/install.sh /tmp/linpkg/uninstall.sh

# 5. README
cat > /tmp/linpkg/README.txt <<'EOF'
=== Orbit Client (Linux) ===
1. [首次] bash register.sh -> 输入 服务器地址(如 vpn.example.com:4431) + 邀请码 -> 自动下载服务器CA + 注册 -> 生成 orbit-cli.yaml
2. sudo bash install.sh -> 安装到 /opt/orbit-client, 注册 systemd 服务 orbit-client 并启动
3. 状态: systemctl status orbit-client; 日志: /var/log/orbit-cli.log
4. 出口上网: 向管理员申请授权, 授权后把 orbit-cli.yaml 的 mode 改为 smart 并配置 rules
5. 卸载: sudo bash uninstall.sh(停服务/删配置二进制/删网卡/清日志)
---
EOF

# 6. 打 tar.gz 到下载目录
cd /tmp/linpkg
rm -f /opt/orbit/www/orbit-client-linux.tar.gz
tar -czf /opt/orbit/www/orbit-client-linux.tar.gz .
ls -la /opt/orbit/www/orbit-client-linux.tar.gz
echo "LINUX PKG READY"