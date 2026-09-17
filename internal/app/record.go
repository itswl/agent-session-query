package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// record 是各数据源统一输出的一条会话记录。
//
// fields 是对外字段（source / key / sessionId / file / hasFile / status / updatedAt
// 以及各源的额外字段）；其余都是派生出来的内部数据，不对外出现：
// sortAt / sortKey 用于列表排序，lowerSID / lowerKey 是匹配用的小写形式——
// 都在建记录时算好，查找时不必对每条记录重复做一次 ToLower。
type record struct {
	fields map[string]any

	sortAt   time.Time // 解析出来的更新时间；零值表示这条记录的时间解析不出来
	sortKey  string    // 解析不出时间时的退路：原始字符串
	lowerSID string
	lowerKey string
}

// newRecord 由字段表和「更新时间的原始值」建一条记录。
//
// 各数据源的时间形态不一（mtime 的 2006-01-02T15:04:05、Gemini 的 RFC3339Nano、
// Hermes 里直接来自 sessions.json 的字符串、epoch 数字……），统一在这里解析成
// time.Time 再排序：只按字典序比字符串的话，一个带时区偏移的时间戳就能把跨源排序排错。
func newRecord(fields map[string]any, updatedAt any) record {
	at, _ := parseTimestampValue(updatedAt)
	return record{
		fields:   fields,
		sortAt:   at,
		sortKey:  toStr(updatedAt),
		lowerSID: normalizeForMatch(toStr(fields["sessionId"])),
		lowerKey: normalizeForMatch(toStr(fields["key"])),
	}
}

// normalizeForMatch 把用来匹配的字符串统一成「小写 + 正斜杠」。
//
// 文件型数据源的 key 就是完整路径，分隔符跟着操作系统走——Windows 上是 `\`。
// 不统一的话，同一个 pattern 在 Windows 上会从「后缀精确命中」掉到「子串命中」，
// 而且用户按习惯敲 `proj/abc.jsonl` 根本匹配不上 `...\proj\abc.jsonl`。
func normalizeForMatch(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\\", "/"))
}

func (r record) get(k string) any     { return r.fields[k] }
func (r record) str(k string) string  { return toStr(r.fields[k]) }
func (r record) truthy(k string) bool { return truthy(r.fields[k]) }

// activeWindow 更新时间在这个窗口之内就算「正在跑」
const activeWindow = 2 * time.Minute

// public 返回对外字段的副本。记录会被列表缓存长期持有、并发共享，
// 直接把内部 map 交出去的话，调用方一次无心的赋值就会污染后续所有读者。
//
// isActive 在这里算而不是建记录时算：它跟「现在几点」有关，
// 建好就定死的话，一条十分钟前扫出来的记录会一直说自己是活的。
func (r record) public() map[string]any {
	out := make(map[string]any, len(r.fields)+2)
	for k, v := range r.fields {
		out[k] = v
	}
	out["isActive"] = !r.sortAt.IsZero() && time.Since(r.sortAt) < activeWindow
	out["project"] = r.project()
	return out
}

// project 会话所属的「项目」：优先 cwd（claude / codex / pi），
// 其次 gemini 自带的 project 字段。hermes / openclaw 没有这个维度，返回空串。
func (r record) project() string {
	if cwd := r.str("cwd"); cwd != "" {
		return cwd
	}
	return r.str("project")
}

// newerThan 列表排序用：更新时间晚的排前面。
// 时间解析不出来的记录（sortAt 为零值）一律排在有时间的后面，它们之间按原始字符串倒序。
func (r record) newerThan(other record) bool {
	if !r.sortAt.IsZero() || !other.sortAt.IsZero() {
		if r.sortAt.Equal(other.sortAt) {
			return false // 相等时保持数据源顺序（配合 sort.SliceStable）
		}
		return r.sortAt.After(other.sortAt)
	}
	return r.sortKey > other.sortKey
}

// matchRank 见同名函数；这里用建记录时算好的小写形式。
func (r record) matchRank(patternLower string) int {
	return matchRank(patternLower, r.lowerSID, r.lowerKey)
}

// ---------------------------------------------------------------------------
// 通用工具
// ---------------------------------------------------------------------------

// maxLineBytes 单行上限（Claude Code 的工具输出可能很大）。缓冲按需增长，
// 正常文件只用得到起始的 64 KB；真碰到超过上限的行，Scanner 会中止——
// 注意是「这一行往后都不读了」，不是「跳过这一行」，所以下面要把错误报出来。
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
	// 超长行 / 读取失败会让剩下的内容整段读不到，静默截断比报错更难查
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] 读取 %s 中断，该文件后续内容未解析: %v\n", path, err)
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

// tailWindows 从文件尾部往回找最后一条记录时依次尝试的窗口。
// 实测本机 174 个真实 Claude 会话，172 个在最后 64 KB 里就能找到完整记录。
var tailWindows = []int64{64 << 10, 512 << 10, 4 << 20}

