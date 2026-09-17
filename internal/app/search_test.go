package app

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexFold(t *testing.T) {
	cases := []struct {
		text, needle string
		want         int
	}{
		{"Hello World", "world", 6},
		{"HELLO", "hello", 0},
		{"nginx.conf", "NGINX", -1}, // needle 必须已经是小写，调用方负责
		{"配置 Nginx 反代", "nginx", 7}, // 「配置 」是 7 个字节
		{"abc", "", 0},
		{"abc", "abcd", -1},
	}
	for _, c := range cases {
		if got := indexFold(c.text, c.needle); got != c.want {
			t.Errorf("indexFold(%q, %q) = %d, want %d", c.text, c.needle, got, c.want)
		}
	}
}

func TestSnippetAround(t *testing.T) {
	// 命中在正中间：两头都该有省略号，且不能把 UTF-8 切碎
	long := strings.Repeat("一二三四五", 60) + "关键词" + strings.Repeat("六七八九十", 60)
	got := snippetAround(long, "关键词", 10)
	if !strings.Contains(got, "关键词") {
		t.Fatalf("片段里没有命中词: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("两头都被截了却没有省略标记: %q", got)
	}
	if !utf8Valid(got) {
		t.Fatalf("片段把 UTF-8 切碎了: %q", got)
	}
	if n := len([]rune(got)); n > 40 {
		t.Fatalf("片段太长: %d 字符", n)
	}

	// 短文本原样返回，不加省略号
	if got := snippetAround("就这么短", "这么", 20); got != "就这么短" {
		t.Fatalf("短文本 = %q", got)
	}
	// 命中在开头：只有尾部该有省略号
	head := snippetAround("开头命中"+strings.Repeat("填充", 100), "开头", 5)
	if strings.HasPrefix(head, "…") || !strings.HasSuffix(head, "…") {
		t.Fatalf("命中在开头时 = %q", head)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestFindMatchingTextPrefersBody(t *testing.T) {
	// 命中同时出现在字段名和正文里时，要返回正文
	obj := map[string]any{
		"snippetish": "无关",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "改了 nginx 的超时"}},
		},
	}
	got, ok := findMatchingText(obj, "nginx", 0)
	if !ok || got != "改了 nginx 的超时" {
		t.Fatalf("findMatchingText = %q %v", got, ok)
	}
	// 只在键名里出现，不算命中
	if _, ok := findMatchingText(map[string]any{"nginx": 1}, "nginx", 0); ok {
		t.Fatal("键名不该算命中")
	}
}

func newSearchServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-00_a.jsonl"),
		`{"type":"session","id":"s-hit","cwd":"/w/a"}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"帮我配一下 Nginx 反向代理"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"nginx 的 proxy_pass 要这么写"}]}}`,
		`{"type":"message","id":"m3","message":{"role":"assistant","content":[{"type":"text","text":"第三处 NGINX 大写"}]}}`,
	)
	write(t, filepath.Join(root, "p", "2026-01-01T00-00-01_b.jsonl"),
		`{"type":"session","id":"s-miss","cwd":"/w/b"}`,
		`{"type":"message","id":"n1","message":{"role":"user","content":[{"type":"text","text":"完全无关的内容"}]}}`,
	)
	sources := []SessionSource{newPiSource(root)}
	srv := httptest.NewServer(newAPIServer(serverOptions{
		mode: "auto", sources: sources, api: newSessionQueryAPI(sources, 2), maxConnections: 50,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPSearch(t *testing.T) {
	srv := newSearchServer(t)

	code, body := get(t, srv.URL+"/search?q=nginx", "")
	if code != 200 {
		t.Fatalf("/search = %d", code)
	}
	if body["total"] != float64(1) || body["scanned"] != float64(2) {
		t.Fatalf("应当扫 2 个会话、命中 1 个: %v", body)
	}
	hit := body["results"].([]any)[0].(map[string]any)
	if hit["sessionId"] != "s-hit" {
		t.Fatalf("命中的会话不对: %v", hit["sessionId"])
	}
	// 大小写无关：三条都该命中（Nginx / nginx / NGINX）
	if hit["matchCount"] != float64(3) {
		t.Fatalf("matchCount = %v，大小写应当无关", hit["matchCount"])
	}
	matches := hit["matches"].([]any)
	first := matches[0].(map[string]any)
	if !strings.Contains(first["snippet"].(string), "Nginx") || first["role"] != "user" {
		t.Fatalf("第一条命中 = %v", first)
	}

	// per_session 限制每个会话返回几条
	if _, body := get(t, srv.URL+"/search?q=nginx&per_session=1", ""); body["results"].([]any)[0].(map[string]any)["matchCount"] != float64(1) {
		t.Fatalf("per_session=1 未生效: %v", body)
	}
	// limit 限制返回几个会话，但 matched 仍报真实总数
	code, body = get(t, srv.URL+"/search?q=nginx&limit=0", "")
	if body["total"] != float64(0) || body["matched"] != float64(1) || body["truncated"] != true {
		t.Fatalf("limit 截断后应仍报 matched: %v", body)
	}
	// 搜不到
	if _, body := get(t, srv.URL+"/search?q=绝不会出现的词", ""); body["total"] != float64(0) || body["truncated"] != false {
		t.Fatalf("无命中 = %v", body)
	}
	// q 必填
	if code, _ := get(t, srv.URL+"/search", ""); code != 400 {
		t.Fatalf("缺 q 应当 400，得到 %d", code)
	}
}

func TestHTTPSearchNeedsAuth(t *testing.T) {
	// /search 返回会话正文，必须和 /sessions 一样要认证
	srv, _ := newTestServer(t, "secret")
	if code, _ := get(t, srv.URL+"/search?q=x", ""); code != 401 {
		t.Fatalf("无 token 搜索应当 401，得到 %d", code)
	}
	if code, _ := get(t, srv.URL+"/search?q=问题", "secret"); code != 200 {
		t.Fatalf("带 token 搜索 = %d", code)
	}
}

func TestParseSince(t *testing.T) {
	for _, raw := range []string{"30d", "12h", "90m", "2026-09-01", "2026-09-01T10:00:00Z"} {
		if _, err := parseSince(raw); err != nil {
			t.Errorf("parseSince(%q) 报错: %v", raw, err)
		}
	}
	for _, raw := range []string{"zzz", "", "-5x"} {
		if _, err := parseSince(raw); err == nil {
			t.Errorf("parseSince(%q) 应当报错", raw)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	// 搜 "100%" 不该变成匹配任意串
	if got := escapeLike("100%"); got != `100\%` {
		t.Fatalf("escapeLike = %q", got)
	}
	if got := escapeLike("a_b"); got != `a\_b` {
		t.Fatalf("escapeLike = %q", got)
	}
}
