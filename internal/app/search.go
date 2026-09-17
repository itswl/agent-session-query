package app

import (
	"bytes"
	"encoding/json"
	"runtime"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// 内容搜索。
//
// 不建索引：索引意味着要落盘、要维护、要考虑失效，「单二进制、只读、scp 过去就能跑」
// 这条就没了。实测本机 470 MB / 173 个会话裸扫一遍不到 1 秒，够用。
//
// 快在于顺序：先拿原始字节做大小写无关的 Contains，命中了才 JSON 解析那一行——
// 99% 的行连解析都省掉，而 JSON 解析才是扫描里最贵的部分。
const (
	defaultSearchLimit      = 30 // 最多返回多少个会话
	defaultSearchPerSession = 3  // 每个会话最多返回几条命中
	searchSnippetRadius     = 70 // 片段里命中处前后各留多少个字符
	maxSearchDepth          = 8  // JSON 里找正文时的递归深度上限
)

// searchQuery 一次内容搜索。
type searchQuery struct {
	needle     string    // 原样保留，用于回显
	lowered    []byte    // 小写后的待查字节（ASCII 折叠）
	limit      int       // 最多返回多少个会话
	perSession int       // 每个会话最多返回几条命中
	since      time.Time // 只搜更新时间晚于此的会话；零值 = 不限
}

// searchableSource 数据源可以自己实现内容搜索。
// 没实现的走通用路径：扫会话文件（绝大多数源都是一个会话一个 jsonl）。
type searchableSource interface {
	Search(r record, q searchQuery) []map[string]any
}

// searchOutcome 一次搜索的结果与规模。
// scanned / matched 分开报：截断到 limit 之后，用户得知道「还有多少没给你」。
type searchOutcome struct {
	results []map[string]any
	scanned int // 实际扫过的会话数
	matched int // 有命中的会话数，可能多于 len(results)
}

// search 在所有启用的数据源里搜内容，按会话更新时间倒序返回。
func (a *SessionQueryAPI) search(q searchQuery) searchOutcome {
	type candidate struct {
		source SessionSource
		rec    record
	}

	all := []candidate{}
	for _, source := range a.sources {
		for _, rec := range a.recordsOf(source) {
			if !q.since.IsZero() && (rec.sortAt.IsZero() || rec.sortAt.Before(q.since)) {
				continue
			}
			all = append(all, candidate{source: source, rec: rec})
		}
	}
	// 新的排前面：截断到 limit 时留下的是最近的
	sort.SliceStable(all, func(i, j int) bool { return all[i].rec.newerThan(all[j].rec) })

	hits := make([][]map[string]any, len(all))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(all) {
		workers = len(all)
	}
	if workers > 0 {
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					c := all[i]
					hits[i] = safeParse(c.source.Mode(), "搜索", func() []map[string]any {
						return searchOne(c.source, c.rec, q)
					})
				}
			}()
		}
		for i := range all {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}

	out := searchOutcome{results: []map[string]any{}, scanned: len(all)}
	for i, matches := range hits {
		if len(matches) == 0 {
			continue
		}
		out.matched++
		if len(out.results) >= q.limit {
			continue // 还要继续数 matched，好告诉用户被截了多少
		}
		item := all[i].rec.public()
		item["matches"] = matches
		item["matchCount"] = len(matches)
		out.results = append(out.results, item)
	}
	return out
}

// searchOne 在一个会话里找。数据源自己实现了就用它的，否则扫会话文件。
func searchOne(source SessionSource, rec record, q searchQuery) []map[string]any {
	if s, ok := source.(searchableSource); ok {
		return s.Search(rec, q)
	}
	return searchFile(rec.str("file"), q)
}

// searchFile 扫一个 jsonl 会话文件。
func searchFile(path string, q searchQuery) []map[string]any {
	if path == "" || len(q.lowered) == 0 {
		return nil
	}
	var hits []map[string]any
	var lower []byte // 复用，避免每行都分配
	eachJSONLLine(path, func(line []byte) bool {
		lower = appendLowerASCII(lower[:0], line)
		if !bytes.Contains(lower, q.lowered) {
			return true
		}
		if hit := buildHit(line, q); hit != nil {
			hits = append(hits, hit)
		}
		return len(hits) < q.perSession
	})
	return hits
}

