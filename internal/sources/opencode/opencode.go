// Package opencode reads OpenCode's sessions.
//
// Everything lives in one SQLite database, and its layout has changed once:
//
//	2.x   session_v2        one row per session: directory, title, model, version, times,
//	                        parent_id for a session another one spawned, and for a
//	                        fork the session it was made from
//	      session_message   one row per message, ordered by seq, its parts inline in data
//	1.x   session, message, part
//	                        the same, with a message's parts as rows of their own
//
// The layout is detected per database — 2.x when both of its tables exist; one alone is
// a migration caught halfway, and the 1.x tables are still the truth then — and a
// database that saw both lists the 1.x sessions that were never migrated beside the 2.x
// ones, each read through the layout it lives in. For a long time only 1.x was read, so
// a 2.x install listed nothing and said nothing: an empty store and a moved one answer
// alike, which is why a schema that cannot be read is an error here and not zero sessions.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes OpenCode.
var Spec = sources.Spec{
	Name:  "opencode",
	Title: "OpenCode",
	Locate: func(env sources.Env) string {
		// OPENCODE_DB names the database: as it is when it is absolute, or ":memory:" —
		// a database kept in memory, which leaves nothing to read — and inside the data
		// directory when it is not (packages/cli/src/database-path.ts, and the same rule
		// in the released 1.19.6). Empty, it is not set
		db := env.Getenv("OPENCODE_DB")
		if db == memory || filepath.IsAbs(db) {
			return db
		}
		return filepath.Join(dataDir(env), kit.First(db, "opencode.db"))
	},
	// A --path may name the database itself or the data directory it sits in, which is
	// the shape a redirected XDG_DATA_HOME leaves behind
	Open:    func(location string) sources.Source { return &Source{at: location} },
	Movable: true,
	Resume:  "opencode --session",
	Reads:   1,
	Store:   "~/.local/share/opencode/opencode.db",
}

// dataDir is OpenCode's data directory: under XDG_DATA_HOME on every Unix-like — macOS
// included: it really is ~/.local/share/opencode there, not ~/Library/Application
// Support — and LOCALAPPDATA on Windows.
func dataDir(env sources.Env) string {
	if runtime.GOOS == "windows" {
		if d := env.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "opencode")
		}
		return filepath.Join(env.Home, "AppData", "Local", "opencode")
	}
	if d := env.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	return filepath.Join(env.Home, ".local", "share", "opencode")
}

// memory is the OPENCODE_DB that keeps the database in memory.
const memory = ":memory:"

// Source is one OpenCode database.
type Source struct {
	at   string // the database, or the data directory it sits in: see path
	list kit.Snapshot[[]sources.Ref]
}

// path is the database. A location is the database itself — OPENCODE_DB can give it any
// name — unless it is a directory, which is the data directory the database sits in as
// opencode.db. Which it is is asked each time, since either can appear while asq runs.
// A database in memory is nowhere to be read.
func (s *Source) path() string {
	if s.at == memory {
		return ""
	}
	if info, err := os.Stat(s.at); err == nil && info.IsDir() {
		return filepath.Join(s.at, "opencode.db")
	}
	return s.at
}

func (s *Source) Location() string {
	if db := s.path(); db != "" {
		return db
	}
	return s.at
}

func (s *Source) Exists() bool {
	db := s.path()
	return db != "" && kit.Exists(db)
}

// hasV2 reports whether the database carries the 2.x tables. Both have to exist.
func hasV2(db *sql.DB) bool {
	return kit.HasTable(db, "session_v2") && kit.HasTable(db, "session_message")
}

