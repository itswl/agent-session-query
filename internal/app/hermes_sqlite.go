package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo, cross-compilation unaffected)
)

// hermesSQLiteFinal reads Hermes's state.db for a session's final message.
//
// Hermes webhook sessions sometimes land their final message only in state.db with
// nothing in the jsonl, so the record has no file and SQLite is the only source for
// final.
//
// nil means "no final message here either" (no database, no row, or a read failure), and
// the layer above turns that into its fallback response.
func hermesSQLiteFinal(dbPath, mode, sessionID, status string) map[string]any {
	if mode != "hermes" || sessionID == "" || dbPath == "" {
		return nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}

	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return nil
	}
	defer db.Close()

	var messageCount sql.NullInt64
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, reasoningTokens sql.NullInt64
	var estimatedCost sql.NullFloat64

	// Select only the columns actually used: one fewer column is one less thing a schema
	// difference can break
	row := db.QueryRow(
		`SELECT message_count, input_tokens, output_tokens, cache_read_tokens,
		        cache_write_tokens, reasoning_tokens, estimated_cost_usd
		 FROM sessions WHERE id = ?`,
		sessionID,
	)
	hasSession := true
	switch err := row.Scan(&messageCount, &inputTokens, &outputTokens, &cacheReadTokens,
		&cacheWriteTokens, &reasoningTokens, &estimatedCost); {
	case errors.Is(err, sql.ErrNoRows):
		hasSession = false
	case err != nil:
		warnHermesSQLite(sessionID, err)
		return nil
	}

	var id, content, finishReason, reasoning, timestamp sql.NullString
	row = db.QueryRow(
		`SELECT id, content, finish_reason, reasoning, timestamp
		 FROM messages
		 WHERE session_id = ?
		   AND role = 'assistant'
		   AND COALESCE(active, 1) = 1
		   AND finish_reason = 'stop'
		   AND COALESCE(content, '') <> ''
		 ORDER BY timestamp DESC, id DESC
		 LIMIT 1`,
		sessionID,
	)
	switch err := row.Scan(&id, &content, &finishReason, &reasoning, &timestamp); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		warnHermesSQLite(sessionID, err)
		return nil
	}

	// Fall back to the real count when message_count is missing or zero
	count := int64(0)
	if messageCount.Valid {
		count = messageCount.Int64
	}
	if count == 0 {
		var actual sql.NullInt64
		err := db.QueryRow(
			`SELECT COUNT(*) FROM messages WHERE session_id = ? AND COALESCE(active, 1) = 1`,
			sessionID,
		).Scan(&actual)
		if err != nil {
			warnHermesSQLite(sessionID, err)
			return nil
		}
		if actual.Valid {
			count = actual.Int64
		}
	}

	usage := map[string]any{}
	if hasSession {
		usage = map[string]any{
			"inputTokens":      nullIntOrZero(inputTokens),
			"outputTokens":     nullIntOrZero(outputTokens),
			"cacheReadTokens":  nullIntOrZero(cacheReadTokens),
			"cacheWriteTokens": nullIntOrZero(cacheWriteTokens),
			"reasoningTokens":  nullIntOrZero(reasoningTokens),
			"estimatedCostUsd": nullFloatOrZero(estimatedCost),
		}
	}

	return map[string]any{
		"status":       status,
		"isFinal":      true,
		"isProcessing": false,
		"messageCount": count,
		"source":       mode,
		"id":           nullStringOrNil(id),
		"timestamp":    sqliteTimeString(timestamp.String),
		"stopReason":   strOr(finishReason.String, "stop"),
		"text":         content.String,
		"thinking":     reasoning.String,
		"toolCalls":    []any{},
		"usage":        usage,
	}
}

