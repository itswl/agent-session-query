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
	"time"

	"modernc.org/sqlite" // pure-Go SQLite driver (no cgo, cross-compilation unaffected)
	sqlite3 "modernc.org/sqlite/lib"
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
	var final map[string]any
	err := readHermesDB(dbPath, func(db *sql.DB) (err error) {
		final, err = hermesFinalFrom(db, mode, sessionID, status)
		return err
	})
	if err != nil {
		warnHermesSQLite("the final message", sessionID, err)
		return nil
	}
	return final
}

// hermesFinalFrom is one attempt at hermesSQLiteFinal's read; nil without an error is "no
// final message here"
func hermesFinalFrom(db *sql.DB, mode, sessionID, status string) (map[string]any, error) {
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
		return nil, err
	}

	shown, err := hermesShownClause(db, "")
	if err != nil {
		return nil, err
	}
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
		return nil, nil
	case err != nil:
		return nil, err
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
			return nil, err
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
	}, nil
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

// sqliteBusyTimeout is how long a read waits for a write to finish before SQLite calls the
// database locked. Every database read here belongs to a program that is still writing to
// it, and without a timeout SQLite gives up at once: against Hermes's state.db, which a
// container writes while the host reads, the service log had "database is locked" 17 times
// — each one a final message or a page of messages missing for no reason but timing.
const sqliteBusyTimeout = 2 * time.Second

// sqliteReadOnlyDSN opens a database read-only — no -wal/-shm files, no changes to someone
// else's database — and sets the busy timeout on every connection the pool makes
func sqliteReadOnlyDSN(dbPath string) string {
	return sqliteURI(dbPath) + "?mode=ro&_pragma=busy_timeout(" +
		strconv.FormatInt(sqliteBusyTimeout.Milliseconds(), 10) + ")"
}

// openHermesDB opens state.db read-only (see sqliteReadOnlyDSN)
func openHermesDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(dbPath))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// hermesRetryPauses are the waits between a read that met a hot journal and its next try.
//
// A hot journal is the -journal file a crashed write leaves behind, and a read-only
// connection cannot roll one back: SQLite fails the read with SQLITE_READONLY_ROLLBACK and
// never consults the busy timeout, because a crash is not something to wait out. But SQLite
// calls a journal hot when no one holds a lock on the database, and the locks of a writer in
// a container need not be visible from the host: there the journal of a commit still in
// progress can look hot. Such a read was early, not wrong ("attempt to write a readonly
// database" four times in the same log), and one a moment later finds the journal gone. A
// journal that outlasts all of these pauses is a real one, and the read fails as before.
var hermesRetryPauses = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

// hermesPause waits out one retry pause; a test swaps it to finish the write in between
var hermesPause = time.Sleep

// readHermesDB opens state.db read-only and runs read on it, again after each pause while
// the read keeps meeting a journal that looks hot. read must be safe to repeat: whatever an
// attempt returned is replaced by the next one's.
func readHermesDB(dbPath string, read func(db *sql.DB) error) error {
	db, err := openHermesDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	err = read(db)
	for _, pause := range hermesRetryPauses {
		if !sqliteHotJournal(err) {
			break
		}
		hermesPause(pause)
		err = read(db)
	}
	return err
}

// sqliteHotJournal reports a read refused over a journal it could not roll back (see
// hermesRetryPauses). A locked database is not one: the busy timeout has already waited
// that out.
func sqliteHotJournal(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_READONLY_ROLLBACK
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
	var out []Record
	err := readHermesDB(dbPath, func(db *sql.DB) (err error) {
		out, err = hermesListFrom(db, mode, skip)
		return err
	})
	if err != nil {
		warnHermesSQLite("the session list", "", err)
	}
	return out, err
}