// Scan lists the sessions with one query per layout, and not at all while the database
// and its write-ahead log have not moved.
//
// A session's stamp is built from what its own rows say — when it and its newest message
// were last written, and how many messages it has — so a message arriving in one session
// leaves every other session's stamp where it was.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	path := s.path()
	return s.list.Load(path, func() ([]sources.Ref, error) {
		var refs []sources.Ref
		err := kit.ReadDB(ctx, path, func(db *sql.DB) error {
			refs = refs[:0]
			v2 := hasV2(db)
			if v2 {
				// time_updated on the session is not always moved by a new message; the
				// messages' own times are
				// A fork's copied rows are another session's for as long as that session
				// holds them: how many are belongs to what a fork was read at
				copies := "0"
				if cols, err := kit.Columns(db, "session_v2"); err == nil && cols["fork_session_id"] {
					copies = `CASE WHEN COALESCE(s.fork_session_id, '') = '' THEN 0 ELSE
       (SELECT COUNT(*) FROM session_message m WHERE m.session_id = s.id AND ` + copied + `) END`
				}
				if err := scan(ctx, db, path, &refs, `
SELECT s.id, s.time_updated, s.time_created,
       (SELECT COUNT(*) FROM session_message m WHERE m.session_id = s.id),
       (SELECT MAX(m.time_updated) FROM session_message m WHERE m.session_id = s.id),
       `+copies+`
FROM session_v2 s`); err != nil {
					return err
				}
			}
			if kit.HasTable(db, "session") {
				// A migrated session keeps its id, so NOT EXISTS keeps each to one row
				unmigrated := ""
				if v2 {
					unmigrated = ` WHERE NOT EXISTS (SELECT 1 FROM session_v2 v WHERE v.id = s.id)`
				}
				return scan(ctx, db, path, &refs, `
SELECT s.id, s.time_updated, s.time_created,
       (SELECT COUNT(*) FROM part p WHERE p.session_id = s.id),
       (SELECT MAX(m.time_updated) FROM message m WHERE m.session_id = s.id),
       0
FROM session s`+unmigrated)
			}
			if !v2 {
				return errors.New("no session tables: not a layout this reads")
			}
			return nil
		})
		return refs, err
	})
}

func scan(ctx context.Context, db *sql.DB, path string, refs *[]sources.Ref, query string) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullString
		var updated, created, count, last, copies sql.NullInt64
		if err := rows.Scan(&id, &updated, &created, &count, &last, &copies); err != nil {
			return err
		}
		if id.String == "" {
			continue
		}
		at := max(updated.Int64, last.Int64, created.Int64)
		stamp := strconv.FormatInt(updated.Int64, 36) + "." + strconv.FormatInt(last.Int64, 36) + "." + strconv.FormatInt(count.Int64, 36)
		if copies.Int64 > 0 {
			stamp += ".copied" + strconv.FormatInt(copies.Int64, 36)
		}
		*refs = append(*refs, sources.Ref{Key: path + "#" + id.String, Stamp: stamp, ModTime: time.UnixMilli(at)})
	}
	return rows.Err()
}

// Read parses one session.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	_, id, ok := strings.Cut(ref.Key, "#")
	if !ok || id == "" {
		return errors.New("opencode: a session is named <database>#<id>")
	}
	// A read that met a write in flight is run again from the start: what it had added
	// is discarded with the writer the caller hands over only once, so the rows are
	// gathered first and added after — and so is what was passed over unknown, which a
	// read run again would otherwise count twice
	var msgs []transcript.Message
	var sess model.Session
	var unread []string
	note := func(kind string) { unread = append(unread, kind) }
	path := s.path()
	err := kit.ReadDB(ctx, path, func(db *sql.DB) error {
		msgs, sess, unread = msgs[:0], model.Session{}, unread[:0]
		if hasV2(db) {
			found, err := readV2(ctx, db, id, &sess, &msgs, note)
			if err != nil || found {
				return err
			}
		}
		if !kit.HasTable(db, "session") {
			return sql.ErrNoRows
		}
		return readV1(ctx, db, id, &sess, &msgs, note)
	})
	if err != nil {
		return err
	}
	sess.ID, sess.Path = id, path
	w.S = sess
	for _, m := range msgs {
		w.Add(m)
	}
	for _, kind := range unread {
		w.Unread(kind)
	}
	return nil
}

// unknown names a record of a kind the adapter does not know, for Writer.Unread. One that
// does not say what it is — a part with no type — is such a record too.
func unknown(what, kind string) string {
	if kind == "" {
		kind = "(untyped)"
	}
	return what + ":" + kind
}

