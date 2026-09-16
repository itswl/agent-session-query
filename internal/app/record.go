package app

import (
	"bufio"
	"bytes"
	"encoding/json"

	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// record 是各数据源统一输出的一条会话记录。
//
// fields 是对外字段（source / key / sessionId / file / hasFile / status / updatedAt
// 以及各源的额外字段）；sortKey 只用于列表排序，不对外出现。
type record struct {
	fields  map[string]any
	sortKey string
}

func (r record) get(k string) any       { return r.fields[k] }
func (r record) str(k string) string    { return toStr(r.fields[k]) }
func (r record) truthy(k string) bool   { return truthy(r.fields[k]) }
func (r record) public() map[string]any { return r.fields }

// ---------------------------------------------------------------------------
// 通用工具
// ---------------------------------------------------------------------------

// maxLineBytes 单行上限（Claude Code 的工具输出可能很大；超过就跳过该行）
const maxLineBytes = 256 * 1024 * 1024

// eachJSONLLine 逐行读原始字节，回调里的切片只在本次调用内有效（缓冲会被复用），
// 需要留用必须自己拷贝。用 Scanner 而不是 ReadBytes：Scanner.Bytes() 是内部缓冲的
// 视图，每行不额外分配，实测读完 3000 行的大会话比逐行 ReadBytes 快约 40%。
func eachJSONLLine(path string, fn func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !fn(line) {
			return
		}
	}
}

// eachJSONL 逐行解析出对象（坏行跳过、非对象跳过）。fn 返回 false 表示提前停止。
func eachJSONL(path string, fn func(obj map[string]any) bool) {
	eachJSONLLine(path, func(line []byte) bool {
		var obj map[string]any
		if json.Unmarshal(line, &obj) == nil && obj != nil {
			return fn(obj)
		}
		return true
	})
}

// readJSONL 逐行读 jsonl，最多读 limit 条有效记录（limit <= 0 表示不限）。
func readJSONL(path string, limit int) []map[string]any {
	out := []map[string]any{}
	eachJSONL(path, func(obj map[string]any) bool {
		if limit > 0 && len(out) >= limit {
			return false
		}
		out = append(out, obj)
		return true
	})
	return out
}

// contentText 把各种形态的 content 收敛成纯文本（字符串 / 数组 / 单个字典）。
func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		for _, key := range []string{"text", "content", "thinking"} {
			inner, ok := v[key]
			if !ok {
				continue
			}
			switch t := inner.(type) {
			case string:
				return t
			case []any:
				return contentText(t)
			}
		}
		return ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := contentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// mtimeISO 文件修改时间（UTC，秒级），与 Python 版的 %Y-%m-%dT%H:%M:%S 一致。
func mtimeISO(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return st.ModTime().UTC().Format("2006-01-02T15:04:05")
}

// matchRank 计算 pattern 与会话记录的匹配度：0 最精确，-1 表示不匹配。
func matchRank(patternLower string, f map[string]any) int {
	sid := strings.ToLower(toStr(f["sessionId"]))
	key := strings.ToLower(toStr(f["key"]))
	if sid != "" && patternLower == sid {
		return 0
	}
	if key == patternLower {
		return 1
	}
	if strings.HasSuffix(key, ":"+patternLower) || strings.HasSuffix(key, "/"+patternLower) {
		return 2
	}
	if strings.Contains(key, patternLower) {
		return 3
	}
	if sid != "" && strings.Contains(sid, patternLower) {
		return 4
	}
	return -1
}

// truncate 按「字符」（Unicode 码点）截断，与 Python 的 len()/切片语义一致；
// 超长时在末尾加标记。
func truncate(text string, limit int, mark string) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	count := 0
	for i := range text {
		if count == limit {
			return text[:i] + mark
		}
		count++
	}
	return text + mark
}

// truthy 对应 Python 的真值判断：None/False/0/""/空容器 为假。
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// toStr 近似 Python 的 str()：字符串原样，数字不带指数，其余用字面量。
func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// strOr 对应 Python 的 `x or default`。
func strOr(v any, def string) string {
	if truthy(v) {
		return toStr(v)
	}
	return def
}

// getOr 对应 Python 的 `m.get(key, default)`：键存在就用原值（哪怕是 null），否则用默认值。
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// getMap 取一个 map 字段（缺失或类型不符时返回空 map，与 Python 的 `or {}` 一致）。
func getMap(m map[string]any, key string) map[string]any {
	if inner, ok := m[key].(map[string]any); ok && inner != nil {
		return inner
	}
	return map[string]any{}
}

// getSlice 取一个数组字段（缺失或类型不符时返回 nil）。
func getSlice(m map[string]any, key string) []any {
	if inner, ok := m[key].([]any); ok {
		return inner
	}
	return nil
}

// strField 取字符串字段（非字符串返回空串）。
func strField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// utcFromSeconds 把秒级时间戳格式化成 UTC 的两种形态；
// 越界（对应 Python fromtimestamp 抛异常）返回 ok=false。
func utcFromSeconds(sec float64) (dashed string, iso string, ok bool) {
	whole := int64(sec)
	if sec < 0 && float64(whole) != sec {
		whole-- // 与 time.Unix 的取整方向对齐
	}
	if whole < -62135596800 || whole > 253402300799 {
		return "", "", false
	}
	t := time.Unix(whole, 0).UTC()
	return t.Format("2006-01-02 15:04:05"), t.Format("2006-01-02T15:04:05"), true
}
