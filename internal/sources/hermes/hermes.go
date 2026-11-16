// Package hermes reads Hermes's sessions.
//
// Hermes has kept them two ways, and an install can hold both:
//
//	<home>/state.db                     everything, in SQLite: sessions and messages
//	<home>/sessions/sessions.json       the older index, and beside it
//	<home>/sessions/<session id>.jsonl    a transcript per session
//
// A session is listed once, by its id. Where a transcript file exists it is what is
// read — it is the older, complete record — and otherwise the database is; what the
// session row says about the session (its title, directory, branch, totals) is used
// either way.
package hermes

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Hermes.
var Spec = sources.Spec{
	Name:  "hermes",
	Title: "Hermes",
	Locate: func(env sources.Env) string {
		// HERMES_HOME moves the whole ~/.hermes: sessions, state.db and configuration
		if d := env.Getenv("HERMES_HOME"); d != "" {
			return d
		}
		return filepath.Join(env.Home, ".hermes")
	},
	Open: func(location string) sources.Source {
		return &Source{home: location, db: filepath.Join(location, "state.db"),
			index: jsonmap.At(filepath.Join(location, "sessions"))}
	},
	// The store is a database and a directory beside it: no one path relocates it
	Movable: false,
	Resume:  "hermes --resume",
	Store:   "~/.hermes/state.db (or ~/.hermes/sessions/sessions.json + <id>.jsonl)",
}

// Source is one Hermes home.
type Source struct {
	home  string
	db    string
	index *jsonmap.Index
	list  kit.Snapshot[[]listed]
}

// listed is one session of the database, as a scan needs it.
type listed struct {
	id    string
	stamp string
	at    time.Time
}

func (s *Source) Location() string { return s.home }

// Exists: either store is enough. Newer Hermes writes no sessions.json at all.
func (s *Source) Exists() bool { return kit.Exists(s.db) || s.index.Exists() }

// Scan lists every session once: those of the database, and those only the index knows
// that have a transcript to read.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	entries, indexErr := s.index.Entries()
	byID := make(map[string]jsonmap.Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	var refs []sources.Ref
	inDB := map[string]bool{}
	if kit.Exists(s.db) {
		rows, err := s.list.Load(s.db, func() ([]listed, error) { return s.listDB(ctx) })
		if err != nil {
			// A database that cannot be read is not an empty one — and the failure looks
			// exactly like a Hermes with no sessions in it
			return nil, err
		}
		for _, row := range rows {
			inDB[row.id] = true
			if e, ok := byID[row.id]; ok && e.File != "" {
				// The transcript file is what is read; the stamp still moves with the
				// session's row, where its title and totals are
				ref := s.index.Ref(e)
				ref.Stamp += ";" + row.stamp
				refs = append(refs, ref)
				continue
			}
			refs = append(refs, sources.Ref{Key: s.db + "#" + row.id, Stamp: row.stamp, ModTime: row.at})
		}
	} else if indexErr != nil {
		return nil, indexErr
	}
	for _, e := range entries {
		// A record with neither a row nor a file names a session with nothing to read
		if !inDB[e.ID] && e.File != "" {
			refs = append(refs, s.index.Ref(e))
		}
	}
	return refs, nil
}