// rowOf reads a query's first row as column → value, so a column a version of the schema
// does not have is simply absent rather than a failed query.
func rowOf(ctx context.Context, db *sql.DB, query string, args ...any) (map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, col := range cols {
		if b, ok := vals[i].([]byte); ok {
			out[col] = string(b)
		} else {
			out[col] = vals[i]
		}
	}
	return out, nil
}

// sessionOf fills in what a session row says, whichever layout it is from.
func sessionOf(row map[string]any, sess *model.Session) {
	// OpenCode writes a title for every session, and that is what its own list shows
	sess.Title = kit.First(kit.Str(row["title"]), kit.Str(row["slug"]))
	sess.Cwd = kit.Str(row["directory"])
	sess.Version = kit.Str(row["version"])
	sess.Model = modelName(kit.Str(row["model"]))
	sess.StartedAt = millis(row["time_created"])
	sess.UpdatedAt = millis(row["time_updated"])
	sess.Archived = row["time_archived"] != nil
	// A session another one spawned — a task handed to a subagent — names its parent
	if parent := kit.Str(row["parent_id"]); parent != "" {
		sess.Parent = &model.Parent{Session: parent, Type: kit.Str(row["agent"])}
	}
	// OpenCode's own totals for the session. They are the session's alone — measured on
	// 2.3: not its subagents', and for a fork not the rows it copied — and they hold what
	// no message does: the call that gave the session its title.
	if _, kept := kit.Num(row["tokens_input"]); kept {
		reasoning := kit.Count(row["tokens_reasoning"])
		u := model.Usage{
			Input:      kit.Count(row["tokens_input"]),
			Output:     kit.Count(row["tokens_output"]) + reasoning,
			CacheRead:  kit.Count(row["tokens_cache_read"]),
			CacheWrite: kit.Count(row["tokens_cache_write"]),
			Reasoning:  reasoning,
		}
		cost := jsonmap.Cost(row["cost"])
		if !u.IsZero() || cost > 0 {
			sess.Recorded = &model.Recorded{Usage: &u, CostUSD: cost}
		}
	}
}

func millis(v any) time.Time {
	if n := kit.Int(v); n > 0 {
		return time.UnixMilli(n).UTC()
	}
	return time.Time{}
}

// modelName turns the model column's JSON ({"id":…,"providerID":…}) into the
// provider/model OpenCode displays. Anything else is kept as it is.
func modelName(raw string) string {
	if raw == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return raw
	}
	return providerModel(kit.Str(m["providerID"]), kit.Str(m["id"]))
}

func providerModel(provider, id string) string {
	if id == "" {
		return ""
	}
	if provider != "" {
		return provider + "/" + id
	}
	return id
}

// messageModel names the model an assistant message came from: 1.x keeps modelID and
// providerID on the message, 2.x a model object.
func messageModel(data map[string]any) string {
	if m, ok := data["model"].(map[string]any); ok {
		return providerModel(kit.Str(m["providerID"]), kit.Str(m["id"]))
	}
	return providerModel(kit.Str(data["providerID"]), kit.Str(data["modelID"]))
}

// usageOf reads a message's own counters: tokens{input, output, reasoning, cache{read,
// write}}. OpenCode reports the cache beside the input, which is the canonical shape
// already, and the reasoning beside an output it is not part of — on one machine's
// history the reasoning outweighed the output, 1.70M to 1.14M — so it is added in.
func usageOf(data map[string]any) *model.Usage {
	tokens, ok := data["tokens"].(map[string]any)
	if !ok {
		return nil
	}
	cache := kit.Map(tokens["cache"])
	reasoning := kit.Count(tokens["reasoning"])
	u := model.Usage{
		Input:      kit.Count(tokens["input"]),
		Output:     kit.Count(tokens["output"]) + reasoning,
		CacheRead:  kit.Count(cache["read"]),
		CacheWrite: kit.Count(cache["write"]),
		Reasoning:  reasoning,
	}
	if u.IsZero() {
		return nil
	}
	return &u
}

