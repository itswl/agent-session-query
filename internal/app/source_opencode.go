package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo, cross-compilation unaffected)
)

// OpenCode source: everything lives in one SQLite database (session / message / part
// tables). There is no jsonl per session, so this is a searchableSource — the generic
// path has no file to scan.
//
// Verified against the on-disk schema, not against documentation:
//
//	session:  id, directory (the cwd), title, model (a JSON string like
//	          {"id":"deepseek-flash","providerID":"deepseek"}), cost, tokens_*,
//	          time_created / time_updated (unix milliseconds), time_archived
//	message:  id, session_id, data (JSON: role, path.cwd, cost, tokens{...},
//	          modelID, providerID, time.created / completed, finish)
//	part:     message_id, session_id, data (JSON, discriminated by type:
//	          text | reasoning | tool | step-start | step-finish)
//
// A tool part carries the call and the result together (state.status / state.input /
// state.output / state.error); text is state.output as a plain string.
type OpenCodeSource struct {
	dbPath string
}

func newOpenCodeSource(dbPath string) *OpenCodeSource {
	return &OpenCodeSource{dbPath: dbPath}
}

// openCodeDataDir follows opencode's own resolution: XDG_DATA_HOME on every Unix-like
// (macOS included — it really is ~/.local/share/opencode there, not
// ~/Library/Application Support), LOCALAPPDATA on Windows.
func openCodeDataDir(home string) string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "opencode")
		}
		return filepath.Join(home, "AppData", "Local", "opencode")
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

func (s *OpenCodeSource) Mode() string     { return "opencode" }
func (s *OpenCodeSource) Location() string { return s.dbPath }
func (s *OpenCodeSource) Exists() bool     { return fileExists(s.dbPath) }

// open opens the database read-only. Verified against a live writer: mode=ro reads
// through the write-ahead log fine, so sessions still being written are visible.
func (s *OpenCodeSource) open() (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteURI(s.dbPath)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (s *OpenCodeSource) List() []record {
	db, err := s.open()
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id, directory, title, slug, model, time_created, time_updated
		FROM session
		WHERE time_archived IS NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []record{}
	for rows.Next() {
		var id, directory, title, slug, model sql.NullString
		var created, updated sql.NullInt64
		if err := rows.Scan(&id, &directory, &title, &slug, &model, &created, &updated); err != nil {
			return out
		}
		if !id.Valid || id.String == "" {
			continue
		}

		// title is what opencode itself shows in its session list (it writes one for
		// every session); fall back to the slug, then the raw id
		name := firstNonEmpty(title.String, slug.String, id.String)

		out = append(out, newRecord(map[string]any{
			"source":    "opencode",
			"key":       s.dbPath + "#" + id.String,
			"shortKey":  name,
			"sessionId": id.String,
			"file":      nil,
			"hasFile":   false,
			"status":    "done",
			"cwd":       directory.String,
			"model":     openCodeModelName(model.String),
			"updatedAt": millisToISO(updated),
			"createdAt": millisToISO(created),
		}, millisToISO(updated)))
	}
	return out
}

// openCodeModelName turns the model column's JSON into the "provider/model" opencode
// displays. Anything unexpected (empty, not JSON) becomes the raw string.
func openCodeModelName(raw string) string {
	if raw == "" {
		return ""
	}
	var m struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw
	}
	if m.ProviderID != "" {
		return m.ProviderID + "/" + m.ID
	}
	return m.ID
}

// millisToISO formats a unix-milliseconds column the way the file sources format
// their epochs, so sorting and display agree across sources.
func millisToISO(ms sql.NullInt64) string {
	if !ms.Valid || ms.Int64 <= 0 {
		return ""
	}
	return time.UnixMilli(ms.Int64).UTC().Format("2006-01-02T15:04:05")
}

// openCodeMessage is one message row plus its decoded payload.
type openCodeMessage struct {
	id   string
	data map[string]any
}

