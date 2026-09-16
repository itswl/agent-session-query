package app

import (
	"embed"
	"io/fs"
	"net/http"
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
