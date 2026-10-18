package api

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

// dashboardFiles is the event-stream UI. It is served by the process itself so
// an operator can open the log without a second frontend deploy.
//
//go:embed web
var dashboardFiles embed.FS

// handleDashboardPage serves the event stream dashboard.
//
// The page is unauthenticated. The data it draws comes from the existing
// consume API, which still requires the admin token or a subscription key
// when one is configured. The page collects that credential in the browser
// and sends it as a Bearer token; it is never embedded in the HTML.
//
// @Summary      事件流看板
// @Description  在浏览器里查看各流的序号和最近事件。页面本身不鉴权；数据请求走消费接口，配置了 admin token 时在页面上填写。
// @Tags         consume
// @Produce      html
// @Success      200  {string}  string  "看板 HTML"
// @Router       /dashboard [get]
// @Router       / [get]
func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	s.writeDashboardFile(w, r, "index.html")
}

// handleDashboardAsset serves one dashboard static file (css / js).
func (s *Server) handleDashboardAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !validDashboardAsset(name) {
		http.NotFound(w, r)
		return
	}
	s.writeDashboardFile(w, r, name)
}

func validDashboardAsset(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return false
	}
	switch path.Ext(name) {
	case ".css", ".js", ".svg":
		return true
	default:
		return false
	}
}

func (s *Server) writeDashboardFile(w http.ResponseWriter, r *http.Request, name string) {
	data, err := dashboardFiles.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", dashboardContentType(name))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func dashboardContentType(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		return "text/html; charset=utf-8"
	}
}
