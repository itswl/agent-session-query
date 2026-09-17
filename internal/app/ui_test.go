package app

import (
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

func TestUIRoutes(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	// 页面免认证：它本身不含数据
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/ui = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "/ui/app.js") || !strings.Contains(body, "/ui/style.css") {
		t.Fatalf("/ui 没有引用静态资源: %s", body[:min(200, len(body))])
	}

	// /ui/ 等价
	if resp, err := http.Get(srv.URL + "/ui/"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("/ui/ = %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// 静态资源与 Content-Type
	for _, asset := range []struct{ path, wantType, wantText string }{
		{"/ui/app.js", "javascript", "textContent"},
		{"/ui/style.css", "text/css", "--bg"},
	} {
		resp, err := http.Get(srv.URL + asset.path)
		if err != nil {
			t.Fatal(err)
		}
		content := readBody(t, resp)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), asset.wantType) {
			t.Fatalf("%s = %d %s", asset.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(content, asset.wantText) {
			t.Fatalf("%s 内容不对", asset.path)
		}
	}

	// 不存在的资源
	if resp, err := http.Get(srv.URL + "/ui/nope.js"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("/ui/nope.js = %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// 页面免认证不等于数据免认证
	if resp, err := http.Get(srv.URL + "/sessions"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("没有 token 取数据应当 401，得到 %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
}

// TestUIRendersWithoutHTMLInjection：会话内容是第三方文本（工具输出、网页正文），
// 页面必须只用 textContent 渲染。这条规则钉在测试里，防止以后有人图快改成拼接。
func TestUIRendersWithoutHTMLInjection(t *testing.T) {
	sub, err := uiSub()
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("}
	for _, name := range []string{"index.html", "app.js", "style.css"} {
		content, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(content), bad) {
				t.Fatalf("%s 里出现了 %s —— 会话内容必须用 textContent 渲染", name, bad)
			}
		}
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 1<<20 {
			break
		}
	}
	return string(buf)
}

// TestFaviconRoute：浏览器打开任何页面都会去根路径要一次 /favicon.ico，
// 不接住的话每次访问都在日志里留一条 404（配了 token 时是 401）。
func TestFaviconRoute(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	resp, err := http.Get(srv.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 配了 token 也要免认证：图标里没有数据
	if resp.StatusCode != 200 {
		t.Fatalf("/favicon.ico = %d，应当免认证返回 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/x-icon" {
		t.Fatalf("Content-Type = %q", ct)
	}

	icon, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// ICO 头：保留位 0、类型 1、图像数量 > 0
	if len(icon) < 6 || icon[0] != 0 || icon[1] != 0 || icon[2] != 1 || icon[3] != 0 {
		t.Fatalf("不是合法的 ICO，前 6 字节 = %v", icon[:min(6, len(icon))])
	}
	if count := int(icon[4]) | int(icon[5])<<8; count == 0 {
		t.Fatal("ICO 里一个图像都没有")
	}
}
