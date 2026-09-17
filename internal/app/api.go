package app

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionQueryAPI 把请求分发到各个数据源适配器。
//
// 会话列表按数据源做短 TTL 缓存（默认 2 秒，--cache-ttl 可调，0 = 不缓存）：
// 查找会话、列表都用它，所以一次请求不必把所有会话文件重新扫一遍。
// 消息与最终结果始终直接读文件，缓存只影响「有哪些会话」这层元数据。
type SessionQueryAPI struct {
	sources  []SessionSource
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]*cachedRecords
}

// cachedRecords 一个数据源的列表缓存。
//
// scan 保证同一数据源同一时刻只有一次扫描：缓存刚过期时若干请求同时打进来，
// 各扫一遍目录是纯浪费（cache stampede）——现在是一个去扫、其余等它的结果。
type cachedRecords struct {
	scan    sync.Mutex
	at      time.Time
	records []record
}

func newSessionQueryAPI(sources []SessionSource, cacheTTLSeconds float64) *SessionQueryAPI {
	if cacheTTLSeconds < 0 {
		cacheTTLSeconds = 0
	}
	return &SessionQueryAPI{
		sources:  sources,
		cacheTTL: time.Duration(cacheTTLSeconds * float64(time.Second)),
		cache:    map[string]*cachedRecords{},
	}
}

// recordsOf 某个数据源的会话列表（带 TTL 缓存）
func (a *SessionQueryAPI) recordsOf(source SessionSource) []record {
	if a.cacheTTL <= 0 {
		return safeList(source)
	}

	a.mu.Lock()
	entry, ok := a.cache[source.Mode()]
	if !ok {
		entry = &cachedRecords{}
		a.cache[source.Mode()] = entry
	}
	a.mu.Unlock()

	entry.scan.Lock()
	defer entry.scan.Unlock()
	if !entry.at.IsZero() && time.Since(entry.at) < a.cacheTTL {
		return entry.records
	}
	entry.records = safeList(source)
	entry.at = time.Now()
	return entry.records
}

// safeList 单个数据源出错时不拖垮整个列表
func safeList(source SessionSource) (out []record) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %s 列出会话失败: %v\n", source.Mode(), rec)
			out = nil
		}
	}()
	return source.List()
}

// listSessions 多源合并后的会话列表，以及一个弱校验值（ETag 用）。
//
// 页面每 10 秒轮询一次，绝大多数时候列表根本没变——带上 ETag 之后
// 这些轮询在 304 就结束了，不必每次把整个列表再序列化、再传一遍。
func (a *SessionQueryAPI) listSessions() ([]map[string]any, string) {
	all := []record{}
	for _, source := range a.sources {
		all = append(all, a.recordsOf(source)...)
	}
	// 按更新时间倒序；相等时保持数据源顺序（稳定排序）
	sort.SliceStable(all, func(i, j int) bool { return all[i].newerThan(all[j]) })

	out := make([]map[string]any, 0, len(all))
	for _, item := range all {
		out = append(out, item.public())
	}
	return out, listVersion(all)
}

// listVersion 列表的弱校验值：成员、更新时间、状态任一变化都会让它变。
func listVersion(records []record) string {
	h := fnv.New64a()
	for _, r := range records {
		for _, field := range []string{"source", "key", "updatedAt", "status"} {
			_, _ = h.Write([]byte(r.str(field)))
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte{0x1e})
	}
	return `W/"` + strconv.FormatUint(h.Sum64(), 16) + `"`
}

