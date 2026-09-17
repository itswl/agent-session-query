package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// SessionSource 是数据源适配器：统一 list / messages / final 三个动作。
type SessionSource interface {
	Mode() string
	Location() string // 启动时打印、缺失时提示用
	Exists() bool
	List() []record
	// Messages 返回会话消息，取哪一段由 messageQuery 决定
	Messages(r record, q messageQuery) []map[string]any
	// Final 返回会话最终结果；nil 表示会话文件还不存在（由上层兜底成错误响应）
	Final(r record) map[string]any
}

// messageQuery 描述一次消息查询：取多少条、从哪一头取。
//
// fromEnd 对应 ?order=desc。会话最有价值的往往是结尾，而只能取「最早 N 条」的话，
// 一个上万条消息的会话在页面上永远只看得到开头。
type messageQuery struct {
	limit   int
	fromEnd bool
}

// messageSink 按 messageQuery 收集消息。
//
// 取最早 N 条：攒够就叫停（add 返回 false），扫描提前结束。
// 取最新 N 条：必须一路扫到文件末尾，所以用一个容量 limit 的环形缓冲——
// 内存只跟 limit 走，不跟会话长度走（16444 条消息的会话也只留住最后 N 条）。
type messageSink struct {
	q     messageQuery
	items []map[string]any
	start int // 环形缓冲的写入位置（只有 fromEnd 用得到）
}

func newMessageSink(q messageQuery) *messageSink {
	if q.limit < 0 {
		q.limit = 0
	}
	capacity := q.limit
	if capacity > 512 {
		capacity = 512 // 别为一个 limit=1000 的请求先占住一整块
	}
	return &messageSink{q: q, items: make([]map[string]any, 0, capacity)}
}

// add 收下一条消息；返回 false 表示够了，调用方可以停止扫描。
func (s *messageSink) add(m map[string]any) bool {
	if s.q.limit == 0 {
		return false
	}
	if !s.q.fromEnd {
		s.items = append(s.items, m)
		return len(s.items) < s.q.limit
	}
	if len(s.items) < s.q.limit {
		s.items = append(s.items, m)
		return true
	}
	s.items[s.start] = m
	s.start = (s.start + 1) % s.q.limit
	return true
}

// result 按时间先后顺序返回收集到的消息
func (s *messageSink) result() []map[string]any {
	if len(s.items) == 0 {
		return []map[string]any{}
	}
	if !s.q.fromEnd || s.start == 0 {
		return s.items
	}
	out := make([]map[string]any, 0, len(s.items))
	out = append(out, s.items[s.start:]...)
	out = append(out, s.items[:s.start]...)
	return out
}

// 支持的数据源（--mode 可选值）；auto 模式下按存在与否启用
var knownModes = []string{"hermes", "openclaw", "pi", "claude", "codex", "gemini"}

// fileExists 路径是否存在
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// defaultHome 读 $HOME 解析 ~
func defaultHome() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/root"
}

// buildSources 按运行模式装配数据源。
//
//   - 指定单个模式：只启用它
//   - all：六个都启用（缺的会警告）
//   - auto：存在的数据源都启用（两个 json-map 源看 sessions.json，其它看目录）
func buildSources(mode string) ([]SessionSource, error) {
	home := defaultHome()

	factories := map[string]func() SessionSource{
		"hermes":   func() SessionSource { return newJsonMapSource(hermesDef(home)) },
		"openclaw": func() SessionSource { return newJsonMapSource(openclawDef(home)) },
		"pi":       func() SessionSource { return newPiSource(filepath.Join(home, ".pi", "agent", "sessions")) },
		"claude":   func() SessionSource { return newClaudeSource(filepath.Join(home, ".claude", "projects")) },
		"codex":    func() SessionSource { return newCodexSource(filepath.Join(home, ".codex", "sessions")) },
		"gemini":   func() SessionSource { return newGeminiSource(filepath.Join(home, ".gemini", "tmp")) },
	}

	if mode != "auto" && mode != "all" {
		factory, ok := factories[mode]
		if !ok {
			return nil, fmt.Errorf("未知模式 %q（可选: auto, all, %s）", mode, joinModes())
		}
		return []SessionSource{factory()}, nil
	}

	enabled := []SessionSource{}
	for _, name := range knownModes {
		source := factories[name]()
		if source.Exists() {
			enabled = append(enabled, source)
		} else if mode == "all" {
			fmt.Fprintf(os.Stderr, "警告: 数据源不存在，已跳过: %s (%s)\n", name, source.Location())
		}
	}
	if len(enabled) > 0 {
		return enabled, nil
	}

	fmt.Fprintln(os.Stderr, "警告: 未检测到任何数据源，默认使用 OpenClaw")
	return []SessionSource{factories["openclaw"]()}, nil
}

func joinModes() string {
	out := ""
	for i, m := range knownModes {
		if i > 0 {
			out += "/"
		}
		out += m
	}
	return out
}

// jsonMapDef 描述「一个 sessions.json 索引 + 每会话一个 jsonl」形态的数据源。
// stateDB 非 empty 时（仅 hermes）：sessions.json 不存在也能从 SQLite 列会话。
type jsonMapDef struct {
	mode            string
	sessionsJSON    string
	sessionsDir     string
	sessionIDField  string
	stopReasonField string
	stateDB         string
}

func hermesDef(home string) jsonMapDef {
	return jsonMapDef{
		mode:            "hermes",
		sessionsJSON:    filepath.Join(home, ".hermes", "sessions", "sessions.json"),
		sessionsDir:     filepath.Join(home, ".hermes", "sessions"),
		sessionIDField:  "session_id",
		stopReasonField: "finish_reason",
		stateDB:         filepath.Join(home, ".hermes", "state.db"),
	}
}

func openclawDef(home string) jsonMapDef {
	return jsonMapDef{
		mode:            "openclaw",
		sessionsJSON:    filepath.Join(home, ".openclaw", "agents", "default", "sessions", "sessions.json"),
		sessionsDir:     filepath.Join(home, ".openclaw", "agents", "default", "sessions"),
		sessionIDField:  "sessionId",
		stopReasonField: "stopReason",
	}
}