// listDB lists the database's sessions with one query. Each session's stamp is built from
// what its own rows say about change — its newest message, how many it has, when it
// ended, and the fields the list shows — so a message in one session leaves every other
// session's stamp where it was.
func (s *Source) listDB(ctx context.Context) ([]listed, error) {
	var out []listed
	err := kit.ReadDB(ctx, s.db, func(db *sql.DB) error {
		out = out[:0]
		cols, err := kit.Columns(db, "sessions")
		if err != nil {
			return err
		}
		if !cols["id"] {
			return errors.New("state.db has no sessions table this reads")
		}
		// A column this Hermes does not have is read as NULL, so one statement fits every
		// version seen
		col := func(name string) string {
			if cols[name] {
				return "s." + name
			}
			return "NULL"
		}
		rows, err := db.QueryContext(ctx, `
SELECT s.id,
       (SELECT MAX(m.rowid) FROM messages m WHERE m.session_id = s.id),
       (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = s.id),
       `+col("message_count")+`, `+col("started_at")+`, `+col("ended_at")+`, `+col("last_activity_at")+`,
       `+col("title")+`, `+col("display_name")+`, `+col("archived")+`, `+col("hidden")+`,
       `+col("input_tokens")+`, `+col("output_tokens")+`, `+col("transcript_revision")+`
FROM sessions s`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id sql.NullString
			var maxID, count, archived, hidden, in, outTok, rev sql.NullInt64
			var last, started, ended, activity sql.NullFloat64
			var title, name sql.NullString
			if err := rows.Scan(&id, &maxID, &last, &count, &started, &ended, &activity, &title, &name,
				&archived, &hidden, &in, &outTok, &rev); err != nil {
				return err
			}
			if id.String == "" {
				continue
			}
			at := max(last.Float64, activity.Float64, ended.Float64, started.Float64)
			out = append(out, listed{
				id: id.String,
				stamp: strings.Join([]string{
					strconv.FormatInt(maxID.Int64, 36), strconv.FormatInt(count.Int64, 36),
					strconv.FormatInt(int64(ended.Float64), 36), strconv.FormatInt(int64(activity.Float64), 36),
					strconv.FormatInt(archived.Int64*2+hidden.Int64, 36), strconv.FormatInt(in.Int64+outTok.Int64, 36),
					strconv.FormatInt(rev.Int64, 36), jsonmap.StampOf(title.String, name.String),
				}, "."),
				at: kit.Epoch(at),
			})
		}
		return rows.Err()
	})
	return out, err
}

// Read parses one session: from its transcript file when it has one, from the database
// otherwise.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	if db, id, ok := strings.Cut(ref.Key, "#"); ok && db == s.db {
		w.S.ID, w.S.Path = id, s.db
		var msgs []transcript.Message
		err := kit.ReadDB(ctx, s.db, func(db *sql.DB) error {
			// A read that met a write in flight is run again: what the first try gathered
			// is dropped, and nothing reaches the writer until one has finished
			msgs = msgs[:0]
			found, err := sessionRow(ctx, db, id, &w.S)
			if err != nil {
				return err
			}
			if !found {
				return sql.ErrNoRows
			}
			msgs, err = messages(ctx, db, id, w.S.Model)
			return err
		})
		if err != nil {
			return err
		}
		for _, m := range msgs {
			w.Add(m)
		}
		return nil
	}

	// A transcript file, and what the index — and the database, when the session is there
	// too — says about it
	entry, _ := s.index.ByFile(ref.Key)
	w.S.Path = ref.Key
	w.S.ID = kit.First(entry.ID, strings.TrimSuffix(filepath.Base(ref.Key), ".jsonl"))
	if entry.Info != nil {
		info := entry.Info
		w.S.Title = kit.Str(info["display_name"])
		w.S.Platform = kit.Str(info["platform"])
		w.S.Model = kit.Str(info["model"])
		w.S.StartedAt, w.S.UpdatedAt = entry.Created(), entry.Updated()
		// Hermes totals a session rather than its responses: the totals stand in for a sum
		// nothing can compute
		u := model.Usage{Input: kit.Int(info["input_tokens"]), Output: kit.Int(info["output_tokens"]),
			CacheRead: kit.Int(info["cache_read_tokens"]), CacheWrite: kit.Int(info["cache_write_tokens"])}
		cost, _ := kit.Num(info["estimated_cost_usd"])
		if !u.IsZero() || cost > 0 {
			w.S.Recorded = &model.Recorded{Usage: &u, CostUSD: cost}
		}
	}
	if kit.Exists(s.db) {
		_ = kit.ReadDB(ctx, s.db, func(db *sql.DB) error {
			_, err := sessionRow(ctx, db, w.S.ID, &w.S)
			return err
		})
	}
	reader := &jsonmap.Reader{Model: w.S.Model}
	if err := reader.ReadFile(ctx, ref.Key, w); err != nil {
		return err
	}
	w.S.Cwd = kit.First(w.S.Cwd, reader.Cwd)
	w.S.Platform = kit.First(w.S.Platform, reader.Platform)
	w.S.Model = kit.First(w.S.Model, reader.Model)
	return nil
}

