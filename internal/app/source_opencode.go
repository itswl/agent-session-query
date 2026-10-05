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
//
// OpenCode 2.x changed the layout: sessions moved to session_v2 and their messages to
// session_message, where one row is one message with its parts inline (data.content[],
// the same shapes as the part rows, except that a tool item names its tool as name and its
// call as id). The V1 tables stop being written. For a long time this read V1 only, so a
// 2.x install listed nothing and said nothing — an empty source and a moved one answer
// alike. The schema is detected per database: V2 when both tables exist, and in a
// database carrying both layouts the V1 sessions that were never migrated are listed too.
type OpenCodeSource struct {
	dbPath string
}

// openCodeHasV2 reports whether the database carries the 2.x tables. Both have to exist:
// one alone is a migration caught halfway, and the V1 tables are still the truth then.
func openCodeHasV2(db *sql.DB) bool {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('session_v2', 'session_message')`).Scan(&n)
	return err == nil && n == 2
}

// openCodeHasTable: whether a V1 table is still around in a V2 database
func openCodeHasTable(db *sql.DB, name string) bool {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return err == nil && n == 1
}

// openCodeInV2 says which layout a session lives in, for a database that has both
func openCodeInV2(db *sql.DB, sessionID string) bool {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM session_v2 WHERE id = ?`, sessionID).Scan(&n)
	return err == nil && n == 1
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

	if openCodeHasV2(db) {
		out := s.listV2(db)
		if openCodeHasTable(db, "session") {
			// A database that saw 2.x and 1.x both: the V1 sessions that never migrated
			// are history too, and a migrated one keeps its id, so NOT EXISTS keeps each
			// session to one row
			out = append(out, s.listV1(db, ` AND NOT EXISTS (SELECT 1 FROM session_v2 v WHERE v.id = session.id)`)...)
		}
		return out
	}
	return s.listV1(db, "")
}

// listV2 lists the 2.x sessions. The columns are the ones every 2.x database has been seen
// to carry: a title, the directory the session ran in, and the two times. Whether the
// table marks archived sessions is read off the schema rather than assumed.
func (s *OpenCodeSource) listV2(db *sql.DB) []record {
	archivedClause := ""
	if columns, err := tableColumns(db, "session_v2"); err == nil && columns["time_archived"] {
		archivedClause = ` WHERE s.time_archived IS NULL`
	}
	rows, err := db.Query(`
		SELECT s.id, s.directory, COALESCE(s.title, ''), s.time_created, s.time_updated,
		       (SELECT COUNT(*) FROM session_message m
		         WHERE m.session_id = s.id AND m.type IN ('user', 'assistant')),
		       (SELECT MAX(m.time_created) FROM session_message m WHERE m.session_id = s.id)
		FROM session_v2 s` + archivedClause)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []record{}
	for rows.Next() {
		var id, directory, title sql.NullString
		var created, updated, messageCount, lastMessage sql.NullInt64
		if err := rows.Scan(&id, &directory, &title, &created, &updated, &messageCount, &lastMessage); err != nil {
			return out
		}
		if !id.Valid || id.String == "" {
			continue
		}
		// time_updated on the session is not always moved by a new message; the newest
		// message's time is, so the later of the two is the session's
		if lastMessage.Valid && lastMessage.Int64 > updated.Int64 {
			updated = lastMessage
		}
		out = append(out, newRecord(map[string]any{
			"source":       "opencode",
			"key":          s.dbPath + "#" + id.String,
			"shortKey":     firstNonEmpty(title.String, id.String),
			"sessionId":    id.String,
			"file":         nil,
			"hasFile":      false,
			"status":       "done",
			"cwd":          directory.String,
			"messageCount": messageCount.Int64,
			"updatedAt":    millisToISO(updated),
			"createdAt":    millisToISO(created),
		}, millisToISO(updated)))
	}
	return out
}