func nullIntOrZero(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

func nullFloatOrZero(v sql.NullFloat64) float64 {
	if v.Valid {
		return v.Float64
	}
	return 0
}

// sqliteURI turns a file path into SQLite's URI form.
//
// Windows paths look like C:\Users\...\state.db, and backslashes inside a URI are
// ambiguous with escapes. Normalising to forward slashes avoids that; SQLite on Windows
// accepts file:C:/Users/.../state.db.
func sqliteURI(dbPath string) string {
	return "file:" + filepath.ToSlash(dbPath)
}

// openHermesDB opens state.db read-only: no -wal/-shm files, no changes to someone
// else's database
func openHermesDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteURI(dbPath)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// hermesSQLiteList lists sessions from state.db's sessions table.
// Newer Hermes no longer writes sessions.json or a jsonl per session; everything lives
// in SQLite. Session IDs registered in skip (those already in sessions.json) are passed
// over so nothing is listed twice.
func hermesSQLiteList(dbPath, mode string, skip map[string]bool) []record {
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite("list", err)
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT s.id, COALESCE(NULLIF(s.session_key, ''), s.id),
		       COALESCE(NULLIF(s.title, ''), NULLIF(s.display_name, ''), ''),
		       s.source, s.model, s.cwd,
		       s.input_tokens, s.output_tokens, s.reasoning_tokens, s.estimated_cost_usd,
		       s.started_at, s.ended_at,
		       (SELECT MAX(m.timestamp) FROM messages m
		         WHERE m.session_id = s.id AND COALESCE(m.active, 1) = 1),
		       s.message_count
		FROM sessions s
		WHERE s.hidden = 0`)
	if err != nil {
		warnHermesSQLite("list", err)
		return nil
	}
	defer rows.Close()

	out := []record{}
	for rows.Next() {
		var sid, key, displayName, platform, model, cwd sql.NullString
		var inputTokens, outputTokens, reasoningTokens sql.NullInt64
		var cost sql.NullFloat64
		var startedAt, endedAt, lastMsg sql.NullFloat64
		var messageCount sql.NullInt64
		if err := rows.Scan(&sid, &key, &displayName, &platform, &model, &cwd,
			&inputTokens, &outputTokens, &reasoningTokens, &cost, &startedAt, &endedAt, &lastMsg,
			&messageCount); err != nil {
			warnHermesSQLite("list", err)
			return out
		}
		if !sid.Valid || sid.String == "" || skip[sid.String] {
			continue
		}

		// updatedAt: last message time, then session end time, then start time
		updated := lastMsg
		if !updated.Valid {
			updated = endedAt
		}
		if !updated.Valid {
			updated = startedAt
		}
		updatedAt := ""
		if updated.Valid {
			if _, iso, ok := utcFromSeconds(updated.Float64); ok {
				updatedAt = iso
			}
		}
		createdAt := ""
		if startedAt.Valid {
			if _, iso, ok := utcFromSeconds(startedAt.Float64); ok {
				createdAt = iso
			}
		}
		totalTokens := float64(nullIntOrZero(inputTokens)) + float64(nullIntOrZero(outputTokens))

		keyStr := key.String
		out = append(out, newRecord(map[string]any{
			"source": mode,
			"key":    keyStr,
			// Hermes writes a real title (title_source marks who made it); the display
			// name is the older field, and the key is the last resort
			"shortKey":         firstNonEmpty(displayName.String, keyStr),
			"sessionId":        sid.String,
			"file":             nil,
			"hasFile":          false,
			"status":           "done",
			"cwd":              cwd.String,
			"updatedAt":        updatedAt,
			"createdAt":        createdAt,
			"displayName":      displayName.String,
			"platform":         platform.String,
			"model":            model.String,
			"totalTokens":      totalTokens,
			"estimatedCostUsd": nullFloatOrZero(cost),
			// Hermes maintains the count itself; the file sources have no such column
			// and get theirs lazily in the page once a session is opened
			"messageCount": nullIntOrZero(messageCount),
		}, updatedAt))
	}
	return out
}

// hermesSQLiteMessages reads messages for sessions that have no jsonl (the user/assistant/
// tool rows in state.db, shaped exactly like the jsonl path: an assistant's reasoning
// becomes thinking, its tool_calls column becomes toolCall blocks, a role=tool row is the
// result and becomes a toolResult block, and epoch-second timestamps become UTC ISO).
// For the latest N, let SQL walk backwards and reverse the result rather than reading the
// whole conversation.
func hermesSQLiteMessages(dbPath, sessionID string, q messageQuery) []map[string]any {
	out := []map[string]any{}
	if q.limit <= 0 {
		return out
	}
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	defer db.Close()

	order := "ASC"
	if q.fromEnd {
		order = "DESC"
	}
	// at anchors the window at a point in time (see messageQuery); the comparison runs
	// with the same direction as the ordering so the window sits on the right side of it
	anchor, anchorArg := "", any(nil)
	if !q.at.IsZero() {
		if q.fromEnd {
			anchor, anchorArg = " AND timestamp <= ?", q.at.Unix()
		} else {
			anchor, anchorArg = " AND timestamp >= ?", q.at.Unix()
		}
	}
	args := []any{sessionID}
	if anchorArg != nil {
		args = append(args, anchorArg)
	}
	args = append(args, q.limit)

	rows, err := db.Query(`
		SELECT id, role, content, reasoning, tool_calls, tool_name, timestamp
		FROM messages
		WHERE session_id = ?
		  AND COALESCE(active, 1) = 1
		  AND role IN ('user', 'assistant', 'tool')`+anchor+`
		ORDER BY timestamp `+order+`, id `+order+`
		LIMIT ?`, args...)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var id, role, content, reasoning, toolCalls, toolName, timestamp any
		if err := rows.Scan(&id, &role, &content, &reasoning, &toolCalls, &toolName, &timestamp); err != nil {
			warnHermesSQLite(sessionID, err)
			return out
		}
		parts := []map[string]any{}
		roleStr := sqliteValueString(role)
		switch roleStr {
		case "tool":
			// One tool row is one result; the content is the tool's structured output
			// (kept as-is, truncated), and tool_name says which tool ran
			parts = append(parts, map[string]any{
				"type":     "toolResult",
				"toolName": sqliteValueString(toolName),
				"content":  truncate(sqliteValueString(content), 500, "...[truncated]"),
			})
		default:
			if r, ok := reasoning.(string); ok && r != "" {
				parts = append(parts, map[string]any{"type": "thinking", "content": truncate(r, 1000, "...[truncated]")})
			}
			parts = append(parts, hermesToolCallBlocks(toolCalls)...)
			if c, ok := content.(string); ok && c != "" {
				parts = append(parts, map[string]any{"type": "text", "content": c})
			}
		}
		out = append(out, map[string]any{
			"id":        sqliteValueString(id),
			"role":      roleStr,
			"timestamp": sqliteTimeString(timestamp),
			"content":   parts,
		})
	}
	if q.fromEnd { // queried in reverse; flip back into chronological order
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// hermesToolCallBlocks turns the tool_calls column (an OpenAI-shaped JSON array of
// {function: {name, arguments-as-string}}) into toolCall blocks.
func hermesToolCallBlocks(raw any) []map[string]any {
	text, ok := raw.(string)
	if !ok || text == "" {
		return nil
	}
	var calls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(text), &calls); err != nil || len(calls) == 0 {
		return nil
	}
	blocks := []map[string]any{}
	for _, call := range calls {
		args := map[string]any{}
		if call.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
		}
		blocks = append(blocks, map[string]any{
			"type": "toolCall", "name": call.Function.Name, "arguments": args,
		})
	}
	return blocks
}

// hermesSQLiteSearch searches one session's body inside state.db.
// SQLite's LIKE is already case-insensitive for ASCII, so let it do the work rather than
// reading every message out to compare here.
func hermesSQLiteSearch(ctx context.Context, dbPath, sessionID string, q searchQuery) []map[string]any {
	if sessionID == "" || len(q.lowered) == 0 || q.perSession <= 0 {
		return nil
	}
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return nil
	}
	defer db.Close()

	like := "%" + escapeLike(string(q.lowered)) + "%"
	rows, err := db.QueryContext(ctx, `
		SELECT role, content, reasoning, timestamp
		FROM messages
		WHERE session_id = ?
		  AND COALESCE(active, 1) = 1
		  AND (content LIKE ? ESCAPE '\' OR reasoning LIKE ? ESCAPE '\')
		ORDER BY timestamp ASC, id ASC
		LIMIT ?`, sessionID, like, like, q.perSession)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return nil
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var role, content, reasoning sql.NullString
		var timestamp any
		if err := rows.Scan(&role, &content, &reasoning, &timestamp); err != nil {
			warnHermesSQLite(sessionID, err)
			return out
		}
		text := content.String
		if indexFold(text, string(q.lowered)) < 0 {
			text = reasoning.String // the hit was in the reasoning
		}
		out = append(out, map[string]any{
			"snippet":   snippetAround(text, string(q.lowered), searchSnippetRadius),
			"role":      role.String,
			"timestamp": sqliteTimeString(timestamp),
		})
	}
	return out
}

// escapeLike escapes LIKE wildcards so that searching for "100%" does not turn into
// matching anything at all
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}

// sqliteValueString folds SQLite's dynamically typed values into strings (id/role are
// stored as TEXT or INTEGER depending on the row)
func sqliteValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// sqliteTimeString handles the several ways a timestamp may be stored: REAL epoch
// seconds become UTC ISO, TEXT (an ISO string) passes through, and a numeric string
// (stored that way by TEXT affinity) is treated as an epoch too
func sqliteTimeString(v any) string {
	if s, ok := v.(string); ok {
		if sec, err := strconv.ParseFloat(s, 64); err == nil {
			if _, iso, ok := utcFromSeconds(sec); ok {
				return iso
			}
		}
		return s
	}
	if sec, ok := toFloat(v); ok {
		if _, iso, ok := utcFromSeconds(sec); ok {
			return iso
		}
	}
	return sqliteValueString(v)
}

// nullStringOrNil passes the field through as-is: SQL NULL becomes JSON null
func nullStringOrNil(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

func warnHermesSQLite(sessionID string, err error) {
	fmt.Fprintf(os.Stderr, "[WARN] reading the Hermes SQLite final message failed for %s: %v\n", sessionID, err)
}