// assistant fills in what an assistant message says about the model call it is: which
// model, what it cost, and how long it took. OpenCode measures a response — time.created
// is when the call was made, time.completed when it ended, and time.streamed (or, before
// that field, the earliest part's own time) when it began to arrive.
func assistant(m *transcript.Message, data map[string]any, parts []any) {
	m.Model = messageModel(data)
	m.Usage = usageOf(data)
	m.CostUSD = jsonmap.Cost(data["cost"])
	m.Stop = kit.Str(data["finish"])
	tm := kit.Map(data["time"])
	created := kit.Int(tm["created"])
	if created <= 0 {
		return
	}
	if completed := kit.Int(tm["completed"]); completed > created {
		m.DurMs = model.DurMs(completed - created)
	}
	first := kit.Int(tm["streamed"])
	if first < created {
		first = 0
		for _, p := range parts {
			if at := kit.Int(kit.Map(kit.Map(p)["time"])["created"]); at >= created && (first == 0 || at < first) {
				first = at
			}
		}
	}
	if first > created {
		m.TTFTMs = model.DurMs(first - created)
	}
}

// errorText is the readable part of an error value: a string as it is, an object's
// message (or its data's, where a provider error nests it), else its name.
func errorText(v any) string {
	switch e := v.(type) {
	case string:
		return strings.TrimSpace(e)
	case map[string]any:
		return strings.TrimSpace(kit.First(kit.Str(e["message"]), kit.Str(kit.Map(e["data"])["message"]), kit.Str(e["name"]), kit.Str(e["type"])))
	}
	return ""
}

// blocksOf maps one part — a 1.x part row, or a part inline in a 2.x response — onto
// blocks. Both layouts write the same three kinds of content; unread is told of a part of
// any other type. The parts 1.x wrote beside the content are left aside before this (see
// besideV1): in a 2.x response a part of one of their types is one this does not know.
func blocksOf(part map[string]any, unread func(kind string)) []transcript.Block {
	switch kind := kit.Str(part["type"]); kind {
	case "text":
		if text := kit.Str(part["text"]); text != "" {
			return []transcript.Block{transcript.TextOf(text)}
		}
	case "reasoning":
		if text := kit.Str(part["text"]); text != "" {
			return []transcript.Block{transcript.ThinkingOf(text)}
		}
	case "tool":
		state := kit.Map(part["state"])
		// 1.x names the tool in tool and the call in callID; a 2.x inline part says name
		// and id
		name := strings.TrimSpace(kit.First(kit.Str(part["tool"]), kit.Str(part["name"])))
		callID := kit.First(kit.Str(part["callID"]), kit.Str(part["id"]))
		input := state["input"]
		if input == nil {
			input = part["input"]
		}
		call := transcript.CallOf(callID, name, input)
		// The call and its result live in one part; the result half exists once the state
		// is completed or error. A pending or running tool is the call alone.
		status := kit.Str(state["status"])
		if status != "completed" && status != "error" {
			return []transcript.Block{call}
		}
		outcome := transcript.Outcome{Status: model.StatusOK}
		text := toolOutput(state, unread)
		if status == "error" {
			outcome.Status = model.StatusError
			errText := errorText(state["error"])
			// A tool the user stopped is an error that says so: by type in 2.x, by its
			// words in 1.x
			if kit.Str(kit.Map(state["error"])["type"]) == "aborted" || strings.Contains(strings.ToLower(errText), "abort") {
				outcome.Status = model.StatusInterrupted
			}
			if errText != "" {
				text = "error: " + errText
			}
		}
		// A shell's exit code sits in the state's metadata; the times of the call are on
		// the state in 1.x (start, end) and on the part in 2.x (ran, completed)
		// OpenCode calls a command that ran to its end completed, whatever it exited with;
		// a non-zero exit is the command saying it failed, as it is for every other CLI.
		if code, ok := kit.Num(kit.Map(state["metadata"])["exit"]); ok {
			outcome.Exit = transcript.ExitOf(int(code))
			if code != 0 && outcome.Status == model.StatusOK {
				outcome.Status = model.StatusError
			}
		}
		if span := kit.Map(state["time"]); len(span) > 0 {
			if start, end := kit.Int(span["start"]), kit.Int(span["end"]); start > 0 && end >= start {
				outcome.DurMs = model.DurMs(end - start)
			}
		} else if span := kit.Map(part["time"]); len(span) > 0 {
			start := kit.Int(span["ran"])
			if start == 0 {
				start = kit.Int(span["created"])
			}
			if end := kit.Int(span["completed"]); start > 0 && end >= start {
				outcome.DurMs = model.DurMs(end - start)
			}
		}
		return []transcript.Block{call, transcript.ResultOf(callID, name, text, outcome)}
	default:
		unread(unknown("part", kind))
	}
	return nil
}

