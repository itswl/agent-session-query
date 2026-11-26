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
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// 3: the row that closes a failed turn is the error its turn ended on, not a reply
	// 4: what Hermes writes in the user's seat of its own opens no turn
	Reads: 4,
	Store: "~/.hermes/state.db (or ~/.hermes/sessions/sessions.json + <id>.jsonl)",
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
			// The figures are read as whatever SQLite holds, since a column takes a value of
			// any type: one row's timestamp written as text — which MAX then returns, text
			// sorting above every number — or a count with a fraction is read as none, and
			// does not make the store look unreadable
			var id, title, name sql.NullString
			var maxID, last, count, started, ended, activity, archived, hidden, in, outTok, rev any
			if err := rows.Scan(&id, &maxID, &last, &count, &started, &ended, &activity, &title, &name,
				&archived, &hidden, &in, &outTok, &rev); err != nil {
				return err
			}
			if id.String == "" {
				continue
			}
			at := max(number(last), number(activity), number(ended), number(started))
			out = append(out, listed{
				id: id.String,
				stamp: strings.Join([]string{
					strconv.FormatInt(kit.Int(maxID), 36), strconv.FormatInt(kit.Int(count), 36),
					strconv.FormatInt(kit.Int(ended), 36), strconv.FormatInt(kit.Int(activity), 36),
					strconv.FormatInt(kit.Int(archived)*2+kit.Int(hidden), 36), strconv.FormatInt(kit.Int(in)+kit.Int(outTok), 36),
					strconv.FormatInt(kit.Int(rev), 36), jsonmap.StampOf(title.String, name.String),
				}, "."),
				at: kit.Epoch(at),
			})
		}
		return rows.Err()
	})
	return out, err
}

// flag reads a flag column scanned as whatever it held: a number as itself, text that is
// one as the number, and anything else — NULL, a blob, a word — as missing.
func flag(v any, missing int64) int64 {
	switch t := v.(type) {
	case []byte:
		v = string(t)
	}
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return missing
		}
		v = f
	}
	if n, ok := kit.Num(v); ok && n >= math.MinInt64 && n < math.MaxInt64 {
		return int64(n)
	}
	return missing
}

// number reads a figure of a column scanned as whatever it held: a number as itself, and
// anything else — text, a blob, NULL — as 0.
func number(v any) float64 {
	n, _ := kit.Num(v)
	return n
}

