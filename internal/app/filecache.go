package app

import (
	"os"
	"sync"
	"time"
)

// fileRecordCache 按 (mtime, size) 记忆化「从会话文件头解析出的列表记录」。
//
// 四个基于文件的数据源（Claude / Codex / Pi / Gemini）列会话时，都要打开每个会话文件
// 读头几行拿 sessionId / cwd 这类元数据。这些内容只随文件本身变化，所以 stat 一下就
// 知道上次的结果还能不能用：稳态下 List() 退化成一轮 stat，不再重复解析没变过的文件。
//
// 实测 173 个 Claude 会话：全量解析 52 ms，只 glob + stat 2.4 ms——九成开销花在
// 重复解析上，而页面是 10 秒轮询一次 /sessions，每一次都会踩到。
type fileRecordCache struct {
	mu      sync.Mutex
	entries map[string]fileRecordEntry
}

type fileRecordEntry struct {
	mod  time.Time
	size int64
	rec  record
}

func newFileRecordCache() *fileRecordCache {
	return &fileRecordCache{entries: map[string]fileRecordEntry{}}
}

// records 逐个取 paths 的记录：文件的 mtime/size 都没变就用缓存，
// 变了或没见过才调 build 重新解析。build 收到的 modISO 是该文件 mtime 的 ISO 形式
// （stat 已经做过，数据源不必再 stat 一次）。
//
// 这一轮没出现的路径会被丢掉：会话文件删了，缓存不会一直占着内存。
func (c *fileRecordCache) records(paths []string, build func(path, modISO string) record) []record {
	out := make([]record, 0, len(paths))
	fresh := make(map[string]fileRecordEntry, len(paths))

	c.mu.Lock()
	known := c.entries
	c.mu.Unlock()

	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			continue // 刚被删掉，这一轮就不列了
		}
		mod, size := st.ModTime(), st.Size()

		if hit, ok := known[path]; ok && hit.size == size && hit.mod.Equal(mod) {
			fresh[path] = hit
			out = append(out, hit.rec)
			continue
		}

		rec := build(path, mod.UTC().Format("2006-01-02T15:04:05"))
		fresh[path] = fileRecordEntry{mod: mod, size: size, rec: rec}
		out = append(out, rec)
	}

	c.mu.Lock()
	c.entries = fresh
	c.mu.Unlock()
	return out
}
