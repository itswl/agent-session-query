package app

import (
	"fmt"
	"os"
	"sort"
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
	cache map[string]cachedRecords
}

type cachedRecords struct {
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
		cache:    map[string]cachedRecords{},
	}
}

// recordsOf 某个数据源的会话列表（带 TTL 缓存）
func (a *SessionQueryAPI) recordsOf(source SessionSource) []record {
	if a.cacheTTL <= 0 {
		return safeList(source)
	}
	now := time.Now()
	a.mu.Lock()
	if hit, ok := a.cache[source.Mode()]; ok && now.Sub(hit.at) < a.cacheTTL {
		a.mu.Unlock()
		return hit.records
	}
	a.mu.Unlock()

	records := safeList(source)
	a.mu.Lock()
	a.cache[source.Mode()] = cachedRecords{at: now, records: records}
	a.mu.Unlock()
	return records
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

func (a *SessionQueryAPI) listSessions() []map[string]any {
	all := []record{}
	for _, source := range a.sources {
		all = append(all, a.recordsOf(source)...)
	}
	// 按更新时间倒序；相等时保持数据源顺序（稳定排序）
	sort.SliceStable(all, func(i, j int) bool { return all[i].sortKey > all[j].sortKey })

	out := make([]map[string]any, 0, len(all))
	for _, item := range all {
		out = append(out, item.public())
	}
	return out
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
	patternLower := strings.ToLower(pattern)

	var bestSource SessionSource
	var bestRecord record
	bestRank := -1
	for _, source := range a.sources {
		for _, item := range a.recordsOf(source) {
			rank := matchRank(patternLower, item.fields)
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

func (a *SessionQueryAPI) getMessages(pattern string, limit int) ([]map[string]any, bool) {
	source, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}
	return source.Messages(item, limit), true
}

func (a *SessionQueryAPI) getFinalMessage(pattern string) (map[string]any, bool) {
	source, item, ok := a.findSession(pattern)
	if !ok {
		return nil, false
	}

	var result map[string]any
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Fprintf(os.Stderr, "[WARN] %s 解析最终结果失败: %v\n", source.Mode(), rec)
				result = nil
			}
		}()
		result = source.Final(item)
	}()

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

func sourceModes(sources []SessionSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Mode())
	}
	return out
}
