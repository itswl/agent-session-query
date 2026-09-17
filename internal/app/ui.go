package app

import (
	"embed"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
)

// 页面用 go:embed 打进二进制：不引 npm、不加构建步骤，仍然是单文件分发。
//
//go:embed ui
var uiAssets embed.FS

// uiSub 是去掉 "ui/" 前缀后的静态资源树
func uiSub() (fs.FS, error) {
	return fs.Sub(uiAssets, "ui")
}

// uiCSP 页面的内容安全策略。
//
// 页面渲染的是第三方文本（工具输出、网页正文），XSS 的后果是 localStorage 里的
// token 被读走。渲染一律走 textContent 是第一道防线（ui_test.go 钉着），
// CSP 是第二道：脚本样式只许同源、不许内联、不许被别人 iframe 套进去。
// 页面本身没有内联 script/style，加这条不需要改任何东西。
const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'none'"

// serveUIAssets 处理 /ui 与 /ui/*。
//
// 页面本身不含任何数据（要 token 才能拿到），所以和 /health 一样免认证；
// 真正的会话内容仍然只有带上 Authorization 头才能取到。
func serveUIAssets(w http.ResponseWriter, r *http.Request, path string) {
	sub, err := uiSub()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
		return
	}

	w.Header().Set("Content-Security-Policy", uiCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	if path == "/ui" || path == "/ui/" {
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 页面是内嵌的，改一次二进制就该刷一次，别让浏览器缓存住旧版本
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
		return
	}

	http.StripPrefix("/ui/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

func isUIPath(path string) bool {
	return path == "/ui" || strings.HasPrefix(path, "/ui/")
}

// serveFavicon 提供 /favicon.ico，返回实际写出的状态码。
//
// 浏览器打开任何页面都会顺手去根路径要一次这个文件——不只是 /ui，访问 / 那个
// JSON 也会。不接住的话每开一次页面就在访问日志里留一条 404，配了 token 时
// 还是一条 401，看着像有人在瞎探。图标本身不含数据，和 /ui 一样免认证。
func serveFavicon(w http.ResponseWriter) int {
	sub, err := uiSub()
	if err == nil {
		var icon []byte
		if icon, err = fs.ReadFile(sub, "favicon.ico"); err == nil {
			w.Header().Set("Content-Type", "image/x-icon")
			w.Header().Set("Content-Length", strconv.Itoa(len(icon)))
			// 图标是内嵌的，同一个二进制里不会变
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(icon)
			return http.StatusOK
		}
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
	return http.StatusInternalServerError
}
