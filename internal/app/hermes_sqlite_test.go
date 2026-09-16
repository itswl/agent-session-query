package app

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// makeHermesDB 造一个与 Hermes 同形的 state.db
func makeHermesDB(t *testing.T, dbPath string, schema string, statements []string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range append([]string{schema}, statements...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("执行 %q 失败: %v", stmt, err)
		}
	}
}

const hermesSchema = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, message_count INTEGER,
	input_tokens INTEGER, output_tokens INTEGER,
	cache_read_tokens INTEGER, cache_write_tokens INTEGER,
	reasoning_tokens INTEGER, estimated_cost_usd REAL,
	session_key TEXT, display_name TEXT, source TEXT, model TEXT,
	started_at REAL, ended_at REAL
);
CREATE TABLE messages (
	id TEXT, session_id TEXT, role TEXT, content TEXT,
	finish_reason TEXT, reasoning TEXT, timestamp REAL,
	active INTEGER DEFAULT 1
);`

func newHermesFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "state.db")
}

func TestHermesSQLiteFinal(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO sessions (id, message_count, input_tokens, output_tokens, cache_read_tokens,
			cache_write_tokens, reasoning_tokens, estimated_cost_usd)
		 VALUES ('h-webhook', 12, 1500, 220, 30, 40, 7, 0.0123)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m1', 'h-webhook', 'assistant', '先看的这条', 'stop', '推理一', '2026-09-13T10:00:00Z', 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m2', 'h-webhook', 'assistant', '最终答案', 'stop', '推理二', '2026-09-13T10:05:00Z', 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m3', 'h-webhook', 'assistant', '被软删了', 'stop', '推理三', '2026-09-13T10:09:00Z', 0)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('m4', 'h-webhook', 'user', '用户消息', NULL, NULL, '2026-09-13T09:59:00Z', 1)`,
	})

	result := hermesSQLiteFinal("hermes", "h-webhook", "done")
	if result == nil {
		t.Fatal("应当从 state.db 取到最终消息")
	}
	if result["text"] != "最终答案" || result["thinking"] != "推理二" {
		t.Fatalf("取到的不是最后一条（active=1 的）助手消息: %v", result)
	}
	if result["isFinal"] != true || result["isProcessing"] != false || result["messageCount"] != int64(12) {
		t.Fatalf("终态字段不对: %v", result)
	}
	if result["stopReason"] != "stop" || result["source"] != "hermes" || result["timestamp"] != "2026-09-13T10:05:00Z" {
		t.Fatalf("字段不对: %v", result)
	}
	usage := result["usage"].(map[string]any)
	if usage["inputTokens"] != int64(1500) || usage["estimatedCostUsd"] != 0.0123 || usage["reasoningTokens"] != int64(7) {
		t.Fatalf("usage 不对: %v", usage)
	}

	// 没在 sessions 表里登记时：usage 为空对象，message_count 回退成实际条数
	makeHermesDB(t, dbPath, "", []string{
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('n1', 'h-count', 'assistant', '靠计数', 'stop', NULL, '2026-09-14T10:00:00Z', 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('n2', 'h-count', 'user', '提问', NULL, NULL, '2026-09-14T09:59:00Z', 1)`,
	})
	second := hermesSQLiteFinal("hermes", "h-count", "done")
	if second == nil || second["messageCount"] != int64(2) {
		t.Fatalf("message_count 应回退成 2: %v", second)
	}
	if len(second["usage"].(map[string]any)) != 0 {
		t.Fatalf("没有 sessions 记录时 usage 应为空: %v", second["usage"])
	}

	// 没有 finish_reason=stop 的助手消息 → nil，交给上层兜底
	if got := hermesSQLiteFinal("hermes", "h-empty", "done"); got != nil {
		t.Fatalf("查不到应当返回 nil: %v", got)
	}
	// 非 hermes 源、空 session id、库不存在，都不走这条路径
	if got := hermesSQLiteFinal("openclaw", "h-webhook", "done"); got != nil {
		t.Fatalf("openclaw 不应走 sqlite: %v", got)
	}
	t.Setenv("HOME", t.TempDir())
	if got := hermesSQLiteFinal("hermes", "h-webhook", "done"); got != nil {
		t.Fatalf("库不存在时应返回 nil: %v", got)
	}
}

func TestJsonMapUsesSQLiteWhenFileMissing(t *testing.T) {
	dbPath := newHermesFixture(t)
	dir := filepath.Dir(dbPath)
	write(t, filepath.Join(dir, "sessions", "sessions.json"),
		`{"hook:webhook:only":{"session_id":"h-talk","updated_at":"2026-09-13T10:10:00Z"}}`,
	)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES ('w1', 'h-talk', 'assistant', '只在 state.db 里的回答', 'stop', NULL, '2026-09-13T10:09:00Z', 1)`,
	})

	source := newJsonMapSource(hermesDef(defaultHome()))
	list := source.List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	final := source.Final(list[0])
	if final["isFinal"] != true || final["text"] != "只在 state.db 里的回答" {
		raw, _ := json.Marshal(final)
		t.Fatalf("final 应来自 state.db: %s", raw)
	}
	if final["error"] != nil {
		t.Fatalf("不该走兜底错误: %v", final)
	}
}