// buildHit 把命中的那一行变成一条结果；命中只落在字段名或转义序列里时返回 nil。
func buildHit(line []byte, q searchQuery) map[string]any {
	var obj map[string]any
	if json.Unmarshal(line, &obj) != nil || obj == nil {
		return nil
	}
	text, ok := findMatchingText(obj, string(q.lowered), 0)
	if !ok {
		return nil
	}
	return map[string]any{
		"snippet":   snippetAround(text, string(q.lowered), searchSnippetRadius),
		"role":      hitRole(obj),
		"timestamp": strOr(obj["timestamp"], ""),
	}
}

// textFieldOrder 正文最可能待的字段，按这个顺序先找——
// map 遍历在 Go 里是乱序的，不定个顺序的话同一个查询每次给出的片段都可能不一样。
var textFieldOrder = []string{"text", "content", "thinking", "reasoning", "message", "payload"}

func findMatchingText(v any, needleLower string, depth int) (string, bool) {
	if depth > maxSearchDepth {
		return "", false
	}
	switch t := v.(type) {
	case string:
		if indexFold(t, needleLower) >= 0 {
			return t, true
		}
	case []any:
		for _, item := range t {
			if s, ok := findMatchingText(item, needleLower, depth+1); ok {
				return s, true
			}
		}
	case map[string]any:
		for _, key := range textFieldOrder {
			if inner, has := t[key]; has {
				if s, ok := findMatchingText(inner, needleLower, depth+1); ok {
					return s, true
				}
			}
		}
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sort.Strings(keys) // 其余字段按键名排序，保证结果稳定
		for _, key := range keys {
			if s, ok := findMatchingText(t[key], needleLower, depth+1); ok {
				return s, true
			}
		}
	}
	return "", false
}

// hitRole 尽力取出这条命中的角色（各源放的位置不一样）
func hitRole(obj map[string]any) string {
	if role := strOr(obj["role"], ""); role != "" {
		return role
	}
	for _, key := range []string{"message", "payload"} {
		if inner, ok := obj[key].(map[string]any); ok {
			if role := strOr(inner["role"], ""); role != "" {
				return role
			}
		}
	}
	return strOr(obj["type"], "")
}

// appendLowerASCII 把 src 里的 A-Z 折成小写追加到 dst。
// 按字节处理，UTF-8 的多字节序列（首字节 >= 0x80）原样穿过，中文本来也没有大小写。
func appendLowerASCII(dst, src []byte) []byte {
	for _, c := range src {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}

// indexFold 大小写无关地找 needleLower（needleLower 必须已经是小写），
// 找不到返回 -1。不预先复制整个 s，省掉长行上的一次分配。
func indexFold(s, needleLower string) int {
	n := len(needleLower)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		hit := true
		for j := 0; j < n; j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needleLower[j] {
				hit = false
				break
			}
		}
		if hit {
			return i
		}
	}
	return -1
}

// snippetAround 截出命中处前后各 radius 个字符，两头有省略时加标记。
func snippetAround(text, needleLower string, radius int) string {
	idx := indexFold(text, needleLower)
	if idx < 0 {
		return truncate(text, radius*2, "…")
	}

	// 先按字节开一个宽窗（UTF-8 最多 4 字节一个字符），再收到字符数
	lo, hi := idx-radius*4, idx+len(needleLower)+radius*4
	if lo < 0 {
		lo = 0
	}
	if hi > len(text) {
		hi = len(text)
	}
	for lo > 0 && !utf8.RuneStart(text[lo]) { // 对齐到字符边界
		lo--
	}
	for hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi++
	}

	head := trimRunesFromLeft(text[lo:idx], radius)
	tail := trimRunesFromRight(text[idx:hi], radius+utf8.RuneCountInString(needleLower))
	out := head + tail
	if lo > 0 || len(head) < idx-lo {
		out = "…" + out
	}
	if hi < len(text) || len(tail) < hi-idx {
		out += "…"
	}
	return out
}

// trimRunesFromLeft 只保留末尾 n 个字符
func trimRunesFromLeft(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	drop := utf8.RuneCountInString(s) - n
	for i := range s {
		if drop == 0 {
			return s[i:]
		}
		drop--
	}
	return ""
}

// trimRunesFromRight 只保留开头 n 个字符
func trimRunesFromRight(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