func (s *OpenCodeSource) Messages(r record, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	sessionID := r.str("sessionId")
	if sessionID == "" {
		return sink.result()
	}

	db, err := s.open()
	if err != nil {
		return sink.result()
	}
	defer db.Close()

	// Two queries rather than a join: parts are grouped per message, and the grouping is
	// cheaper in a map than in ORDER BY--aware scanning. Message order follows the index
	// on (session_id, time_created, id).
	msgs, err := s.messages(db, sessionID)
	if err != nil {
		return sink.result()
	}
	parts, err := s.partsByMessage(db, sessionID)
	if err != nil {
		return sink.result()
	}

	for _, m := range msgs {
		role, _ := m.data["role"].(string)
		if role == "" {
			role = "unknown"
		}
		blocks := []map[string]any{}
		for _, p := range parts[m.id] {
			blocks = append(blocks, openCodeBlocks(p)...)
		}
		sink.add(map[string]any{
			"id":        m.id,
			"role":      role,
			"timestamp": openCodeJSONMillis(m.data, "time", "created"),
			"content":   blocks,
		})
	}
	return sink.result()
}

func (s *OpenCodeSource) messages(db *sql.DB, sessionID string) ([]openCodeMessage, error) {
	rows, err := db.Query(`
		SELECT id, data FROM message
		WHERE session_id = ?
		ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []openCodeMessage{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return out, err
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			continue // one unparsable row must not hide the rest of the session
		}
		out = append(out, openCodeMessage{id: id, data: decoded})
	}
	return out, rows.Err()
}

// partsByMessage loads every part of a session, grouped by message and kept in the order
// the index on (message_id, id) gives.
func (s *OpenCodeSource) partsByMessage(db *sql.DB, sessionID string) (map[string][]map[string]any, error) {
	rows, err := db.Query(`
		SELECT message_id, data FROM part
		WHERE session_id = ?
		ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]map[string]any{}
	for rows.Next() {
		var messageID, data string
		if err := rows.Scan(&messageID, &data); err != nil {
			return out, err
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			continue
		}
		out[messageID] = append(out[messageID], decoded)
	}
	return out, rows.Err()
}

// openCodeBlocks maps one part onto the shared block shapes. step-start / step-finish
// are step boundaries, not content, and are dropped.
func openCodeBlocks(part map[string]any) []map[string]any {
	kind, _ := part["type"].(string)
	switch kind {
	case "text":
		return []map[string]any{{"type": "text", "content": strField(part, "text")}}
	case "reasoning":
		return []map[string]any{{"type": "thinking", "content": strField(part, "text")}}
	case "tool":
		state, _ := part["state"].(map[string]any)
		name, _ := part["tool"].(string)
		call := map[string]any{
			"type": "toolCall", "name": name,
			"arguments": getOr(state, "input", map[string]any{}),
		}
		// The call and its result live in one part; emit the result half only once there
		// is one (completed or errored). A pending or running tool shows as the call.
		status, _ := state["status"].(string)
		if status != "completed" && status != "error" {
			return []map[string]any{call}
		}
		content := ""
		if status == "error" {
			if errText := toStr(state["error"]); errText != "" {
				content = "error: " + errText
			}
		}
		if content == "" {
			content = toStr(state["output"])
		}
		return []map[string]any{call, {
			"type": "toolResult", "toolName": name,
			"content": truncate(content, 500, "...[truncated]"),
		}}
	}
	return nil
}

func (s *OpenCodeSource) Final(r record) map[string]any {
	sessionID := r.str("sessionId")
	if sessionID == "" {
		return nil
	}
	db, err := s.open()
	if err != nil {
		return nil
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM message WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		return nil
	}

	// The newest assistant message that carries a finish field; step boundaries and
	// aborted turns land without one and are not the session's result.
	var lastID, lastData string
	rows, err := db.Query(`
		SELECT id, data FROM message
		WHERE session_id = ? AND json_extract(data, '$.role') = 'assistant'
		ORDER BY time_created DESC, id DESC
		LIMIT 10`, sessionID)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			break
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(data), &decoded) != nil {
			continue
		}
		if finish, ok := decoded["finish"].(string); ok && finish != "" {
			lastID, lastData = id, data
			rows.Close()
			var last map[string]any
			_ = json.Unmarshal([]byte(lastData), &last)
			return s.finalFrom(db, sessionID, lastID, last, count)
		}
	}
	rows.Close()

	return map[string]any{
		"status":       "done",
		"isFinal":      false,
		"isProcessing": false,
		"messageCount": count,
		"source":       "opencode",
		"text":         "",
		"thinking":     "",
	}
}

