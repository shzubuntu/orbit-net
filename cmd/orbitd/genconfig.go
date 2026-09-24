package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// runGenconfig 生成服务端全量配置:
//
//	自签 CA(ca.pem/ca-key.pem) -> 服务端证书(orbitd-cert.pem/orbitd-key.pem, SAN=host)
//	-> orbitd.yaml(数据面/公共 HTTPS/管理面 三监听 + 档位)。
//
// 纯 Go 实现, 不依赖 openssl。用法:
//
//	orbitd genconfig --host <ip|域名> [--listen 28443] [--web 4431] [--admin 127.0.0.1:18444] [--token X] [--dir /opt/orbit]
func runGenconfig(args []string) error {
	fs := flag.NewFlagSet("genconfig", flag.ContinueOnError)
	var host, listen, web, admin, token, dir string
	fs.StringVar(&host, "host", "", "服务器公网 IP 或域名(必填, 写入服务端证书 SAN)")
	fs.StringVar(&listen, "listen", "28443", "数据面 TLS 监听端口")
	fs.StringVar(&web, "web", "4431", "公共 HTTPS 自助入口端口(空=不启用)")
	fs.StringVar(&admin, "admin", "127.0.0.1:18444", "管理面监听地址(仅本机)")
	fs.StringVar(&token, "token", "", "管理 token(默认随机生成)")
	fs.StringVar(&dir, "dir", ".", "输出目录(生成 certs/ 与 orbitd.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("--host 必填(服务器公网 IP 或域名)")
	}
	host = strings.TrimSpace(host)
	if net.ParseIP(host) == nil && !strings.Contains(host, ".") {
		return fmt.Errorf("--host 看起来既不是 IP 也不是域名: %s", host)
	}
	if token == "" {
		tok, err := randToken(12)
		if err != nil {
			return err
		}
		token = tok
	}
	if len(token) < 12 {
		return fmt.Errorf("--token 至少 12 位")
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("--dir: %w", err)
	}
	certDir := filepath.Join(dirAbs, "certs")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return fmt.Errorf("mkdir certs: %w", err)
	}

	caCert, caKey, caPEM, err := makeCA()
	if err != nil {
		return err
	}
	srvCertPEM, srvKeyPEM, err := makeServerCert(caCert, caKey, host)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.pem"), caPEM, 0o644); err != nil {
		return fmt.Errorf("write ca.pem: %w", err)
	}
	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}), 0o600); err != nil {
		return fmt.Errorf("write ca-key.pem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "orbitd-cert.pem"), srvCertPEM, 0o644); err != nil {
		return fmt.Errorf("write orbitd-cert.pem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "orbitd-key.pem"), srvKeyPEM, 0o600); err != nil {
		return fmt.Errorf("write orbitd-key.pem: %w", err)
	}

	cfg := buildYAML(dirAbs, host, listen, web, admin, token)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	cfgPath := filepath.Join(dirAbs, "orbitd.yaml")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}

	fmt.Println("== orbitd genconfig 完成 ==")
	fmt.Printf("  目录      : %s\n", dirAbs)
	fmt.Printf("  数据面    : 0.0.0.0:%s (TLS 监听; 客户端 server_addr = %s:%s)\n", listen, host, listen)
	fmt.Printf("  公共 HTTPS: %s:%s (注册/自助/CA 下载)\n", host, web)
	fmt.Printf("  管理面    : %s (UI/API, 仅本机可达)\n", admin)
	fmt.Printf("  管理 token: %s\n", token)
	fmt.Println()
	fmt.Println("启动: sudo orbitd -config " + cfgPath)
	fmt.Println("生成邀请码: curl -X POST http://" + admin + "/api/v1/admin/invites -H 'X-Admin-Token: " + token + "'")
	fmt.Println("管理 UI  : 先 ssh -L 18444:127.0.0.1:18444 你@服务器, 再开 http://127.0.0.1:18444/")
	return nil
}

// buildYAML 组装 orbitd 配置(路径全部绝对)。
func buildYAML(dir, host, listen, web, admin, token string) map[string]any {
	certDir := filepath.Join(dir, "certs")
	webAddr := ""
	if web != "" {
		webAddr = "0.0.0.0:" + web
	}
	return map[string]any{
		"listen_addr": "0.0.0.0:" + listen,
		"tls": map[string]string{
			"cert_file": filepath.Join(certDir, "orbitd-cert.pem"),
			"key_file":  filepath.Join(certDir, "orbitd-key.pem"),
		},
		"public": map[string]string{
			"addr":    webAddr,
			"ca_file": filepath.Join(certDir, "ca.pem"),
		},
		"networks": map[string]string{"default": "10.0.0.0/24"},
		"admin": map[string]string{
			"addr":  admin,
			"token": token,
		},
		"data_dir":      filepath.Join(dir, "data"),
		"log_file":      filepath.Join(dir, "orbitd.log"),
		"log_max_bytes": 10485760,
		"log_keep":      3,
		"default_tier":  "free",
		"default_quota": map[string]int64{
			"bytes_per_day":        1073741824, // 1 GiB/日
			"egress_bytes_per_day": 536870912,  // 512 MiB/日
		},
		"tiers": map[string]any{
			"free": map[string]int64{"bytes_per_day": 1073741824, "egress_bytes_per_day": 536870912},
			"pro":  map[string]int64{"bytes_per_day": 10737418240, "egress_bytes_per_day": 2147483648},
			"team": map[string]int64{"bytes_per_day": 107374182400, "egress_bytes_per_day": 10737418240},
		},
	}
}

// makeCA 生成自签 CA(ECDSA P-256)。
func makeCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca key: %w", err)
	}
	sn, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "orbit-net CA", Organization: []string{"orbit-net"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ca create: %w", err)
	}
	return tmpl, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// makeServerCert 用 CA 签发服务端证书(SAN 为 host 的 IP 或域名)。
func makeServerCert(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, host string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("server key: %w", err)
	}
	sn, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"orbit-net"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("server create: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// randToken 生成 N 字节随机 hex 作为管理 token。
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