// findSession 在所有启用的数据源里找最佳匹配（精确优先，跨源不互相遮蔽）
func (a *SessionQueryAPI) findSession(pattern string) (SessionSource, record, bool) {
	pattern = strings.TrimSpace(pattern)
	if strings.HasPrefix(pattern, "Run: ") {
		pattern = pattern[len("Run: "):]
	}
	if strings.HasPrefix(pattern, "Session: ") {
		pattern = pattern[len("Session: "):]
	}
	if pattern == "" {
		return nil, record{}, false
	}
	patternLower := normalizeForMatch(pattern)

	var bestSource SessionSource
	var bestRecord record
	bestRank := -1
	for _, source := range a.sources {
		for _, item := range a.recordsOf(source) {
			rank := item.matchRank(patternLower)
			if rank == -1 {
				continue
			}
			if bestRank == -1 || rank < bestRank {
				bestSource, bestRecord, bestRank = source, item, rank
			}
		}
	}
	if bestSource == nil {
		return nil, record{}, false
	}
	return bestSource, bestRecord, true
}

func (a *SessionQueryAPI) getSession(pattern string) (map[string]any, bool) {
	_, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}
	return item.public(), true
}

// safeParse 单个会话解析炸了不至于把请求变成 500：记一笔，按「没解析出来」处理。
// 会话文件是外部写的，格式随时可能变——messages 和 final 都走这层。
func safeParse[T any](mode, what string, parse func() T) (out T) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %s 解析%s失败: %v\n", mode, what, rec)
			var zero T
			out = zero
		}
	}()
	return parse()
}

func (a *SessionQueryAPI) getMessages(pattern string, q messageQuery) ([]map[string]any, bool) {
	source, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}
	messages := safeParse(source.Mode(), "消息", func() []map[string]any {
		return source.Messages(item, q)
	})
	if messages == nil {
		messages = []map[string]any{}
	}
	return messages, true
}

func (a *SessionQueryAPI) getFinalMessage(pattern string) (map[string]any, bool) {
	source, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}

	result := safeParse(source.Mode(), "最终结果", func() map[string]any {
		return source.Final(item)
	})

	if result == nil {
		status := item.str("status")
		if status == "" {
			status = "unknown"
		}
		result = map[string]any{
			"status":       status,
			"isFinal":      false,
			"isProcessing": status == "running",
			"messageCount": 0,
			"source":       item.get("source"),
			"error":        "Session file not available yet (session may be still initializing)",
		}
	}
	return result, true
}

// listProjects 把会话按项目归拢。
//
// 这是这个工具唯一能做、别的工具做不了的事：同一个仓库上，你用 Claude Code、
// Codex、Gemini 分别干过什么，在这里是一个视图。
// 注意 hermes / openclaw 没有 cwd 也没有 project，会落到 ungrouped 里。
func (a *SessionQueryAPI) listProjects() ([]map[string]any, int) {
	type bucket struct {
		sessions int
		sources  map[string]int
		latest   record
	}
	order := []string{}
	buckets := map[string]*bucket{}
	ungrouped := 0

	for _, source := range a.sources {
		for _, rec := range a.recordsOf(source) {
			name := rec.project()
			if name == "" {
				ungrouped++
				continue
			}
			b, ok := buckets[name]
			if !ok {
				b = &bucket{sources: map[string]int{}}
				buckets[name] = b
				order = append(order, name)
			}
			b.sessions++
			b.sources[rec.str("source")]++
			if b.latest.fields == nil || rec.newerThan(b.latest) {
				b.latest = rec
			}
		}
	}

	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		b := buckets[name]
		sources := make([]string, 0, len(b.sources))
		for mode := range b.sources {
			sources = append(sources, mode)
		}
		sort.Strings(sources)
		out = append(out, map[string]any{
			"project":       name,
			"shortName":     filepath.Base(name),
			"sessions":      b.sessions,
			"sources":       sources,
			"sourceCounts":  b.sources,
			"updatedAt":     b.latest.str("updatedAt"),
			"latestSession": b.latest.str("sessionId"),
			"isActive":      !b.latest.sortAt.IsZero() && time.Since(b.latest.sortAt) < activeWindow,
		})
	}
	// 最近动过的项目排前面
	sort.SliceStable(out, func(i, j int) bool {
		return toStr(out[i]["updatedAt"]) > toStr(out[j]["updatedAt"])
	})
	return out, ungrouped
}

func sourceModes(sources []SessionSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Mode())
	}
	return out
}