// lastRecordTime 读会话文件的尾部，取最后一条记录自带的时间。
//
// 为什么不直接用文件 mtime：mtime 是「文件被写过」的时间，不是「对话发生」的时间。
// 实测本机 174 个真实 Claude 会话，43 个（25%）两者相差超过 1 小时，最大差 235 小时
// ——有些操作会重写会话文件却不追加新内容，于是六天前聊完的会话被顶到列表最前面，
// 还会被 isActive 误判成「正在写入」。
//
// 不读整个文件：从尾部 seek 一小段就够（最大的那个会话有 103 MB）。窗口里找不到
// 就逐级放大，仍然找不到返回空串，由调用方退回 mtime。
func lastRecordTime(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size == 0 {
		return ""
	}

	for _, window := range tailWindows {
		if window > size {
			window = size
		}
		start := size - window
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return ""
		}
		// 窗口不是从文件头开始时，第一行多半被切在中间，丢掉
		if start > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		if ts := lastTimestampIn(buf); ts != "" {
			return ts
		}
		if window >= size {
			break // 整个文件都读过了，不必再放大
		}
	}
	return ""
}

// lastTimestampIn 在一段字节里从后往前找第一条带时间的记录。
// 三种放法都认：顶层 timestamp（Claude / Codex / Pi）、message.timestamp（Pi 的部分行）、
// $set.lastUpdated（Gemini 的补丁行）。
func lastTimestampIn(buf []byte) string {
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Timestamp string `json:"timestamp"`
			Message   struct {
				Timestamp string `json:"timestamp"`
			} `json:"message"`
			Set struct {
				LastUpdated string `json:"lastUpdated"`
			} `json:"$set"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		for _, ts := range []string{probe.Timestamp, probe.Message.Timestamp, probe.Set.LastUpdated} {
			if ts != "" {
				return ts
			}
		}
	}
	return ""
}

// updatedAtOf 会话的「最后活动时间」：优先用内容里最后一条记录的时间，
// 取不到、或者取到的时间解析不了（解析不了会被排到列表最末尾，比用 mtime 还糟）
// 才退回文件 mtime。
func updatedAtOf(path, modISO string) string {
	ts := lastRecordTime(path)
	if ts == "" {
		return modISO
	}
	if _, ok := parseTimestamp(ts); !ok {
		return modISO
	}
	return ts
}

// mtimeISO 文件修改时间（UTC，秒级，形如 2026-09-14T07:41:48）。
func mtimeISO(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return st.ModTime().UTC().Format("2006-01-02T15:04:05")
}

// matchRank 计算 pattern 与会话记录的匹配度：0 最精确，-1 表示不匹配。
// pattern / sid / key 传进来时都该已经过 normalizeForMatch（小写 + 正斜杠）。
func matchRank(patternLower, sid, key string) int {
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

// truncate 按「字符」（Unicode 码点）截断，超长时在末尾加标记。
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

// truthy 真值判断：nil/false/0/空串/空容器 为假。
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

// toStr 转字符串：字符串原样，布尔输出 True/False，数字不带指数。
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

// strOr 取字符串值，空值时用默认值。
func strOr(v any, def string) string {
	if truthy(v) {
		return toStr(v)
	}
	return def
}

// getOr 键存在就用原值（哪怕是 null），否则用默认值。
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// getMap 取一个 map 字段（缺失或类型不符时返回空 map）。
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

// timeLayouts 各数据源见过的时间形态，按出现频率排（解析时逐个试）。
// 不带时区的按 UTC 解析——各源写文件时用的都是 UTC。
var timeLayouts = []string{
	time.RFC3339Nano,                // 2026-09-14T03:16:50.601Z、带 +08:00 偏移的也认
	"2006-01-02T15:04:05",           // mtime 派生的形态
	"2006-01-02 15:04:05.999999999", // 空格分隔
	"2006-01-02 15:04:05",
}

// parseTimestampValue 把「更新时间」的原始值解析成 UTC 时间。
// 认字符串（上面几种排版，以及纯数字的 epoch）和数字（epoch 秒 / 毫秒）。
func parseTimestampValue(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		return parseTimestamp(t)
	case float64, int, int64:
		sec, _ := toFloat(v)
		return epochToTime(sec)
	}
	return time.Time{}, false
}

func parseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed.UTC(), true
		}
	}
	// 纯数字的 epoch（有的实现把时间戳当字符串写进 JSON）
	if sec, err := strconv.ParseFloat(s, 64); err == nil {
		return epochToTime(sec)
	}
	return time.Time{}, false
}

// epochToTime epoch 秒或毫秒 → UTC 时间。超过 1e11 的按毫秒算
// （1e11 秒是公元 5138 年，1e11 毫秒是 1973 年，这个分界不会误判）。
func epochToTime(v float64) (time.Time, bool) {
	if v == 0 {
		return time.Time{}, false
	}
	if v > 1e11 || v < -1e11 {
		v /= 1000
	}
	if v < -62135596800 || v > 253402300799 {
		return time.Time{}, false
	}
	sec := int64(v)
	nsec := int64((v - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec).UTC(), true
}

// utcFromSeconds 把秒级时间戳格式化成 UTC 的两种形态；
// 越界返回 ok=false。
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
