package server

import (
	"log"
	"net/http"
	"os"
	"sync"
)

// publicMux 公共 HTTPS 入口(客户端注册/自助管理/CA 下发),与 admin 面分离:
// 管理面保持 127.0.0.1,公共面只暴露 device-token 可访问与公开的资源。
func (s *Server) publicMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/health", s.handleHealth)
	// 公网自助入口(邀请码注册 / 设备自助管理 / 用量查询)
	m.HandleFunc("/api/v1/register", s.handleRegister)
	m.HandleFunc("/api/v1/devices", s.handleDevices)
	m.HandleFunc("/api/v1/devices/rename", s.handleDeviceRename)
	m.HandleFunc("/api/v1/devices/revoke", s.handleDeviceRevoke)
	m.HandleFunc("/api/v1/devices/rotate-token", s.handleDeviceRotateToken)
	m.HandleFunc("/api/v1/usage", s.handleSelfUsage)
	// 客户端 CA 证书下载(自助注册脚本先取 ca.pem 再注册)
	m.HandleFunc("/ca.pem", s.handleCAPEM)
	// 根路径: 最小产品落地页(管理 UI 仅本机可达)
	m.HandleFunc("/", s.handleLanding)
	return m
}

// ListenAndServePublic 启动公共 HTTPS 监听(与数据面共用同一张 TLS 证书)。
func (s *Server) ListenAndServePublic() error {
	addr := s.cfg.Public.Addr
	if addr == "" {
		return errServerClosed
	}
	ln, err := listenTLS(addr, s.cfg.TLS)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.publicMux()}
	log.Printf("orbitd public https serving %s", ln.Addr())
	return srv.Serve(ln)
}

// handleCAPEM 下发 CA 证书(若配置了 public.ca_file)。
func (s *Server) handleCAPEM(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Public.CAFile == "" {
		http.NotFound(w, nil)
		return
	}
	b := s.caCache()
	if b == nil {
		http.Error(w, "ca file missing", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="orbitd-cert.pem"`)
	_, _ = w.Write(b)
}

var (
	caOnce sync.Once
	caPEM  []byte
)

func (s *Server) caCache() []byte {
	caOnce.Do(func() {
		b, err := os.ReadFile(s.cfg.Public.CAFile)
		if err != nil {
			log.Printf("public ca: %v", err)
			return
		}
		caPEM = b
	})
	return caPEM
}

// handleLanding 公共根路径的最小产品说明页。管理 UI 只在本机 admin 端口。
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<title>orbit-net</title><style>body{font-family:sans-serif;margin:10% auto;max-width:640px;text-align:center;color:#333}
code{background:#f3f4f6;padding:2px 6px;border-radius:4px}</style></head><body>
<h1>orbit-net 私有组网</h1>
<p>这里是 <code>orbitd</code> 的公共 HTTPS 入口：客户端注册 / CA 证书下发 / 健康检查。</p>
<p>管理界面与管理 API 只在服务器本机（默认 <code>127.0.0.1:18444</code>）提供，
不在公网开放。请通过 SSH 端口转发访问：<code>ssh -L 18444:127.0.0.1:18444 you@server</code>
后打开 <a href="http://127.0.0.1:18444/">http://127.0.0.1:18444/</a>。</p>
</body></html>`))
}