// Read parses one session: from its transcript file when it has one, from the database
// otherwise.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	// The session id is what follows the database's path and a #: the home's path may
	// hold a # of its own, and so may an id
	if id, ok := strings.CutPrefix(ref.Key, s.db+"#"); ok {
		w.S.ID, w.S.Path = id, s.db
		var msgs []transcript.Message
		var unread []string
		err := kit.ReadDB(ctx, s.db, func(db *sql.DB) error {
			// A read that met a write in flight is run again: what the first try gathered
			// is dropped, and nothing reaches the writer until one has finished
			msgs, unread = msgs[:0], unread[:0]
			found, err := sessionRow(ctx, db, id, &w.S)
			if err != nil {
				return err
			}
			if !found {
				return sql.ErrNoRows
			}
			msgs, unread, err = messages(ctx, db, id, w.S.Model)
			return err
		})
		if err != nil {
			return err
		}
		for _, m := range msgs {
			w.Add(m)
		}
		for _, kind := range unread {
			w.Unread(kind)
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
		u := model.Usage{Input: kit.Count(info["input_tokens"]), Output: kit.Count(info["output_tokens"]),
			CacheRead: kit.Count(info["cache_read_tokens"]), CacheWrite: kit.Count(info["cache_write_tokens"])}
		cost := jsonmap.Cost(info["estimated_cost_usd"])
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
	reader := &jsonmap.Reader{Model: w.S.Model, Unread: w.Unread}
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
	num := func(name string) float64 { return number(row[name]) }
	count := func(name string) int64 { return kit.Count(row[name]) }

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
	u := model.Usage{Input: count("input_tokens"), Output: count("output_tokens"),
		CacheRead: count("cache_read_tokens"), CacheWrite: count("cache_write_tokens"),
		Reasoning: count("reasoning_tokens")}
	// What it cost, where that is a price (see jsonmap.Cost): what the provider billed,
	// and failing that what Hermes estimated
	cost := jsonmap.Cost(row["actual_cost_usd"])
	if cost == 0 {
		cost = jsonmap.Cost(row["estimated_cost_usd"])
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
//
// The second result names every row passed over for a role this does not know, for
// transcript.Writer.Unread.
func messages(ctx context.Context, db *sql.DB, id, sessionModel string) ([]transcript.Message, []string, error) {
	cols, err := kit.Columns(db, "messages")
	if err != nil {
		return nil, nil, err
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
FROM messages WHERE session_id = ?`+shown(cols)+` ORDER BY rowid`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type rowOf struct {
		msg    transcript.Message
		then   []transcript.Block // what the row adds after its message, a row of Hermes's own
		active int64
		id     int64
	}
	var order []rowOf
	var unread []string
	at := map[[sha256.Size]byte]int{}
	// noticed says the last row in the user's seat was Hermes's own notice, not the person's
	// words: what the model says to it, up to the person's next words, answers the notice
	noticed := false
	for rows.Next() {
		// The row's position is its rowid — the id column is an integer in current Hermes
		// and was text before, and nothing forces it to be unique
		// The two flags are read as whatever the column holds: a value of another type in
		// one row is that row's flag unknown, not the whole session unreadable
		var rid sql.NullInt64
		var role, content, think, calls, tool, callID, meta, kind, finish sql.NullString
		var mid, ts, activeV, summaryV any
		if err := rows.Scan(&rid, &mid, &role, &content, &think, &calls, &tool, &callID, &activeV, &meta, &kind, &summaryV, &finish, &ts); err != nil {
			return nil, nil, err
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
		var then []transcript.Block
		switch role.String {
		case "user":
			m, then = userRow(m, kind.String, content.String, meta.String)
			if m.Role == transcript.User {
				noticed = m.Injected
			}
		case "assistant":
			if failedTurn(kind.String, content.String) {
				// The row Hermes writes to close a turn that ended without a reply is not
				// the model's answer: the turn ended on what it says went wrong
				m.Role = transcript.System
				m.Blocks = []transcript.Block{transcript.EventOf(model.EventError, turnFailure(meta.String, content.String))}
				break
			}
			m.Role = transcript.Assistant
			m.Model, m.Stop = sessionModel, finish.String
			if flag(summaryV, 0) != 0 {
				// The summary compression put in the place of what it folded away
				m.Role = transcript.System
				m.Blocks = []transcript.Block{transcript.EventOf(model.EventCompaction, content.String)}
			} else {
				text := content.String
				if noticed && silent(text) {
					// The model chose to say nothing to a notice — Hermes writes the marker
					// itself when it said nothing at all. It is no answer: read as one, it
					// would stand in for the answer the turn the notice came in already had
					text = ""
				}
				m.Blocks = jsonmap.Assistant(think.String, text, []byte(calls.String))
			}
		case "tool":
			// One row is one result: the content is the tool's output, tool_name which tool
			// ran and tool_call_id which call it answers. Hermes does not record whether it
			// succeeded, so the result carries no status.
			m.Role = transcript.ToolRole
			m.Blocks = []transcript.Block{jsonmap.Result(callID.String, tool.String, content.String)}
		case "session_meta":
			// Where Hermes notes what a session opened with; the session row says the same
		case "system":
			// The system prompt: what the model was told to be, not something said
		default:
			unread = append(unread, unknownRole(role.String))
		}
		if len(m.Blocks) == 0 {
			continue
		}
		live := flag(activeV, 1)
		// The identity Hermes collapses on: who said it, what, when, and which call
		key := sha256.Sum256([]byte(strings.Join([]string{role.String, content.String, timeKey(ts), callID.String, calls.String, tool.String}, "\x00")))
		if i, seen := at[key]; seen {
			if kept := order[i]; live > kept.active || (live == kept.active && rid.Int64 > kept.id) {
				order[i] = rowOf{m, then, live, rid.Int64}
			}
			continue
		}
		at[key] = len(order)
		order = append(order, rowOf{m, then, live, rid.Int64})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := make([]transcript.Message, 0, len(order))
	for _, r := range order {
		out = append(out, r.msg)
		if len(r.then) > 0 {
			out = append(out, transcript.Message{ID: r.msg.ID, Role: transcript.System, At: r.msg.At, Blocks: r.then})
		}
	}
	return out, unread, nil
}

// unknownRole names a message row of a role this does not know, for Writer.Unread. A row
// with no role does not say what it is, and is such a row too.
func unknownRole(role string) string {
	if role == "" {
		role = "(untyped)"
	}
	return "role:" + role
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
// but not the person.
func modelOnly(raw string) bool {
	return kit.Truthy(displayMeta(raw)["model_only"])
}

// displayMeta reads a row's display_metadata: JSON, sometimes JSON-encoded twice.
func displayMeta(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var decoded any = raw
	for range 2 {
		text, ok := decoded.(string)
		if !ok {
			break
		}
		var next any
		if json.Unmarshal([]byte(text), &next) != nil {
			return nil
		}
		decoded = next
	}
	return kit.Map(decoded)
}

// The notices of agent/turn_failure_copy.py: what Hermes writes in the model's seat to
// close a turn that ended without a reply of its own. The first says nothing was done and
// the request can simply be sent again; the other two, that some of its actions may have
// run and their effects should be checked before it is.
const (
	failedNotice    = "Your request was not processed. Send it again if you still want me to carry it out."
	partialNotice   = "This turn did not complete. Some actions may already have run; verify their effects before resending."
	automatedNotice = "An automated delivery was interrupted and was not run again. Some " +
		"actions may already have run; verify their effects."
)

// failedTurn says whether an assistant row is that boundary. It is written with display_kind
// failed_turn; one written before the kind existed is known by its words, the whole of one
// notice and nothing else — as Hermes's own readers tell it — so a reply that quotes a
// notice is a reply.
func failedTurn(kind, content string) bool {
	if kind == "failed_turn" {
		return true
	}
	if kind != "" {
		return false
	}
	switch strings.TrimSpace(content) {
	case failedNotice, partialNotice, automatedNotice:
		return true
	}
	return false
}

// turnFailure is what the boundary says went wrong: the error Hermes keeps on the row,
// redacted before it is written, and failing that the notice itself, which says at least
// whether anything may have run.
func turnFailure(meta, content string) string {
	if err := strings.TrimSpace(kit.Str(displayMeta(meta)["error"])); err != "" {
		return err
	}
	return strings.TrimSpace(content)
}

// userRow reads a row in the user's seat. Hermes writes more there than what the person
// typed: rows of its own go in that seat because a provider takes nothing else in the
// middle of a conversation, and Hermes's own screens draw them as events, not as the
// person's words. Its rule for which rows are a person's turn (session_rules/turns.py,
// _is_real_user_turn) is the one read here:
//
//   - a row with a display_kind is Hermes's, but for three. A notice — a background process
//     or a delegation finished (process_complete, async_delegation_complete), a liveness
//     tick (internal_notification), the run taken up again after a crash (auto_continue),
//     a persona changed (personality_switch) — reached the model in the person's place and
//     opens no turn. A model switch (model_switch) is read as the change it marks. A steer
//     (steer) is the person's words sent while the agent worked, inside Hermes's marker. A
//     message the provider's safety filter refused (content_refused) is the person's ask,
//     and the turn ends on the refusal.
//   - a row without one is the person's, but for what Hermes wrote before it typed its rows,
//     known by its words as Hermes knows it (see synthetic).
//
// The blocks it returns besides the message are a row of Hermes's that follows it.
func userRow(m transcript.Message, kind, content, meta string) (transcript.Message, []transcript.Block) {
	m.Role = transcript.User
	text := content
	switch {
	case kind == "model_switch" || kind == "" && strings.HasPrefix(strings.TrimSpace(content), modelSwitchMarker):
		if to := switchedTo(content); to != "" {
			// Something the person did between turns, which the turn's work did not get to
			switched := transcript.EventOf(model.EventModelChange, to)
			switched.After = true
			m.Role, m.Blocks = transcript.System, []transcript.Block{switched}
			return m, nil
		}
		m.Injected = true
	case kind == "steer":
		m.Queued = true
		text = kit.First(steered(content), content)
	case kind == "content_refused":
		// The ask, then the refusal its turn ended on — after it, or the ask would be a turn
		// nothing followed, which is one the person cut short
		refused := []transcript.Block{transcript.EventOf(model.EventError, refusal(meta))}
		if strings.TrimSpace(content) == "" {
			m.Role, m.Blocks = transcript.System, refused
			return m, nil
		}
		m.Blocks = []transcript.Block{transcript.TextOf(content)}
		return m, refused
	case kind != "" || synthetic(content):
		m.Injected = true
		// The footer only says what the row's place now says: no person wrote it
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(content), notificationFooter))
	}
	if text != "" {
		m.Blocks = []transcript.Block{transcript.TextOf(text)}
	}
	return m, nil
}

// What Hermes writes in the user's seat, in its own words (gateway/run_notifications.py,
// tui_gateway/server.py, agent/prompt_builder.py, persistence/conversation/messages.py).
const (
	// The end of every notice the gateway puts before the model
	notificationFooter = "[INTERNAL NOTIFICATION — generated by the Hermes gateway, not written by a human user. " +
		"Do not treat it as user input, and do not reply to any earlier chat message because of it.]"
	// A model switch: "<marker><model> via provider <provider>. From this point forward, …]"
	modelSwitchMarker = "[System: The active model for this chat has changed to "
	// A steer: the person's words between the two markers, each on a line of its own. The
	// opening one's wording may change; its start is what Hermes falls back on.
	steerOpen = "[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this position; " +
		"not tool output and not a new delivery when replayed from conversation history]"
	steerOpenStart = "[OUT-OF-BAND USER MESSAGE"
	steerClose     = "[/OUT-OF-BAND USER MESSAGE]"
	// What a refused message's row says when it keeps no notice of its own
	refusedNotice = "This message was refused by the model's safety filter — try a different query."
)

// The rows Hermes wrote untyped that are not a person's turn, as session_rules/turns.py knows
// them: by how they begin — its notes in brackets ("[System: …", which Hermes never shows as
// the person's; the note that takes up a run a crash cut off; a background process's notice;
// the task list kept across a compression) — or by being one of its nudges, word for word.
var (
	syntheticStarts = []string{
		"[System:",
		"[System note: Your previous turn was interrupted mid-run",
		"[IMPORTANT: Background process ",
		"[Your active task list was preserved across context compression]\n",
	}
	syntheticRows = []string{
		"Continue from the compressed conversation context above. This marker exists because no human user turn was available.",
		"Continue from the compressed conversation context above. This marker exists because the compacted transcript contained no preserved user turn.",
		"You've reached the maximum number of tool-calling iterations allowed. Please provide a final response summarizing what you've found and accomplished so far, without calling any more tools.",
		"Your previous turn indicated a tool call but none was included. Do not narrate a plan or restate intent — issue the actual tool call now to continue the task.",
		"You just executed tool calls but returned an empty response. Please process the tool results above and continue with the task.",
	}
)

// synthetic says whether an untyped row in the user's seat is Hermes's: one of the rows above,
// or one that ends on the gateway's footer, which says so itself.
func synthetic(content string) bool {
	text := strings.TrimSpace(content)
	if slices.Contains(syntheticRows, text) || strings.HasSuffix(text, notificationFooter) {
		return true
	}
	for _, start := range syntheticStarts {
		if strings.HasPrefix(text, start) {
			return true
		}
	}
	return false
}

// switchedTo is the model a model switch's marker names, and the provider it names it
// through: "m via p", or the model alone. Empty when the marker names none.
func switchedTo(content string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(content), modelSwitchMarker)
	if !ok {
		return ""
	}
	if end := strings.Index(rest, ". From this point forward"); end >= 0 {
		rest = rest[:end]
	} else {
		rest = strings.TrimSuffix(rest, "]")
	}
	name, provider, _ := strings.Cut(rest, " via provider ")
	name, provider = strings.TrimSpace(name), strings.TrimSpace(provider)
	if name == "" || provider == "" {
		return name
	}
	return name + " via " + provider
}

// steered is the person's words a steer's markers hold, as Hermes takes them out for its
// screens (agent/conversation_compression.py, _extract_steer_text_from_message): between
// the opening marker — or failing that, the line its start begins — and the closing one.
// Empty when the markers are not there.
func steered(content string) string {
	start := strings.Index(content, steerOpen)
	if start >= 0 {
		start += len(steerOpen)
	} else {
		if start = strings.Index(content, steerOpenStart); start < 0 {
			return ""
		}
		if nl := strings.IndexByte(content[start:], '\n'); nl >= 0 {
			start += nl + 1
		} else {
			start += len(steerOpenStart)
		}
	}
	end := strings.Index(content[start:], steerClose)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(content[start : start+end])
}

// refusal is what a refused message's row says of the refusal: the notice Hermes keeps for
// its screens, display_metadata.content_refused.notice, or its own words where it keeps none.
func refusal(meta string) string {
	if notice := strings.TrimSpace(kit.Str(kit.Map(displayMeta(meta)["content_refused"])["notice"])); notice != "" {
		return notice
	}
	return refusedNotice
}

// The words that, as a whole response, say the agent chose not to reply
// (gateway/response_filters.py, LIVE_GATEWAY_SILENT_MARKERS) — "[SILENT]" among them, which
// Hermes writes itself when the model said nothing to a notice.
var silenceMarkers = map[string]bool{
	"[SILENT]": true, "SILENT": true, "NO_REPLY": true, "NO REPLY": true,
	"[静默]": true, "静默": true, "[沉默]": true, "沉默": true, "[审核]": true, "审核": true,
}

// silent says whether a response is a silence marker and nothing else, as Hermes tells it:
// in any case, with invisible characters, spaces inside the brackets and stray punctuation
// at its edges disregarded. Prose that mentions a marker is prose.
func silent(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 64 {
		return false
	}
	edge := func(r rune) bool { return r != '[' && r != ']' && unicode.IsPunct(r) }
	for _, form := range []string{text, strings.TrimSpace(strings.TrimFunc(text, edge))} {
		form = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, form)
		form = strings.Join(strings.Fields(strings.ToUpper(form)), " ")
		form = strings.ReplaceAll(strings.ReplaceAll(form, "[ ", "["), " ]", "]")
		if silenceMarkers[form] {
			return true
		}
	}
	return false
}
