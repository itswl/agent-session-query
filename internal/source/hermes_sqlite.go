package source

import (
	"context"
	"crypto/sha256"
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

	shown := hermesShownClause(db, "")
	var id, content, finishReason, reasoning, timestamp sql.NullString
	row = db.QueryRow(
		`SELECT id, content, finish_reason, reasoning, timestamp
		 FROM messages
		 WHERE session_id = ?
		   AND role = 'assistant'`+shown+`
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
			`SELECT COUNT(*) FROM messages WHERE session_id = ?`+shown,
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
		"stopReason":   StrOr(finishReason.String, "stop"),
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
//
// The error is returned alongside the records rather than only logged: a database this
// cannot read is not an empty database, and a caller that cannot tell the two apart reports
// the first as the second.
func hermesSQLiteList(dbPath, mode string, skip map[string]bool) ([]Record, error) {
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite("list", err)
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT s.id, COALESCE(NULLIF(s.session_key, ''), s.id),
		       COALESCE(NULLIF(s.title, ''), NULLIF(s.display_name, ''), ''),
		       s.source, s.model, s.cwd,
		       s.input_tokens, s.output_tokens, s.reasoning_tokens, s.estimated_cost_usd,
		       s.started_at, s.ended_at,
		       (SELECT MAX(m.timestamp) FROM messages m
		         WHERE m.session_id = s.id` + hermesShownClause(db, "m.") + `),
		       s.message_count
		FROM sessions s` + hermesVisibilityClause(db))
	if err != nil {
		warnHermesSQLite("list", err)
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
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
			return out, err
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
		// A title the CLI wrote is text like any other: cleaned on the way in, so a
		// display name that quoted a key does not ride out through the list, the brief
		// or the page (see RedactSecrets)
		title := RedactSecrets(StripTerminalControls(displayName.String))
		out = append(out, NewRecord(Record{
			Source: mode,
			Key:    keyStr,
			// Hermes writes a real title (title_source marks who made it); the display
			// name is the older field, and the key is the last resort
			ShortKey:         FirstNonEmpty(title, keyStr),
			SessionID:        sid.String,
			Status:           "done",
			Cwd:              cwd.String,
			UpdatedAt:        updatedAt,
			CreatedAt:        createdAt,
			DisplayName:      title,
			Platform:         platform.String,
			Model:            model.String,
			TotalTokens:      totalTokens,
			EstimatedCostUsd: nullFloatOrZero(cost),
			// Hermes maintains the count itself; the file sources have no such column
			// and get theirs lazily in the page once a session is opened
			MessageCount: int(nullIntOrZero(messageCount)),
			HasCount:     true,
		}, updatedAt))
	}
	// The rows loop ends on an error too (a truncated read, a database replaced under us),
	// and the partial list that came back must not be handed up as the whole one
	if err := rows.Err(); err != nil {
		warnHermesSQLite("list", err)
		return out, err
	}
	return out, nil
}

// hermesVisibilityClause returns the WHERE fragment that leaves out the sessions Hermes
// itself hides, or "" when this state.db has no column for them.
//
// Which column marks them is not the same in every Hermes: Bot Mode sessions were marked in
// `hidden`, and current Hermes (schema 25) marks them in `archived` instead. An unknown
// column does not degrade a query, it takes the whole statement down — so the columns are
// read off the database instead of assumed, the way Hermes itself filters on
// COALESCE(archived, 0).
//
// With neither column present the list runs unfiltered, which is the safer failure: showing
// one session Hermes would have hidden beats reporting a database full of sessions as empty.
func hermesVisibilityClause(db *sql.DB) string {
	columns, err := tableColumns(db, "sessions")
	if err != nil {
		warnHermesSQLite("list", err)
		return ""
	}
	switch {
	case columns["hidden"]:
		return "\n\t\tWHERE COALESCE(s.hidden, 0) = 0"
	case columns["archived"]:
		return "\n\t\tWHERE COALESCE(s.archived, 0) = 0"
	}
	return ""
}

