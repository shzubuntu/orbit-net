package server

import (
	"embed"
	"net/http"
)

//go:embed web/orbit.html
var webFS embed.FS

const webUIMain = "web/orbit.html"

// handleWebUI 内嵌管理 UI: 管理面根路径返回单页应用(API 调用见页面内 JS)。
// 未知路径一律 404(避免 "/" 通配吞掉其他路由)。
func (s *Server) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := "/"
	if len(r.URL.Path) > 1 {
		// 允许显式访问 /web/ 单页,避免静态资源路径纠纷
		if r.URL.Path != "/web/" {
			http.NotFound(w, r)
			return
		}
		base = "/web/"
	}
	b, err := webFS.ReadFile(webUIMain)
	if err != nil {
		http.Error(w, "webui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
	_ = base
}