// 新版 Hermes 的形态：sessions.json / jsonl 都不存在，会话全部在 state.db 里
func TestHermesSQLiteOnlySource(t *testing.T) {
	dbPath := newHermesFixture(t)
	makeHermesDB(t, dbPath, hermesSchema, []string{
		`INSERT INTO sessions (id, session_key, display_name, source, model,
			input_tokens, output_tokens, estimated_cost_usd, started_at, ended_at)
		 VALUES ('20260814_002606_1a7908', NULL, '桌面会话', 'desktop', 'deepseek-v4-pro',
			1500, 220, 0.0123, 1786638366.44, NULL)`,
		`INSERT INTO messages (id, session_id, role, content, reasoning, timestamp, active)
		 VALUES (1, '20260814_002606_1a7908', 'user', '你是什么模型？', NULL, 1786638379.3163, 1)`,
		`INSERT INTO messages (id, session_id, role, content, finish_reason, reasoning, timestamp, active)
		 VALUES (2, '20260814_002606_1a7908', 'assistant', '我是 Hermes', 'stop', '想一下', 1786638385.27674, 1)`,
		`INSERT INTO messages (id, session_id, role, content, reasoning, timestamp, active)
		 VALUES (3, '20260814_002606_1a7908', 'user', '被软删的', NULL, 1786713684.97493, 0)`,
	})

	source := newJsonMapSource(hermesDef(defaultHome()))
	if !source.Exists() {
		t.Fatal("state.db 存在时 hermes 源应启用")
	}
	if source.Location() != dbPath {
		t.Fatalf("location 应指向 state.db: %v", source.Location())
	}

	list := source.List()
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	r := list[0]
	if r.str("sessionId") != "20260814_002606_1a7908" || r.str("key") != "20260814_002606_1a7908" {
		t.Fatalf("record = %v", r.fields)
	}
	if r.str("platform") != "desktop" || r.str("model") != "deepseek-v4-pro" || r.str("displayName") != "桌面会话" {
		t.Fatalf("record = %v", r.fields)
	}
	if r.get("totalTokens") != float64(1720) || r.get("estimatedCostUsd") != 0.0123 {
		t.Fatalf("record = %v", r.fields)
	}
	if r.get("hasFile") != false || r.get("file") != nil {
		t.Fatalf("record = %v", r.fields)
	}
	// updatedAt 取最后一条 active 消息的时间（epoch 秒 → UTC ISO）
	if r.str("createdAt") != "2026-08-13T16:26:06" || r.str("updatedAt") != "2026-08-13T16:26:25" {
		t.Fatalf("createdAt/updatedAt = %v / %v", r.str("createdAt"), r.str("updatedAt"))
	}

	msgs := source.Messages(r, 50)
	if len(msgs) != 2 { // active=0 的不算
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["role"] != "user" || msgs[0]["timestamp"] != "2026-08-13T16:26:19" {
		t.Fatalf("msgs[0] = %v", msgs[0])
	}
	blocks := msgs[1]["content"].([]map[string]any)
	if len(blocks) != 2 || blocks[0]["type"] != "thinking" || blocks[0]["content"] != "想一下" ||
		blocks[1]["type"] != "text" || blocks[1]["content"] != "我是 Hermes" {
		t.Fatalf("blocks = %v", blocks)
	}

	final := source.Final(r)
	if final["isFinal"] != true || final["text"] != "我是 Hermes" || final["thinking"] != "想一下" {
		t.Fatalf("final = %v", final)
	}
}
