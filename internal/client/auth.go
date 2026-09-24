package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"orbit/internal/protocol"
)

// dialAndAuth 建立 TLS 连接并完成设备 token 握手,返回连接与 WELCOME。
func dialAndAuth(cfg *Config) (net.Conn, *protocol.Welcome, error) {
	tlsCfg, err := buildClientTLSConfig(cfg.CACertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("build tls config: %w", err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", cfg.ServerAddr, tlsCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", cfg.ServerAddr, err)
	}

	hello := &protocol.Hello{
		User:     cfg.Account,
		DeviceID: cfg.DeviceID,
		Hostname: cfg.Hostname,
		OS:       runtime.GOOS,
		Egress:   cfg.Egress,
		Mode:     cfg.Mode,
		Auth:     protocol.Auth{Mode: protocol.AuthDevice, Token: cfg.DeviceToken},
	}
	payload, err := protocol.EncodeCtrl(&protocol.CtrlMsg{Type: protocol.MsgHello, Hello: hello})
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := protocol.WriteFrame(conn, protocol.FrameCtrl, payload); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, resp, err := protocol.ReadFrame(conn, make([]byte, 0, 4096))
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("read welcome: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if typ != protocol.FrameCtrl {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("expect ctrl frame, got 0x%x", typ)
	}
	m, err := protocol.DecodeCtrl(resp)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("decode welcome: %w", err)
	}
	switch m.Type {
	case protocol.MsgWelcome:
		if m.Welcome == nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("welcome has no payload")
		}
		return conn, m.Welcome, nil
	case protocol.MsgReject:
		reason := "rejected"
		if m.Reject != nil {
			reason = m.Reject.Reason
		}
		_ = conn.Close()
		return nil, nil, fmt.Errorf("server rejected: %s", reason)
	default:
		_ = conn.Close()
		return nil, nil, fmt.Errorf("unexpected response: %s", m.Type)
	}
}

// buildClientTLSConfig 服务器自签证书:配置 CA 即校验证书,否则跳过验证(仅调试)。
func buildClientTLSConfig(caPath string) (*tls.Config, error) {
	if caPath == "" {
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read ca %s: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ca %s: no certs appended", caPath)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
