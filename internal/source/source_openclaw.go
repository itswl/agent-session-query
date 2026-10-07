package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo, cross-compilation unaffected)
)

// OpenClaw source, 2026.9 layout: one SQLite database per agent at
// ~/.openclaw/agents/<id>/agent/openclaw-agent.sqlite. There is no sessions.json and no
// jsonl any more — the transcript rows moved into the transcript_events table with their
// JSON shape unchanged (type=session / message / model_change rows, message.content a
// string or a block array).
//
// Verified against the on-disk schema and real rows:
//
//	session_windows:   session_id, session_key, status (running/done/failed/killed/
//	                   timeout), started_at / ended_at / updated_at /
//	                   transcript_updated_at (unix milliseconds), model_provider, model,
//	                   display_name
//	transcript_events: (session_id, seq) rows of event_json; the type=session row carries
//	                   the cwd, message rows carry message.role / message.content /
//	                   message.stopReason / message.usage
//
// A tool result arrives as its own message with role "toolResult", carrying toolCallId
// and toolName, which is exactly what the shared toolResult block wants.
//
// The pre-SQLite layout (~/.openclaw/agents/default/sessions/sessions.json plus a jsonl
// per session) still exists in older installs, so it stays as the fallback when no
// agent database is found (see BuildSources).
type OpenClawSource struct {
	dbPaths []string

	// listErr is the failure the last List() hit (see ListErrorReporter): a database
	// that cannot be opened or queried must read as "could not be read", not as
	// "nothing there".
	mu      sync.Mutex
	listErr error
}

// setListError records how the last list went; nil clears a previous failure
func (s *OpenClawSource) setListError(err error) {
	s.mu.Lock()
	s.listErr = err
	s.mu.Unlock()
}

// ListError implements ListErrorReporter
func (s *OpenClawSource) ListError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listErr
}

func NewOpenClawSource(dbPaths []string) *OpenClawSource {
	return &OpenClawSource{dbPaths: dbPaths}
}

// openClawAgentDBs finds every agent database under ~/.openclaw/agents. The layout is
// uniform (~/.openclaw/agents/<id>/agent/openclaw-agent.sqlite), so additional isolated
// agents pick up without further code.
func openClawAgentDBs(home string) []string {
	matches, _ := filepath.Glob(filepath.Join(home, ".openclaw", "agents", "*", "agent", "openclaw-agent.sqlite"))
	return matches
}

func (s *OpenClawSource) Mode() string { return "openclaw" }
func (s *OpenClawSource) Location() string {
	if len(s.dbPaths) == 1 {
		return s.dbPaths[0]
	}
	return strings.Join(s.dbPaths, ", ")
}
func (s *OpenClawSource) Exists() bool { return len(s.dbPaths) > 0 }