// besideV1 reports whether a 1.x part row is one 1.x wrote beside the content, which is
// left aside knowing what it is. 2.x writes none of these in a response, so they are not
// left aside there.
func besideV1(kind string) bool {
	switch kind {
	case "step-start", "step-finish":
		// Where a model call began and ended — boundaries, not content; the message
		// carries the call's usage
	case "snapshot", "patch":
		// The worktree's snapshot hash, and the files a step changed under it —
		// OpenCode's bookkeeping for undo; the tools that changed them are read
	case "file", "agent":
		// A file the person attached to a prompt, an agent they @-mentioned in it — what
		// the prompt's text names; 2.x keeps both on the user row, not read there either
	case "subtask":
		// A command handing a prompt to a subagent, whose session is listed on its own
	case "retry":
		// A model call that failed and was made again; the call that answered is read
	case "compaction":
		// The marker on a prompt that asked for the context to be compacted; the summary
		// is the assistant message that follows
	default:
		return false
	}
	return true
}

// toolOutput is what a finished tool returned: a plain string in 1.x (state.output), a
// list of content items in 2.x (state.content) whose text entries are the output and
// whose files are named. unread is told of an item of any other type.
func toolOutput(state map[string]any, unread func(kind string)) string {
	if out, ok := state["output"].(string); ok {
		return out
	}
	var parts []string
	for _, item := range kit.List(state["content"]) {
		m := kit.Map(item)
		switch kind := kit.Str(m["type"]); kind {
		case "text":
			parts = append(parts, kit.Str(m["text"]))
		case "file":
			parts = append(parts, "[file: "+kit.First(kit.Str(m["name"]), kit.Str(m["uri"]))+"]")
		default:
			unread(unknown("output", kind))
		}
	}
	return strings.Join(parts, "\n")
}