// listV1 lists the sessions of the original layout; extra narrows the set in a mixed
// database
func (s *OpenCodeSource) listV1(db *sql.DB, extra string) []record {
	rows, err := db.Query(`
		SELECT id, directory, title, slug, model, time_created, time_updated,
		       (SELECT COUNT(*) FROM message m WHERE m.session_id = session.id)
		FROM session
		WHERE time_archived IS NULL` + extra)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []record{}
	for rows.Next() {
		var id, directory, title, slug, model sql.NullString
		var created, updated, messageCount sql.NullInt64
		if err := rows.Scan(&id, &directory, &title, &slug, &model, &created, &updated, &messageCount); err != nil {
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
			// message carries a (session_id, ...) index, so the count is an index scan
			"messageCount": messageCount.Int64,
			"updatedAt":    millisToISO(updated),
			"createdAt":    millisToISO(created),
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

	if openCodeHasV2(db) && openCodeInV2(db, sessionID) {
		return s.messagesV2(db, sessionID, q)
	}

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
			blocks = append(blocks, openCodeBlocks(p, q.full)...)
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

// messagesV2 reads a 2.x session: one session_message row per message, ordered by seq,
// the parts inline under data.content (a user row may carry its words as data.text
// instead). The row's type is the role.
func (s *OpenCodeSource) messagesV2(db *sql.DB, sessionID string, q messageQuery) []map[string]any {
	sink := newMessageSink(q)
	rows, err := db.Query(`
		SELECT id, type, time_created, data FROM session_message
		WHERE session_id = ? AND type IN ('user', 'assistant', 'system')
		ORDER BY seq, rowid`, sessionID)
	if err != nil {
		return sink.result()
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, data string
		var created sql.NullInt64
		if err := rows.Scan(&id, &kind, &created, &data); err != nil {
			return sink.result()
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(data), &decoded) != nil {
			continue // one unparsable row must not hide the rest of the session
		}
		blocks := openCodeV2Blocks(kind, decoded, q.full)
		if !sink.add(map[string]any{
			"id":        id,
			"role":      kind,
			"timestamp": millisToISO(created),
			"content":   blocks,
		}) {
			break
		}
	}
	return sink.result()
}

// openCodeV2Blocks folds a 2.x message's data into blocks: the content array when it has
// one (the part shapes, inline), else the text field.
func openCodeV2Blocks(kind string, data map[string]any, full bool) []map[string]any {
	blocks := []map[string]any{}
	switch content := data["content"].(type) {
	case []any:
		for _, item := range content {
			if part, ok := item.(map[string]any); ok {
				blocks = append(blocks, openCodeBlocks(part, full)...)
			}
		}
	case string:
		if content != "" {
			blocks = append(blocks, textBlock(content))
		}
	}
	if len(blocks) == 0 {
		if text := strField(data, "text"); text != "" {
			blocks = append(blocks, textBlock(text))
		}
	}
	return blocks
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
func openCodeBlocks(part map[string]any, full bool) []map[string]any {
	kind, _ := part["type"].(string)
	switch kind {
	case "text":
		return []map[string]any{textBlock(strField(part, "text"))}
	case "reasoning":
		return []map[string]any{thinkingBlock(strField(part, "text"), full)}
	case "tool":
		state, _ := part["state"].(map[string]any)
		// V1 names the tool in tool and the call in callID; a 2.x inline item says name
		// and id
		name := firstNonEmpty(strField(part, "tool"), strField(part, "name"))
		callID := firstNonEmpty(strField(part, "callID"), strField(part, "id"))
		args := state["input"]
		if args == nil {
			args = part["input"]
		}
		call := toolCallBlock(callID, name, args)
		// The call and its result live in one part; emit the result half only once there
		// is one (completed or errored). A pending or running tool shows as the call.
		status, _ := state["status"].(string)
		if status != "completed" && status != "error" {
			return []map[string]any{call}
		}
		errText := toStr(state["error"])
		outcome := toolOutcome{status: statusOK}
		if status == "error" {
			outcome.status = statusError
			// opencode reports a tool the user stopped as an error whose text says so
			if strings.Contains(strings.ToLower(errText), "abort") {
				outcome.status = statusInterrupted
			}
		}
		// A shell's exit code sits in the state's metadata, and the state carries the
		// start and end of the call
		if metadata, ok := state["metadata"].(map[string]any); ok {
			if code, ok := toFloat(metadata["exit"]); ok {
				outcome.exitCode, outcome.hasExit = int(code), true
			}
		}
		if span, ok := state["time"].(map[string]any); ok {
			if start, ok := toFloat(span["start"]); ok {
				if end, ok := toFloat(span["end"]); ok && end >= start {
					outcome.durationMs = int64(end - start)
				}
			}
		}
		content := toStr(state["output"])
		if status == "error" && errText != "" {
			content = "error: " + errText
		}
		return []map[string]any{call, outcome.apply(toolResultBlock(callID, name, content, full))}
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

	if openCodeHasV2(db) && openCodeInV2(db, sessionID) {
		return s.finalV2(db, sessionID)
	}

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

// finalV2 assembles the final result of a 2.x session: the newest assistant message that
// said something, with the usage summed over every assistant row (a 2.x message carries
// its own tokens and cost under data, the way V1 message rows did; whether session_v2
// still totals them is not relied on).
func (s *OpenCodeSource) finalV2(db *sql.DB, sessionID string) map[string]any {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_message
		WHERE session_id = ? AND type IN ('user', 'assistant')`, sessionID).Scan(&count); err != nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT id, time_created, data FROM session_message
		WHERE session_id = ? AND type = 'assistant'
		ORDER BY seq DESC, rowid DESC`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var totals usageTotals
	var lastID string
	var lastAt sql.NullInt64
	var last map[string]any
	texts, thoughts := []string{}, []string{}
	for rows.Next() {
		var id, data string
		var created sql.NullInt64
		if err := rows.Scan(&id, &created, &data); err != nil {
			break
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(data), &decoded) != nil {
			continue
		}
		totals.add(openCodeMessageUsage(decoded))
		if last != nil {
			continue
		}
		for _, block := range openCodeV2Blocks("assistant", decoded, false) {
			switch block["type"] {
			case "text":
				if t := strField(block, "content"); t != "" {
					texts = append(texts, t)
				}
			case "thinking":
				if t := strField(block, "content"); t != "" {
					thoughts = append(thoughts, t)
				}
			}
		}
		if len(texts) > 0 {
			last, lastID, lastAt = decoded, id, created
		} else {
			texts, thoughts = texts[:0], thoughts[:0]
		}
	}
	if last == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "opencode",
			"text":         "",
			"thinking":     "",
			"usage":        totals.result(),
		}
	}
	finish := strField(last, "finish")
	model := ""
	if id := strField(last, "modelID"); id != "" {
		model = id
		if p := strField(last, "providerID"); p != "" {
			model = p + "/" + id
		}
	}
	return map[string]any{
		"status":       "done",
		"isFinal":      finish == "" || finish == "stop" || finish == "end_turn",
		"isProcessing": false,
		"messageCount": count,
		"source":       "opencode",
		"id":           lastID,
		"timestamp":    millisToISO(lastAt),
		"stopReason":   finish,
		"model":        model,
		"text":         strings.Join(texts, "\n"),
		"thinking":     strings.Join(thoughts, "\n"),
		"toolCalls":    []any{},
		"usage":        totals.result(),
	}
}

// openCodeMessageUsage reads a message row's own accounting — tokens{input, output,
// reasoning, cache{read, write}} and cost — into the shared usage names
func openCodeMessageUsage(data map[string]any) map[string]any {
	out := map[string]any{}
	tokens, _ := data["tokens"].(map[string]any)
	for from, to := range map[string]string{"input": "inputTokens", "output": "outputTokens", "reasoning": "reasoningTokens"} {
		if n, ok := toFloat(tokens[from]); ok {
			out[to] = n
		}
	}
	if cache, ok := tokens["cache"].(map[string]any); ok {
		if n, ok := toFloat(cache["read"]); ok {
			out["cacheReadTokens"] = n
		}
		if n, ok := toFloat(cache["write"]); ok {
			out["cacheWriteTokens"] = n
		}
	}
	if n, ok := toFloat(data["cost"]); ok {
		out["estimatedCostUsd"] = n
	}
	return out
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

	if openCodeHasV2(db) && openCodeInV2(db, sessionID) {
		return s.searchV2(ctx, db, sessionID, q)
	}

	like := "%" + escapeLike(string(q.lowered)) + "%"
	// The role filter goes into the query rather than onto the result: filtering the rows
	// the LIMIT already returned would keep whichever hits came first.
	roleClause := ""
	if q.role != "" {
		roleClause = "\n\t\t  AND json_extract(m.data, '$.role') = '" + escapeLike(q.role) + "'"
	}
	// body text and reasoning are both in part.data.text, so one LIKE covers both
	rows, err := db.QueryContext(ctx, `
		SELECT p.data, m.data
		FROM part p JOIN message m ON m.id = p.message_id
		WHERE p.session_id = ?
		  AND p.data LIKE ? ESCAPE '\'`+roleClause+`
		ORDER BY p.time_created, p.id
		LIMIT ?`, sessionID, like, q.probeLimit())
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

// searchV2 searches a 2.x session: the words live inline in session_message.data, so one
// LIKE over the row and a decode of the hits to find the matching text
func (s *OpenCodeSource) searchV2(ctx context.Context, db *sql.DB, sessionID string, q searchQuery) []map[string]any {
	roleClause := ""
	args := []any{sessionID}
	if q.role != "" {
		roleClause = " AND type = ?"
		args = append(args, q.role)
	}
	args = append(args, "%"+escapeLike(string(q.lowered))+"%", q.probeLimit())
	rows, err := db.QueryContext(ctx, `
		SELECT type, time_created, data FROM session_message
		WHERE session_id = ? AND type IN ('user', 'assistant')`+roleClause+`
		  AND data LIKE ? ESCAPE '\'
		ORDER BY seq, rowid
		LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var kind, data string
		var created sql.NullInt64
		if err := rows.Scan(&kind, &created, &data); err != nil {
			return out
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(data), &decoded) != nil {
			continue
		}
		text, ok := findMatchingText(decoded, string(q.lowered), 0)
		if !ok {
			continue // the match sat in an id or a tool's arguments, not in words
		}
		out = append(out, map[string]any{
			"role":      kind,
			"snippet":   snippetAround(text, string(q.lowered), searchSnippetRadius),
			"timestamp": millisToISO(created),
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