// hermesListFrom is one attempt at hermesSQLiteList's read
func hermesListFrom(db *sql.DB, mode string, skip map[string]bool) ([]Record, error) {
	shown, err := hermesShownClause(db, "m.")
	if err != nil {
		return nil, err
	}
	archived, err := hermesArchivedColumn(db)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT s.id, COALESCE(NULLIF(s.session_key, ''), s.id),
		       COALESCE(NULLIF(s.title, ''), NULLIF(s.display_name, ''), ''),
		       s.source, s.model, s.cwd,
		       s.input_tokens, s.output_tokens, s.reasoning_tokens, s.estimated_cost_usd,
		       s.started_at, s.ended_at,
		       (SELECT MAX(m.timestamp) FROM messages m
		         WHERE m.session_id = s.id` + shown + `),
		       s.message_count, ` + archived + `
		FROM sessions s`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var sid, key, displayName, platform, model, cwd sql.NullString
		var inputTokens, outputTokens, reasoningTokens sql.NullInt64
		var cost sql.NullFloat64
		var startedAt, endedAt, lastMsg sql.NullFloat64
		var messageCount, hidden sql.NullInt64
		if err := rows.Scan(&sid, &key, &displayName, &platform, &model, &cwd,
			&inputTokens, &outputTokens, &reasoningTokens, &cost, &startedAt, &endedAt, &lastMsg,
			&messageCount, &hidden); err != nil {
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
			HasTotalTokens:   true,
			EstimatedCostUsd: nullFloatOrZero(cost),
			HasEstimatedCost: true,
			// Hermes maintains the count itself; the file sources have no such column
			// and get theirs lazily in the page once a session is opened
			MessageCount: int(nullIntOrZero(messageCount)),
			HasCount:     true,
			Archived:     hidden.Int64 != 0,
		}, updatedAt))
	}
	// The rows loop ends on an error too (a truncated read, a database replaced under us),
	// and the partial list that came back must not be handed up as the whole one
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// hermesArchivedColumn is the select expression that says whether Hermes hides a session —
// listed all the same and marked archived, the way every source marks the sessions its CLI
// has put away, and left out of the list unless asked for.
//
// Which column marks them is not the same in every Hermes: Bot Mode sessions were marked in
// `hidden`, and current Hermes (schema 25) marks them in `archived` instead. An unknown
// column does not degrade a query, it takes the whole statement down — so the columns are
// read off the database instead of assumed, the way Hermes itself filters on
// COALESCE(archived, 0). With neither present, no session is marked. A schema that cannot be
// read fails the read it is for, rather than running on a guess.
func hermesArchivedColumn(db *sql.DB) (string, error) {
	columns, err := tableColumns(db, "sessions")
	if err != nil {
		return "", err
	}
	switch {
	case columns["hidden"]:
		return "COALESCE(s.hidden, 0)", nil
	case columns["archived"]:
		return "COALESCE(s.archived, 0)", nil
	}
	return "0", nil
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
func hermesShownClause(db *sql.DB, alias string) (string, error) {
	columns, err := tableColumns(db, "messages")
	if err != nil {
		return "", err
	}
	return hermesShownFrom(columns, alias), nil
}

// hermesShownFrom is hermesShownClause for a messages schema already read
func hermesShownFrom(columns map[string]bool, alias string) string {
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
	err := readHermesDB(dbPath, func(db *sql.DB) (err error) {
		out, err = hermesMessagesFrom(db, sessionID, q)
		return err
	})
	if err != nil {
		warnHermesSQLite("messages", sessionID, err)
	}
	return out
}

// hermesMessagesFrom is one attempt at hermesSQLiteMessages's read. A failure part-way
// returns the messages emitted so far with the error, as the read always has.
func hermesMessagesFrom(db *sql.DB, sessionID string, q MessageQuery) ([]map[string]any, error) {
	columns, err := tableColumns(db, "messages")
	if err != nil {
		return []map[string]any{}, err
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
		WHERE session_id = ?`+hermesShownFrom(columns, "")+`
		  AND role IN ('user', 'assistant', 'tool')
		ORDER BY id ASC`, sessionID)
	if err != nil {
		return []map[string]any{}, err
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
			return []map[string]any{}, err
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
		return []map[string]any{}, err
	}
	// The pool is one connection (SetMaxOpenConns in openHermesDB), so the first read has
	// to be finished before the second pass can query
	rows.Close()

	// Pass two: fetch just the winning rows, in chunks and in shown order, and emit them
	// through the sink. Pass one only read; what stays resident is the window plus one
	// chunk of rows, not the session's text (identity needs every row read once, so the
	// scan itself is still one pass over the session).
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
		nullIDs := false
		for _, ref := range order[start:end] {
			placeholders = append(placeholders, "?")
			args = append(args, ref.id)
			if ref.id == "" {
				nullIDs = true // a NULL id column, which IN (?) can never match
			}
		}
		nullClause := ""
		if nullIDs {
			// SQLite allows NULL under PRIMARY KEY; those rows come back as id ""
			nullClause = " OR id IS NULL"
		}
		page, err := db.Query(`
			SELECT id, role, content, `+col("reasoning")+`, `+col("tool_calls")+`, `+col("tool_name")+`,
				   `+col("tool_call_id")+`, timestamp
			FROM messages
			WHERE session_id = ? AND (id IN (`+strings.Join(placeholders, ",")+`)`+nullClause+`)`, args...)
		if err != nil {
			return sink.result(), err
		}
		type fullRow struct {
			role, content, reasoning, toolCalls, toolName, toolCallID string
			timestamp                                                 any
		}
		fetched := map[string]fullRow{}
		byID := map[string]fullRow{} // fallback for a row rewritten between the passes
		for page.Next() {
			var id, role, content, reasoning, toolCalls, toolName, toolCallID, timestamp any
			if err := page.Scan(&id, &role, &content, &reasoning, &toolCalls, &toolName, &toolCallID, &timestamp); err != nil {
				page.Close()
				return sink.result(), err
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
			byID[sqliteValueString(id)] = row
		}
		page.Close()
		if err := page.Err(); err != nil {
			// A mid-page read error is not a row that changed: say so instead of letting
			// the missing refs pass for concurrent writes
			return sink.result(), err
		}
		for _, ref := range order[start:end] {
			row, ok := fetched[ref.id+"\x00"+ref.key]
			if !ok {
				// A row rewritten between the passes (a live compaction) has a new identity
				// hash; the row with that id is still the one that belongs here. A deleted
				// row has nothing, and the message is dropped rather than guessed
				row, ok = byID[ref.id]
			}
			if !ok {
				continue
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
				return sink.result(), nil
			}
		}
	}
	return sink.result(), nil
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
	var out []map[string]any
	err := readHermesDB(dbPath, func(db *sql.DB) (err error) {
		out, err = hermesSearchFrom(ctx, db, sessionID, q)
		return err
	})
	// A search that stopped because its caller did — the scan cancels what is still
	// running once it has enough hits, and a reader can leave — has nothing to report: those
	// were the "context canceled" warnings in the log
	if err != nil && ctx.Err() == nil {
		warnHermesSQLite("search hits", sessionID, err)
	}
	return out
}

// hermesSearchFrom is one attempt at hermesSQLiteSearch's read
func hermesSearchFrom(ctx context.Context, db *sql.DB, sessionID string, q SearchQuery) ([]map[string]any, error) {
	shown, err := hermesShownClause(db, "")
	if err != nil {
		return nil, err
	}
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
		WHERE session_id = ?`+shown+roleClause+`
		  AND (content LIKE ? ESCAPE '\' OR reasoning LIKE ? ESCAPE '\')
		ORDER BY timestamp ASC, id ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var role, content, reasoning sql.NullString
		var timestamp any
		if err := rows.Scan(&role, &content, &reasoning, &timestamp); err != nil {
			return out, err
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
	return out, rows.Err()
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

// warnHermesSQLite reports a read of state.db that failed for good — after its retries —
// naming what was being read; every read used to report itself as the final message
func warnHermesSQLite(what, sessionID string, err error) {
	if sessionID == "" {
		fmt.Fprintf(os.Stderr, "[WARN] reading %s from Hermes's state.db failed: %v\n", what, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[WARN] reading %s from Hermes's state.db failed for %s: %v\n", what, sessionID, err)
}