// hermesShownClause is the WHERE fragment (leading AND) that keeps the message rows Hermes
// itself displays, built from the columns this state.db has.
//
// Hermes marks rows two ways. Undo, rewind and regenerate set active = 0 and the row is
// gone from the conversation. Context compression also sets active = 0 on the rows it
// folded into a summary, but marks them compacted = 1 — and Hermes keeps showing those,
// because the person did say and read them; only the model stopped seeing them. Filtering
// on active alone, which this did, hid every compressed stretch of a long session: the
// older half of the conversation vanished as soon as Hermes compressed it. So a row is
// shown when it is live or compacted, and hidden only when it is neither. An older
// state.db without the compacted column keeps the active-only rule, and one without
// either column is left unfiltered.
func hermesShownClause(db *sql.DB, alias string) string {
	columns, err := tableColumns(db, "messages")
	if err != nil {
		warnHermesSQLite("messages", err)
		return ""
	}
	switch {
	case columns["active"] && columns["compacted"]:
		return " AND (COALESCE(" + alias + "active, 1) = 1 OR COALESCE(" + alias + "compacted, 0) = 1)"
	case columns["active"]:
		return " AND COALESCE(" + alias + "active, 1) = 1"
	}
	return ""
}

// tableColumns reads one table's column names out of the schema: a schema read, so no rows
// are touched and no data is read
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// hermesSQLiteMessages reads messages for sessions that have no jsonl (the user/assistant/
// tool rows in state.db, shaped exactly like the jsonl path: an assistant's reasoning
// becomes thinking, its tool_calls column becomes toolCall blocks, a role=tool row is the
// result and becomes a toolResult block paired by tool_call_id, and epoch-second
// timestamps become UTC ISO).
//
// The rows are read the way Hermes reads them for display: in insertion order (timestamps
// can regress — clock skew, and compression re-inserting rows with their original time),
// filtered to what Hermes shows (see hermesShownClause), and collapsed on Hermes' own
// display identity. Compression re-inserts the protected head and tail of a conversation
// as live copies of archived originals, so a row is one message whether the database holds
// it once or twice; the first position is kept and the most live copy is shown. Rows the
// model reads but nobody typed (display_metadata.model_only, a micro-compaction merge) are
// left out. The projection is read in two passes — one decides identity and the winning copy and
// keeps only a light reference per message, the other fetches just the winners in
// chunks — so a read costs the window plus one chunk rather than the whole session. A
// LIMIT in SQL would count duplicates and hidden rows as messages; that is why the
// window is cut here.
func hermesSQLiteMessages(dbPath, sessionID string, q MessageQuery) []map[string]any {
	out := []map[string]any{}
	if q.Limit <= 0 {
		return out
	}
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	defer db.Close()

	columns, err := tableColumns(db, "messages")
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	// A column the schema lacks is selected as NULL under the same name, so one statement
	// fits every Hermes version seen so far
	col := func(name string) string {
		if columns[name] {
			return name
		}
		return "NULL AS " + name
	}
	rows, err := db.Query(`
		SELECT id, role, content, `+col("reasoning")+`, `+col("tool_calls")+`, `+col("tool_name")+`,
		       `+col("tool_call_id")+`, `+col("active")+`, `+col("display_metadata")+`, timestamp
		FROM messages
		WHERE session_id = ?`+hermesShownClause(db, "")+`
		  AND role IN ('user', 'assistant', 'tool')
		ORDER BY id ASC`, sessionID)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	defer rows.Close()

	// Pass one: decide which rows are shown and which copy of a duplicated row wins,
	// keeping only a light reference per message — its id, its liveness and its identity
	// hash. The content is read (it is part of the identity) and dropped here, so memory is
	// one small record per message rather than the whole session's text.
	type rowRef struct {
		id, key string
		active  int64
	}
	order := []rowRef{}
	index := map[string]int{}
	for rows.Next() {
		var id, role, content, reasoning, toolCalls, toolName, toolCallID, active, displayMeta, timestamp any
		if err := rows.Scan(&id, &role, &content, &reasoning, &toolCalls, &toolName, &toolCallID, &active, &displayMeta, &timestamp); err != nil {
			warnHermesSQLite(sessionID, err)
			return out
		}
		if hermesModelOnly(displayMeta) {
			continue
		}
		ref := rowRef{id: sqliteValueString(id), active: 1}
		if n, ok := ToFloat(active); ok {
			ref.active = int64(n)
		}
		ref.key = hermesRowKey(sqliteValueString(role), sqliteValueString(content), timestamp,
			sqliteValueString(toolCallID), sqliteValueString(toolCalls), sqliteValueString(toolName))
		if at, seen := index[ref.key]; seen {
			kept := order[at]
			if ref.active > kept.active || (ref.active == kept.active && ref.id > kept.id) {
				order[at] = ref
			}
			continue
		}
		index[ref.key] = len(order)
		order = append(order, ref)
	}
	if err := rows.Err(); err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	// The pool is one connection (SetMaxOpenConns in openHermesDB), so the first read has
	// to be finished before the second pass can query
	rows.Close()

	// Pass two: fetch just the winning rows, in chunks and in shown order, and emit them
	// through the sink. The chunk bounds what is resident: one page of rows plus the
	// window, rather than the session.
	sink := newMessageSink(q)
	const fetchChunk = 200
	for start := 0; start < len(order); start += fetchChunk {
		end := start + fetchChunk
		if end > len(order) {
			end = len(order)
		}
		placeholders := make([]string, 0, end-start)
		args := make([]any, 0, end-start+1)
		args = append(args, sessionID)
		for _, ref := range order[start:end] {
			placeholders = append(placeholders, "?")
			args = append(args, ref.id)
		}
		page, err := db.Query(`
			SELECT id, role, content, `+col("reasoning")+`, `+col("tool_calls")+`, `+col("tool_name")+`,
				   `+col("tool_call_id")+`, timestamp
			FROM messages
			WHERE session_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			warnHermesSQLite(sessionID, err)
			return sink.result()
		}
		type fullRow struct {
			role, content, reasoning, toolCalls, toolName, toolCallID string
			timestamp                                                 any
		}
		fetched := map[string]fullRow{}
		for page.Next() {
			var id, role, content, reasoning, toolCalls, toolName, toolCallID, timestamp any
			if err := page.Scan(&id, &role, &content, &reasoning, &toolCalls, &toolName, &toolCallID, &timestamp); err != nil {
				page.Close()
				warnHermesSQLite(sessionID, err)
				return sink.result()
			}
			row := fullRow{
				role: sqliteValueString(role), content: sqliteValueString(content),
				reasoning: sqliteValueString(reasoning), toolCalls: sqliteValueString(toolCalls),
				toolName: sqliteValueString(toolName), toolCallID: sqliteValueString(toolCallID),
				timestamp: timestamp,
			}
			// The id alone does not identify a row — nothing forces it to be unique in every
			// Hermes schema — so the winner is matched on id plus its identity hash
			key := hermesRowKey(row.role, row.content, timestamp, row.toolCallID, row.toolCalls, row.toolName)
			fetched[sqliteValueString(id)+"\x00"+key] = row
		}
		page.Close()
		for _, ref := range order[start:end] {
			row, ok := fetched[ref.id+"\x00"+ref.key]
			if !ok {
				continue // the row changed between the passes; there is nothing to show
			}
			parts := []map[string]any{}
			switch row.role {
			case "tool":
				// One tool row is one result; the content is the tool's structured output,
				// tool_name says which tool ran and tool_call_id which call it answers. Hermes
				// does not record whether it succeeded, so the block carries no status.
				parts = append(parts, ToolResultBlock(row.toolCallID, row.toolName, row.content, q.Full))
			default:
				if row.reasoning != "" {
					parts = append(parts, ThinkingBlock(row.reasoning, q.Full))
				}
				parts = append(parts, hermesToolCallBlocks(row.toolCalls)...)
				if row.content != "" {
					parts = append(parts, TextBlock(row.content))
				}
			}
			if !sink.add(map[string]any{
				"id":        ref.id,
				"role":      row.role,
				"timestamp": sqliteTimeString(row.timestamp),
				"content":   parts,
			}) {
				return sink.result()
			}
		}
	}
	return sink.result()
}

// hermesRowKey is the dedupe identity of one message row, as a hash: the index holds one
// entry per row for the whole session, and as a text key it kept every row's content a
// second time. SHA-256 rather than something cheaper because a collision would merge two
// rows that are genuinely distinct.
func hermesRowKey(role, content string, timestamp any, toolCallID, toolCalls, toolName string) string {
	keyHash := sha256.New()
	for _, part := range []string{role, content, sqliteTimeString(timestamp), toolCallID, toolCalls, toolName} {
		_, _ = keyHash.Write([]byte(part))
		_, _ = keyHash.Write([]byte{0})
	}
	return string(keyHash.Sum(nil))
}

// hermesModelOnly reads display_metadata.model_only, the mark on rows Hermes shows the
// model but not the person. The column holds JSON, sometimes JSON-encoded twice.
func hermesModelOnly(v any) bool {
	raw := sqliteValueString(v)
	if raw == "" {
		return false
	}
	var decoded any = raw
	for i := 0; i < 2; i++ {
		text, ok := decoded.(string)
		if !ok {
			break
		}
		var next any
		if json.Unmarshal([]byte(text), &next) != nil {
			return false
		}
		decoded = next
	}
	meta, ok := decoded.(map[string]any)
	if !ok {
		return false
	}
	return Truthy(meta["model_only"])
}

// hermesToolCallBlocks turns the tool_calls column (an OpenAI-shaped JSON array of
// {id, function: {name, arguments-as-string}}) into toolCall blocks.
func hermesToolCallBlocks(raw string) []map[string]any {
	if raw == "" {
		return nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(raw), &calls); err != nil || len(calls) == 0 {
		return nil
	}
	blocks := []map[string]any{}
	for _, call := range calls {
		args := map[string]any{}
		if call.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
		}
		blocks = append(blocks, ToolCallBlock(call.ID, call.Function.Name, args))
	}
	return blocks
}

// hermesSQLiteSearch searches one session's body inside state.db.
// SQLite's LIKE is already case-insensitive for ASCII, so let it do the work rather than
// reading every message out to compare here.
func hermesSQLiteSearch(ctx context.Context, dbPath, sessionID string, q SearchQuery) []map[string]any {
	if sessionID == "" || len(q.Lowered) == 0 || q.PerSession <= 0 {
		return nil
	}
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return nil
	}
	defer db.Close()

	like := "%" + EscapeLike(string(q.Lowered)) + "%"
	// The role filter goes into the query rather than onto the result: filtering the rows
	// the LIMIT already returned would keep whichever hits came first in the table.
	roleClause := ""
	args := []any{sessionID}
	if q.Role != "" {
		roleClause = "\n\t\t  AND role = ?"
		args = append(args, q.Role)
	}
	args = append(args, like, like, q.ProbeLimit())
	rows, err := db.QueryContext(ctx, `
		SELECT role, content, reasoning, timestamp
		FROM messages
		WHERE session_id = ?`+hermesShownClause(db, "")+roleClause+`
		  AND (content LIKE ? ESCAPE '\' OR reasoning LIKE ? ESCAPE '\')
		ORDER BY timestamp ASC, id ASC
		LIMIT ?`, args...)
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
		if IndexFold(text, string(q.Lowered)) < 0 {
			text = reasoning.String // the hit was in the reasoning
		}
		out = append(out, map[string]any{
			"snippet":   SnippetAround(text, string(q.Lowered), SearchSnippetRadius),
			"role":      role.String,
			"timestamp": sqliteTimeString(timestamp),
		})
	}
	return out
}

// EscapeLike escapes LIKE wildcards so that searching for "100%" does not turn into
// matching anything at all
func EscapeLike(s string) string {
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
	if sec, ok := ToFloat(v); ok {
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