func (s *OpenClawSource) open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteURI(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (s *OpenClawSource) List() []Record {
	out := []Record{}
	var listErr error
	for _, dbPath := range s.dbPaths {
		recs, err := s.listOne(dbPath)
		out = append(out, recs...)
		if err != nil && listErr == nil {
			listErr = err
		}
	}
	s.setListError(listErr)
	return out
}

func (s *OpenClawSource) listOne(dbPath string) ([]Record, error) {
	db, err := s.open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT session_id, status, model_provider, model, display_name,
		       COALESCE(transcript_updated_at, updated_at),
		       (SELECT COUNT(*) FROM transcript_events te
		         WHERE te.session_id = session_windows.session_id
		           AND json_extract(te.event_json, '$.type') = 'message'),
		       (SELECT json_extract(te.event_json, '$.cwd')
		          FROM transcript_events te
		         WHERE te.session_id = session_windows.session_id
		           AND json_extract(te.event_json, '$.type') = 'session'
		         ORDER BY te.seq LIMIT 1) AS cwd,
		       (SELECT te.event_json
		          FROM transcript_events te
		         WHERE te.session_id = session_windows.session_id
		           AND json_extract(te.event_json, '$.type') = 'message'
		           AND json_extract(te.event_json, '$.message.role') = 'user'
		         ORDER BY te.seq LIMIT 1) AS first_user
		FROM session_windows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := []Record{}
	for rows.Next() {
		var id, status, provider, model, displayName, cwd, firstUser sql.NullString
		var updated, messageCount sql.NullInt64
		if err := rows.Scan(&id, &status, &provider, &model, &displayName,
			&updated, &messageCount, &cwd, &firstUser); err != nil {
			return list, err
		}
		if !id.Valid || id.String == "" {
			continue
		}

		// display_name is set for chat-channel sessions; CLI runs leave it empty and the
		// first user message is the title, as with the file sources. It is text a CLI
		// wrote, so it is cleaned like every assembled title (the fallback below already
		// goes through TitleFromUserText, which redacts).
		name := RedactSecrets(StripTerminalControls(displayName.String))
		if name == "" && firstUser.Valid {
			var event struct {
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(firstUser.String), &event) == nil {
				if text, ok := blockArrayText(event.Message.Content); ok {
					name = TitleFromUserText(text)
				}
			}
		}

		fullModel := model.String
		if provider.Valid && provider.String != "" && fullModel != "" {
			fullModel = provider.String + "/" + fullModel
		}

		list = append(list, NewRecord(Record{
			Source:    "openclaw",
			Key:       dbPath + "#" + id.String,
			ShortKey:  name,
			SessionID: id.String,
			Status:    StrOr(status.String, "done"),
			Cwd:       cwd.String,
			Model:     fullModel,
			// transcript_events is keyed (session_id, seq), so the count is an index scan
			MessageCount: int(messageCount.Int64),
			HasCount:     true,
			UpdatedAt:    millisToISO(updated),
		}, millisToISO(updated)))
	}
	if err := rows.Err(); err != nil {
		return list, err
	}
	return list, nil
}

func (s *OpenClawSource) Messages(r Record, q MessageQuery) []map[string]any {
	sink := newMessageSink(q)
	sessionID := r.SessionID
	if sessionID == "" {
		return sink.result()
	}
	rows, close, err := s.querySession(context.Background(), r, `
		SELECT event_json FROM transcript_events
		WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return sink.result()
	}
	defer close()

	for rows.Next() {
		var eventJSON string
		if err := rows.Scan(&eventJSON); err != nil {
			return sink.result()
		}
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Role       string `json:"role"`
				Timestamp  any    `json:"timestamp"`
				ToolName   string `json:"toolName"`
				ToolCallID string `json:"toolCallId"`
				IsError    bool   `json:"isError"`
				Content    any    `json:"content"`
				Model      string `json:"model"`
			} `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(eventJSON), &event) != nil || event.Type != "message" {
			continue
		}
		m := event.Message

		blocks := []map[string]any{}
		switch m.Role {
		case "toolResult":
			// A tool result is its own message; keep it one, as a single toolResult
			// block — toolName, toolCallId and isError travel on the message
			text, _ := blockArrayText(m.Content)
			block := ToolResultBlock(m.ToolCallID, m.ToolName, text, q.Full)
			blocks = append(blocks, ToolOutcome{Status: statusFromError(m.IsError)}.Apply(block))
		default:
			if content, ok := m.Content.([]any); ok {
				for _, item := range content {
					block, ok := item.(map[string]any)
					if !ok {
						continue
					}
					switch block["type"] {
					case "text":
						blocks = append(blocks, TextBlock(strField(block, "text")))
					case "toolCall":
						blocks = append(blocks, ToolCallBlock(strField(block, "id"), strField(block, "name"), GetOr(block, "arguments", map[string]any{})))
					case "thinking":
						blocks = append(blocks, ThinkingBlock(strField(block, "thinking"), q.Full))
					default:
						cut, _ := clip(ContentText(block), toolResultLimit, q.Full)
						blocks = append(blocks, map[string]any{
							"type":    StrOr(block["type"], "unknown"),
							"content": cut,
						})
					}
				}
			} else if text, ok := m.Content.(string); ok && text != "" {
				blocks = append(blocks, TextBlock(text))
			}
		}

		ts := event.Timestamp
		if ts == "" {
			ts = openClawEventMillis(m.Timestamp)
		}
		sink.add(map[string]any{
			"role":      StrOr(m.Role, "unknown"),
			"timestamp": ts,
			"content":   blocks,
		})
	}
	return sink.result()
}

// openClawEventMillis formats the message-level numeric timestamp; the event-level one is
// already an ISO string and is preferred.
func openClawEventMillis(v any) string {
	if ms, ok := ToFloat(v); ok {
		return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05")
	}
	return ""
}