// finalFrom assembles the final result from the chosen message plus its text and
// reasoning parts, with the session-level cost and token totals.
func (s *OpenCodeSource) finalFrom(db *sql.DB, sessionID, messageID string, last map[string]any, count int) map[string]any {
	finish, _ := last["finish"].(string)

	texts, thoughts := []string{}, []string{}
	if rows, err := db.Query(`
		SELECT data FROM part
		WHERE session_id = ? AND message_id = ?
		ORDER BY time_created, id`, sessionID, messageID); err == nil {
		for rows.Next() {
			var data string
			if rows.Scan(&data) != nil {
				break
			}
			var part map[string]any
			if json.Unmarshal([]byte(data), &part) != nil {
				continue
			}
			switch part["type"] {
			case "text":
				if t := strField(part, "text"); t != "" {
					texts = append(texts, t)
				}
			case "reasoning":
				if t := strField(part, "text"); t != "" {
					thoughts = append(thoughts, t)
				}
			}
		}
		rows.Close()
	}

	var cost sql.NullFloat64
	var in, out, reasoning, cacheRead, cacheWrite sql.NullInt64
	_ = db.QueryRow(`
		SELECT cost, tokens_input, tokens_output, tokens_reasoning,
		       tokens_cache_read, tokens_cache_write
		FROM session WHERE id = ?`, sessionID).
		Scan(&cost, &in, &out, &reasoning, &cacheRead, &cacheWrite)

	model := ""
	if id, _ := last["modelID"].(string); id != "" {
		model = id
		if p, _ := last["providerID"].(string); p != "" {
			model = p + "/" + id
		}
	}

	return map[string]any{
		"status":       "done",
		"isFinal":      finish == "stop" || finish == "end_turn",
		"isProcessing": false,
		"messageCount": count,
		"source":       "opencode",
		"id":           messageID,
		"timestamp":    openCodeJSONMillis(last, "time", "completed"),
		"stopReason":   finish,
		"model":        model,
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    []any{},
		"usage": map[string]any{
			"inputTokens":      nullIntOrZero(in),
			"outputTokens":     nullIntOrZero(out),
			"reasoningTokens":  nullIntOrZero(reasoning),
			"cacheReadTokens":  nullIntOrZero(cacheRead),
			"cacheWriteTokens": nullIntOrZero(cacheWrite),
			"estimatedCostUsd": nullFloatOrZero(cost),
		},
	}
}

// Search implements searchableSource: the bodies are in SQLite, so LIKE beats scanning
// files that do not exist.
func (s *OpenCodeSource) Search(ctx context.Context, r record, q searchQuery) []map[string]any {
	sessionID := r.str("sessionId")
	if sessionID == "" || len(q.lowered) == 0 {
		return nil
	}
	db, err := s.open()
	if err != nil {
		return nil
	}
	defer db.Close()

	like := "%" + escapeLike(string(q.lowered)) + "%"
	// body text and reasoning are both in part.data.text, so one LIKE covers both
	rows, err := db.QueryContext(ctx, `
		SELECT p.data, m.data
		FROM part p JOIN message m ON m.id = p.message_id
		WHERE p.session_id = ?
		  AND p.data LIKE ? ESCAPE '\'
		ORDER BY p.time_created, p.id
		LIMIT ?`, sessionID, like, q.perSession)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var partData, msgData string
		if err := rows.Scan(&partData, &msgData); err != nil {
			return out
		}
		var part, msg map[string]any
		if json.Unmarshal([]byte(partData), &part) != nil || json.Unmarshal([]byte(msgData), &msg) != nil {
			continue
		}
		body := strField(part, "text")
		if body == "" || part["type"] == "tool" {
			// a hit inside a tool call's arguments or metadata is not a body hit
			continue
		}
		role, _ := msg["role"].(string)
		out = append(out, map[string]any{
			"role":      role,
			"snippet":   snippetAround(body, string(q.lowered), searchSnippetRadius),
			"timestamp": openCodeJSONMillis(msg, "time", "created"),
		})
	}
	return out
}

// openCodeJSONMillis digs a unix-milliseconds value out of nested JSON (time.created /
// time.completed) and formats it like the column helper. The JSON values are already
// milliseconds — the same unit as the columns.
func openCodeJSONMillis(obj map[string]any, outer, inner string) string {
	nested, _ := obj[outer].(map[string]any)
	if nested == nil {
		return ""
	}
	ms, ok := toFloat(nested[inner])
	if !ok {
		return ""
	}
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05")
}

// firstNonEmpty returns the first argument that is not empty
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
