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
	// Messages 返回会话消息；limit 取「最早的前 limit 条」
	Messages(r record, limit int) []map[string]any
	// Final 返回会话最终结果；nil 表示会话文件还不存在（由上层兜底成错误响应）
	Final(r record) map[string]any
}

// 支持的数据源（--mode 可选值）；auto 模式下按存在与否启用
var knownModes = []string{"hermes", "openclaw", "pi", "claude", "codex", "gemini"}

// defaultHome 解析 ~（与 Python 的 Path.home() 相同，读 $HOME）
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