func (s *OpenClawSource) Final(r Record) map[string]any {
	sessionID := r.SessionID
	if sessionID == "" {
		return nil
	}
	// The count runs first and on its own connection: opened while the message query's
	// rows are still live it came back empty, and the error that said so was being
	// discarded, so the session reported a messageCount of 0 next to a full transcript.
	count := 0
	if countRows, closeCount, err := s.querySession(context.Background(), r,
		`SELECT COUNT(*) FROM transcript_events
		 WHERE session_id = ? AND json_extract(event_json, '$.type') = 'message'`, sessionID); err == nil {
		// Next() before Scan(): a *sql.Rows has to be advanced to its first row, and
		// scanning without that fails — which the discarded error had been hiding, so the
		// session reported 0 messages beside a full transcript.
		if countRows.Next() {
			if err := countRows.Scan(&count); err != nil {
				count = 0
			}
		}
		closeCount()
	}

	rows, close, err := s.querySession(context.Background(), r, `
		SELECT event_json FROM transcript_events
		WHERE session_id = ?
		  AND json_extract(event_json, '$.message.role') = 'assistant'
		ORDER BY seq DESC
		LIMIT 10`, sessionID)
	if err != nil {
		return nil
	}
	defer close()

	for rows.Next() {
		var eventJSON string
		if rows.Scan(&eventJSON) != nil {
			break
		}
		var event struct {
			Message struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
				Model      string `json:"model"`
				Timestamp  any    `json:"timestamp"`
				Content    any    `json:"content"`
				Usage      struct {
					Input      int64 `json:"input"`
					Output     int64 `json:"output"`
					CacheRead  int64 `json:"cacheRead"`
					CacheWrite int64 `json:"cacheWrite"`
					Cost       struct {
						Total float64 `json:"total"`
					} `json:"cost"`
				} `json:"usage"`
			} `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(eventJSON), &event) != nil {
			continue
		}
		m := event.Message
		// toolUse means the turn ended to run tools; the result of the session is the
		// message that actually stopped
		if m.StopReason != "" && m.StopReason != "toolUse" {
			rows.Close()
			texts := []string{}
			if content, ok := m.Content.([]any); ok {
				for _, item := range content {
					if block, ok := item.(map[string]any); ok && block["type"] == "text" {
						if t := strField(block, "text"); t != "" {
							texts = append(texts, t)
						}
					}
				}
			}
			ts := event.Timestamp
			if ts == "" {
				ts = openClawEventMillis(m.Timestamp)
			}
			return map[string]any{
				"status":       "done",
				"isFinal":      m.StopReason == "stop",
				"isProcessing": false,
				"messageCount": count,
				"source":       "openclaw",
				"timestamp":    ts,
				"stopReason":   m.StopReason,
				"model":        m.Model,
				"text":         strings.Join(texts, "\n"),
				"thinking":     "",
				"toolCalls":    []any{},
				"usage": map[string]any{
					"inputTokens":      m.Usage.Input,
					"outputTokens":     m.Usage.Output,
					"cacheReadTokens":  m.Usage.CacheRead,
					"cacheWriteTokens": m.Usage.CacheWrite,
					"estimatedCostUsd": m.Usage.Cost.Total,
				},
			}
		}
	}
	rows.Close()
	return map[string]any{
		"status":       "done",
		"isFinal":      false,
		"isProcessing": false,
		"messageCount": count,
		"source":       "openclaw",
		"text":         "",
		"thinking":     "",
	}
}

// Search implements searchableSource: the transcript is in SQLite, so LIKE over the
// event bodies.
func (s *OpenClawSource) Search(ctx context.Context, r Record, q SearchQuery) []map[string]any {
	sessionID := r.SessionID
	if sessionID == "" || len(q.Lowered) == 0 {
		return nil
	}
	// The role filter goes into the query rather than onto the result: filtering the rows
	// the LIMIT already returned would keep whichever hits came first.
	roleClause := ""
	if q.Role != "" {
		roleClause = "\n\t\t  AND json_extract(event_json, '$.message.role') = '" + EscapeLike(q.Role) + "'"
	}
	rows, close, err := s.querySession(ctx, r, `
		SELECT event_json FROM transcript_events
		WHERE session_id = ? AND event_json LIKE ? ESCAPE '\'`+roleClause+`
		ORDER BY seq
		LIMIT ?`, sessionID, "%"+EscapeLike(string(q.Lowered))+"%", q.ProbeLimit())
	if err != nil {
		return nil
	}
	defer close()

	out := []map[string]any{}
	for rows.Next() {
		var eventJSON string
		if rows.Scan(&eventJSON) != nil {
			return out
		}
		var event struct {
			Message struct {
				Role      string `json:"role"`
				Content   any    `json:"content"`
				Timestamp any    `json:"timestamp"`
			} `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(eventJSON), &event) != nil || event.Message.Role == "" {
			continue
		}
		text, _ := blockArrayText(event.Message.Content)
		if text == "" {
			continue
		}
		ts := event.Timestamp
		if ts == "" {
			ts = openClawEventMillis(event.Message.Timestamp)
		}
		out = append(out, map[string]any{
			"role":      event.Message.Role,
			"snippet":   SnippetAround(text, string(q.Lowered), SearchSnippetRadius),
			"timestamp": ts,
		})
	}
	return out
}

// querySession opens the database the record came from. The key is "<db>#<session>", so
// cutting the suffix recovers the path; the returned closer must run after the rows.
func (s *OpenClawSource) querySession(ctx context.Context, r Record, query string, args ...any) (*sql.Rows, func(), error) {
	key := r.Key
	sessionID := r.SessionID
	dbPath := strings.TrimSuffix(key, "#"+sessionID)
	db, err := s.open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return rows, func() { rows.Close(); db.Close() }, nil
}