// sessionRow fills in what the database's session row says, and reports whether there is
// one. Fields the row leaves empty keep whatever the session already had.
func sessionRow(ctx context.Context, db *sql.DB, id string, sess *model.Session) (bool, error) {
	cols, err := kit.Columns(db, "sessions")
	if err != nil {
		return false, err
	}
	want := []string{"title", "display_name", "source", "model", "cwd", "git_branch", "started_at", "ended_at",
		"last_activity_at", "archived", "hidden", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_tokens", "reasoning_tokens", "estimated_cost_usd", "actual_cost_usd"}
	var have []string
	for _, name := range want {
		if cols[name] {
			have = append(have, name)
		}
	}
	if len(have) == 0 {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(have, ", ")+`,
  (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id) FROM sessions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	vals := make([]any, len(have)+1)
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return false, err
	}
	row := map[string]any{}
	for i, name := range have {
		if b, ok := vals[i].([]byte); ok {
			row[name] = string(b)
		} else {
			row[name] = vals[i]
		}
	}
	num := func(name string) float64 { n, _ := kit.Num(row[name]); return n }

	// Hermes writes a title for a session (title_source says who made it); display_name is
	// the older field
	sess.Title = kit.First(kit.Str(row["title"]), kit.Str(row["display_name"]), sess.Title)
	sess.Platform = kit.First(kit.Str(row["source"]), sess.Platform)
	sess.Model = kit.First(kit.Str(row["model"]), sess.Model)
	sess.Cwd = kit.First(kit.Str(row["cwd"]), sess.Cwd)
	sess.Branch = kit.First(kit.Str(row["git_branch"]), sess.Branch)
	if t := kit.Epoch(num("started_at")); !t.IsZero() {
		sess.StartedAt = t
	}
	// Last written: the newest message, then what the row says of its activity and its end
	if last, _ := kit.Num(vals[len(have)]); last > 0 {
		sess.UpdatedAt = kit.Epoch(last)
	} else if t := kit.Epoch(max(num("last_activity_at"), num("ended_at"), num("started_at"))); !t.IsZero() {
		sess.UpdatedAt = t
	}
	// The sessions Hermes itself puts away. Which column marks them has moved between
	// versions — Bot Mode sessions were marked in hidden, later ones in archived — and a
	// database can carry both: either one is "put away"
	sess.Archived = num("archived") != 0 || num("hidden") != 0
	// Hermes keeps its counters per session, already apart: the input leaves the cache
	// out. They are the session's Recorded totals — no response carries any.
	u := model.Usage{Input: int64(num("input_tokens")), Output: int64(num("output_tokens")),
		CacheRead: int64(num("cache_read_tokens")), CacheWrite: int64(num("cache_write_tokens")),
		Reasoning: int64(num("reasoning_tokens"))}
	cost := num("actual_cost_usd")
	if cost == 0 {
		cost = num("estimated_cost_usd")
	}
	if !u.IsZero() || cost > 0 {
		sess.Recorded = &model.Recorded{Usage: &u, CostUSD: cost}
	}
	return true, nil
}

// shown is the condition that keeps the message rows Hermes itself displays, built from
// the columns this database has.
//
// Hermes marks rows two ways. Undo, rewind and regenerate set active = 0, and the row is
// gone from the conversation. Context compression also sets active = 0 on the rows it
// folded into a summary, but marks them compacted = 1 — and Hermes keeps showing those:
// the person did say and read them, only the model stopped seeing them. Filtering on
// active alone hides the older half of every compressed session. So a row is shown when
// it is live or compacted. A database without the compacted column keeps the active-only
// rule, and one with neither is left unfiltered.
func shown(cols map[string]bool) string {
	switch {
	case cols["active"] && cols["compacted"]:
		return " AND (COALESCE(active, 1) = 1 OR COALESCE(compacted, 0) = 1)"
	case cols["active"]:
		return " AND COALESCE(active, 1) = 1"
	}
	return ""
}

// messages reads a session's rows the way Hermes reads them for display: in insertion
// order — timestamps can regress, since compression re-inserts rows with their original
// time — filtered to what Hermes shows, and collapsed on Hermes's own display identity.
// Compression re-inserts the protected head and tail of a conversation as live copies of
// archived originals, so a row is one message whether the database holds it once or
// twice: the first position is kept, and the most live copy is what is shown there.
func messages(ctx context.Context, db *sql.DB, id, sessionModel string) ([]transcript.Message, error) {
	cols, err := kit.Columns(db, "messages")
	if err != nil {
		return nil, err
	}
	col := func(name string) string {
		if cols[name] {
			return name
		}
		return "NULL"
	}
	reasoning := col("reasoning")
	if !cols["reasoning"] {
		reasoning = col("reasoning_content")
	}
	rows, err := db.QueryContext(ctx, `
SELECT rowid, id, role, content, `+reasoning+`, `+col("tool_calls")+`, `+col("tool_name")+`, `+col("tool_call_id")+`,
       `+col("active")+`, `+col("display_metadata")+`, `+col("display_kind")+`, `+col("_compressed_summary")+`,
       `+col("finish_reason")+`, timestamp
FROM messages WHERE session_id = ?`+shown(cols)+` AND role IN ('user', 'assistant', 'tool') ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rowOf struct {
		msg    transcript.Message
		active int64
		id     int64
	}
	var order []rowOf
	at := map[[sha256.Size]byte]int{}
	for rows.Next() {
		// The row's position is its rowid — the id column is an integer in current Hermes
		// and was text before, and nothing forces it to be unique
		var rid, active, summary sql.NullInt64
		var role, content, think, calls, tool, callID, meta, kind, finish sql.NullString
		var mid, ts any
		if err := rows.Scan(&rid, &mid, &role, &content, &think, &calls, &tool, &callID, &active, &meta, &kind, &summary, &finish, &ts); err != nil {
			return nil, err
		}
		// Rows the model reads but nobody typed, and the ones Hermes itself leaves out
		if modelOnly(meta.String) || kind.String == "hidden" {
			continue
		}
		if b, ok := mid.([]byte); ok {
			mid = string(b)
		}
		if b, ok := ts.([]byte); ok {
			ts = string(b)
		}
		m := transcript.Message{ID: kit.Text(mid), At: kit.Time(ts)}
		switch role.String {
		case "user":
			m.Role = transcript.User
			if content.String != "" {
				m.Blocks = []transcript.Block{transcript.TextOf(content.String)}
			}
		case "assistant":
			m.Role = transcript.Assistant
			m.Model, m.Stop = sessionModel, finish.String
			if summary.Int64 != 0 {
				// The summary compression put in the place of what it folded away
				m.Role = transcript.System
				m.Blocks = []transcript.Block{transcript.EventOf(model.EventCompaction, content.String)}
			} else {
				m.Blocks = jsonmap.Assistant(think.String, content.String, []byte(calls.String))
			}
		case "tool":
			// One row is one result: the content is the tool's output, tool_name which tool
			// ran and tool_call_id which call it answers. Hermes does not record whether it
			// succeeded, so the result carries no status.
			m.Role = transcript.ToolRole
			m.Blocks = []transcript.Block{jsonmap.Result(callID.String, tool.String, content.String)}
		}
		if len(m.Blocks) == 0 {
			continue
		}
		live := int64(1)
		if active.Valid {
			live = active.Int64
		}
		// The identity Hermes collapses on: who said it, what, when, and which call
		key := sha256.Sum256([]byte(strings.Join([]string{role.String, content.String, timeKey(ts), callID.String, calls.String, tool.String}, "\x00")))
		if i, seen := at[key]; seen {
			if kept := order[i]; live > kept.active || (live == kept.active && rid.Int64 > kept.id) {
				order[i] = rowOf{m, live, rid.Int64}
			}
			continue
		}
		at[key] = len(order)
		order = append(order, rowOf{m, live, rid.Int64})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]transcript.Message, len(order))
	for i, r := range order {
		out[i] = r.msg
	}
	return out, nil
}

// timeKey renders a timestamp column for the identity: the same instant reads the same
// whether SQLite hands it back as a float or as text.
func timeKey(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if f, ok := kit.Num(v); ok {
		return strconv.FormatFloat(f, 'f', 6, 64)
	}
	return kit.Text(v)
}

// modelOnly reads display_metadata.model_only, the mark on rows Hermes shows the model
// but not the person. The column holds JSON, sometimes JSON-encoded twice.
func modelOnly(raw string) bool {
	if raw == "" {
		return false
	}
	var decoded any = raw
	for range 2 {
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
	return kit.Truthy(kit.Map(decoded)["model_only"])
}