// readV2 reads a 2.x session: one session_message row per message, ordered by seq, its
// type the role, its parts inline under data.content. It reports whether the session is
// in this layout at all; unread is told of every row and part it passed over not knowing
// what it was.
func readV2(ctx context.Context, db *sql.DB, id string, sess *model.Session, msgs *[]transcript.Message, unread func(kind string)) (bool, error) {
	row, err := rowOf(ctx, db, `SELECT * FROM session_v2 WHERE id = ?`, id)
	if err != nil || row == nil {
		return false, err
	}
	sessionOf(row, sess)

	// A fork's rows are read each with whether it is a copy of a row counted elsewhere
	query := `SELECT m.id, m.type, m.time_created, m.data, 0 FROM session_message m WHERE m.session_id = ? ORDER BY m.seq, m.rowid`
	if kit.Str(row["fork_session_id"]) != "" {
		query = `SELECT m.id, m.type, m.time_created, m.data, ` + copied + `
FROM session_message m JOIN session_v2 s ON s.id = m.session_id WHERE m.session_id = ? ORDER BY m.seq, m.rowid`
	}
	rows, err := db.QueryContext(ctx, query, id)
	if err != nil {
		return true, err
	}
	defer rows.Close()
	stopped := false // the turn in flight was already marked interrupted
	for rows.Next() {
		var mid, kind, raw string
		var created sql.NullInt64
		var inherited bool
		if err := rows.Scan(&mid, &kind, &created, &raw, &inherited); err != nil {
			return true, err
		}
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue // one unparsable row must not hide the rest of the session
		}
		at := millis(created.Int64)
		if at.IsZero() {
			at = millis(kit.Map(data["time"])["created"])
		}
		add := func(role transcript.Role, blocks ...transcript.Block) {
			if len(blocks) > 0 {
				*msgs = append(*msgs, transcript.Message{ID: mid, Role: role, At: at, Inherited: inherited, Blocks: blocks})
			}
		}
		event := func(kind, text string) { add(transcript.System, transcript.EventOf(kind, text)) }

		switch kind {
		case "user":
			stopped = false
			text := kit.Str(data["text"])
			if text == "" {
				text = kit.ContentText(data["content"])
			}
			if text != "" {
				add(transcript.User, transcript.TextOf(text))
			}
		case "assistant":
			parts := kit.List(data["content"])
			var blocks []transcript.Block
			for _, p := range parts {
				blocks = append(blocks, blocksOf(kit.Map(p), unread)...)
			}
			if text, ok := data["content"].(string); ok && text != "" {
				blocks = append(blocks, transcript.TextOf(text))
			}
			m := transcript.Message{ID: mid, Role: transcript.Assistant, At: at, Inherited: inherited, Blocks: blocks}
			assistant(&m, data, parts)
			// A response that ended in an error says which: the user stopping it, or the
			// provider refusing it
			if e := kit.Map(data["error"]); len(e) > 0 {
				if kit.Str(e["type"]) == "aborted" {
					m.Blocks = append(m.Blocks, transcript.EventOf(model.EventInterrupted, errorText(e)))
					stopped = true
				} else {
					m.Blocks = append(m.Blocks, transcript.EventOf("error", errorText(e)))
				}
			}
			*msgs = append(*msgs, m)
		case "system", "synthetic", "skill":
			// Text OpenCode put before the model itself: updated instructions, a background
			// command reporting in, the instructions of a skill once it was activated
			text := strings.TrimSpace(kit.Str(data["text"]))
			if desc := strings.TrimSpace(kit.Str(data["description"])); desc != "" && !strings.HasPrefix(text, desc) {
				text = strings.TrimSpace(desc + "\n\n" + text)
			}
			if text != "" {
				add(transcript.System, transcript.TextOf(text))
			}
		case "shell":
			// A command the person ran themselves, with a !, beside the conversation
			outcome := transcript.Outcome{}
			if code, ok := kit.Num(data["exit"]); ok {
				outcome.Exit = transcript.ExitOf(int(code))
				outcome.Status = transcript.StatusOf(code != 0)
			}
			tm := kit.Map(data["time"])
			if start, end := kit.Int(tm["created"]), kit.Int(tm["completed"]); start > 0 && end >= start {
				outcome.DurMs = model.DurMs(end - start)
			}
			out := kit.Str(kit.Map(data["output"])["output"])
			if out == "" {
				out = kit.Str(data["output"])
			}
			add(transcript.System, transcript.CallOf(mid, "shell", map[string]any{"command": kit.Str(data["command"])}),
				transcript.ResultOf(mid, "shell", out, outcome))
		case "compaction":
			// Where the context the model goes on from was rewritten, with the summary it
			// went on from; a failed one has no summary, and its error says what happened.
			// The messages before it stay in the table, so this adds the boundary, not a
			// second copy.
			text := strings.TrimSpace(kit.Str(data["summary"]))
			if text == "" {
				text = errorText(data["error"])
			}
			if text == "" {
				text = "Context compacted"
			}
			event(model.EventCompaction, text)
		case "model-switched":
			m := kit.Map(data["model"])
			if name := providerModel(kit.Str(m["providerID"]), kit.Str(m["id"])); name != "" {
				event(model.EventModelChange, name)
			}
		case "agent-switched":
			if agent := kit.Str(data["agent"]); agent != "" {
				event("agent_switch", strings.TrimPrefix(kit.Str(data["previous"])+" → "+agent, " → "))
			}
		case "location-switched":
			// The session moved to another directory or worktree; the session row says where
			// it is now
		case "idle":
			// How the turn ended, once it had
			switch kit.Str(data["outcome"]) {
			case "interrupted":
				if !stopped {
					event(model.EventInterrupted, "Interrupted")
				}
			case "failed":
				event("error", kit.First(errorText(data["error"]), "The turn failed"))
			case "succeeded":
				// A turn that ended as it should: its last reply says so
			}
			stopped = false
		default:
			unread(unknown("row", kind))
		}
	}
	return true, rows.Err()
}

// copied is the condition under which m, a row of the fork s, is a copy of a row that is
// counted where it was first written.
//
// OpenCode forks a session by copying its messages into a new session, each under a new
// id and otherwise as it was: its seq, its time, and the tokens an assistant row says it
// cost (measured on OpenCode 2.3, which records on the fork which session that was, in
// fork_session_id). Read as the fork's own, every one of those responses is counted a
// second time, and a third in a fork of the fork.
//
// A row older than the fork it is in was not written by it; and it is somebody else's for
// as long as an older session holds the same row — the session forked from, or with that
// one deleted (which leaves its forks behind) the fork made before this one. So every
// response is counted once, by the oldest session that still holds it, and where none
// does — the original deleted, or undone to before the row — the copy is the only record
// left of what was spent and is read as the fork's own. The fork's own rows are never
// taken for copies: they are newer than it is.
const copied = `(m.time_created < s.time_created AND EXISTS (
        SELECT 1 FROM session_message p JOIN session_v2 o ON o.id = p.session_id
        WHERE p.time_created = m.time_created AND p.seq = m.seq AND p.type = m.type AND p.session_id <> s.id
          AND (o.time_created < s.time_created OR (o.time_created = s.time_created AND o.id < s.id))))`

// readV1 reads a 1.x session: messages and their parts joined in display order. unread is
// told of every message and part it passed over not knowing what it was.
//
// message_id alone joins the parts: a part belongs to the message it names, whatever its
// own session_id column holds. Matching on p.session_id as well invites the planner to
// drive the join from a session-wide part index instead — measured on a 2 000-message /
// 20 000-part database, 2.96 s with that plan against 0.02 s without.
func readV1(ctx context.Context, db *sql.DB, id string, sess *model.Session, msgs *[]transcript.Message, unread func(kind string)) error {
	row, err := rowOf(ctx, db, `SELECT * FROM session WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if row == nil {
		return sql.ErrNoRows
	}
	sessionOf(row, sess)

	rows, err := db.QueryContext(ctx, `
SELECT m.id, m.data, p.data FROM message m LEFT JOIN part p ON p.message_id = m.id
WHERE m.session_id = ? ORDER BY m.time_created, m.id, p.time_created, p.id`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	var cur *transcript.Message
	var curID string
	var curData map[string]any
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Role == transcript.Assistant {
			assistant(cur, curData, nil)
		}
		if len(cur.Blocks) > 0 || cur.Usage != nil {
			*msgs = append(*msgs, *cur)
		}
		cur = nil
	}
	started := false // past the first row; cur is nil from then on for a message passed over
	for rows.Next() {
		var mid, msgRaw string
		var partRaw sql.NullString
		if err := rows.Scan(&mid, &msgRaw, &partRaw); err != nil {
			return err
		}
		if !started || curID != mid {
			flush()
			started, curID, curData = true, mid, nil
			// A message row that will not parse is skipped whole: its parts belong to a
			// message that is not there to carry them
			if json.Unmarshal([]byte(msgRaw), &curData) != nil || curData == nil {
				continue
			}
			var role transcript.Role
			switch r := kit.Str(curData["role"]); r {
			case "user":
				role = transcript.User
			case "assistant":
				role = transcript.Assistant
			default:
				// 1.x writes no other role: a message of one is passed over with its parts,
				// once, as one that will not parse is. The role is what says whose its parts
				// are — a tool part is a call the model made because the message is the
				// assistant's — so read under a role guessed for it, its tools would be
				// counted as this session's runs and failures on no evidence
				unread(unknown("role", r))
				continue
			}
			cur = &transcript.Message{ID: mid, Role: role, At: millis(kit.Map(curData["time"])["created"])}
		}
		if cur == nil || !partRaw.Valid {
			continue
		}
		var part map[string]any
		if json.Unmarshal([]byte(partRaw.String), &part) == nil && part != nil && !besideV1(kit.Str(part["type"])) {
			cur.Blocks = append(cur.Blocks, blocksOf(part, unread)...)
		}
	}
	flush()
	return rows.Err()
}
