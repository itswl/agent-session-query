// Package openclaw reads OpenClaw's sessions.
//
//	<state>/agents/<agent>/agent/openclaw-agent.sqlite   2026.9 and later: one database
//	                                                     per agent
//	<state>/agents/default/sessions/sessions.json        before that: an index, and a
//	<state>/agents/default/sessions/<id>.jsonl             transcript per session
//
// The database's rows are the JSON the transcript files held, unchanged:
//
//	session_windows     the sessions: status, model, display_name, unix-millisecond times
//	transcript_events   (session_id, seq) → event_json: a type=session row carries the
//	                    cwd, a message row the message (pi's shape: role, a content block
//	                    array, stopReason, usage; a tool's result a message of its own,
//	                    role toolResult, carrying toolCallId and toolName)
//
// The older layout is read when no agent database exists.
package openclaw

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/jsonmap"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"

	"github.com/klauspost/compress/zstd"
)

// Spec describes OpenClaw.
var Spec = sources.Spec{
	Name:  "openclaw",
	Title: "OpenClaw",
	Locate: func(env sources.Env) string {
		// OPENCLAW_STATE_DIR moves the whole state directory; every agent's database
		// lives under its agents/
		if d := env.Getenv("OPENCLAW_STATE_DIR"); d != "" {
			return d
		}
		return filepath.Join(env.Home, ".openclaw")
	},
	Open: func(location string) sources.Source {
		return &Source{state: location, index: jsonmap.At(filepath.Join(location, "agents", "default", "sessions")),
			lists: map[string]*kit.Snapshot[[]sources.Ref]{}}
	},
	// The store spans a directory of agents: no one path relocates it
	Movable: false,
	// OpenClaw can resume by session id, which its help does not say — the query is
	// matched against a list built from titles, labels, ids and keys. What rules the
	// command out is the candidate set: that list is the last 50 sessions still inside
	// the recent-activity window, fetched from a running Gateway, while this covers every
	// session on disk. A command that works for the newest handful and fails for the
	// rest is worse than none.
	Resume: "",
	Reads:  2,
	Store:  "~/.openclaw/agents/<agent>/agent/openclaw-agent.sqlite",
}

// Source is one OpenClaw state directory.
type Source struct {
	state string
	index *jsonmap.Index

	mu    sync.Mutex
	lists map[string]*kit.Snapshot[[]sources.Ref] // one per agent database
}

func (s *Source) Location() string { return s.state }

// databases finds every agent's database. The layout is uniform, so an agent added
// later is picked up without anything being told.
func (s *Source) databases() []string {
	matches, _ := filepath.Glob(filepath.Join(s.state, "agents", "*", "agent", "openclaw-agent.sqlite"))
	return matches
}

func (s *Source) Exists() bool { return len(s.databases()) > 0 || s.index.Exists() }

// Scan lists the sessions of every agent database — or, where there is none, of the
// older index.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	dbs := s.databases()
	if len(dbs) == 0 {
		entries, err := s.index.Entries()
		if err != nil {
			return nil, err
		}
		refs := make([]sources.Ref, 0, len(entries))
		for _, e := range entries {
			if e.File != "" { // a record with no transcript names a session with nothing to read
				refs = append(refs, s.index.Ref(e))
			}
		}
		return refs, nil
	}
	var refs []sources.Ref
	var unread error
	for _, db := range dbs {
		s.mu.Lock()
		list := s.lists[db]
		if list == nil {
			list = &kit.Snapshot[[]sources.Ref]{}
			s.lists[db] = list
		}
		s.mu.Unlock()
		// One agent's database that cannot be read — locked, half-created, of a layout
		// this does not know — is not every agent's: the others are listed, with what
		// this one last said if it ever said anything, and the error says the list is
		// short, so that nothing missing from it is taken for deleted
		found, err := list.Load(db, func() ([]sources.Ref, error) { return listDB(ctx, db) })
		if err != nil {
			unread = errors.Join(unread, fmt.Errorf("%s: %w", db, err))
		}
		refs = append(refs, found...)
	}
	return refs, unread
}

// listDB lists one agent database's sessions with one query. A session's stamp is what
// its own rows say about change: when its window and its transcript were last written,
// how far its events run, and what the list shows of it.
func listDB(ctx context.Context, path string) ([]sources.Ref, error) {
	var refs []sources.Ref
	err := kit.ReadDB(ctx, path, func(db *sql.DB) error {
		refs = refs[:0]
		if !kit.HasTable(db, "session_windows") || !kit.HasTable(db, "transcript_events") {
			return errors.New("not a layout this reads: no session_windows or transcript_events")
		}
		rows, err := db.QueryContext(ctx, `
SELECT w.session_id, w.updated_at, w.transcript_updated_at, w.status, w.display_name,
       COALESCE(w.model_provider, '') || '/' || COALESCE(w.model, '') || '@' || COALESCE(w.started_at, 0),
       (SELECT MAX(e.seq) FROM transcript_events e WHERE e.session_id = w.session_id)
FROM session_windows w`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			// The figures are read as whatever SQLite holds, since a column takes a value of
			// any type: one row's time written as text is read as none, and does not make
			// the agent's whole database look unreadable
			var id, status, name, mdl sql.NullString
			var updatedV, transcriptV, seqV any
			if err := rows.Scan(&id, &updatedV, &transcriptV, &status, &name, &mdl, &seqV); err != nil {
				return err
			}
			if id.String == "" {
				continue
			}
			updated, transcript, seq := kit.Int(updatedV), kit.Int(transcriptV), kit.Int(seqV)
			refs = append(refs, sources.Ref{
				Key: path + "#" + id.String,
				Stamp: strings.Join([]string{strconv.FormatInt(updated, 36), strconv.FormatInt(transcript, 36),
					strconv.FormatInt(seq, 36), jsonmap.StampOf(status.String, name.String, mdl.String)}, "."),
				ModTime: time.UnixMilli(max(updated, transcript)),
			})
		}
		return rows.Err()
	})
	return refs, err
}

// Read parses one session.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	// The session id is what follows the last #: the path before it may hold one too
	at := strings.LastIndexByte(ref.Key, '#')
	if at < 0 || !strings.HasSuffix(ref.Key[:at], ".sqlite") {
		return s.readFile(ctx, ref, w)
	}
	db, id := ref.Key[:at], ref.Key[at+1:]
	w.S.ID, w.S.Path = id, db
	var msgs []transcript.Message
	var sess model.Session
	var unread []string // what the reader passed over not knowing it, told to w once the read is done
	err := kit.ReadDB(ctx, db, func(conn *sql.DB) error {
		msgs, sess, unread = msgs[:0], w.S, unread[:0]
		// started_at and ended_at are a run's, not the session's: a run sets started_at and
		// clears ended_at as it begins, and writes ended_at as it ends. When the session
		// began is created_at, which came with 2026.11; before it, started_at is all there is.
		created := "NULL"
		if cols, err := kit.Columns(conn, "session_windows"); err == nil && cols["created_at"] {
			created = "created_at"
		}
		var status, mdl, name sql.NullString
		var beganV, startedV, endedV, updatedV any // whatever the column holds: see listDB
		err := conn.QueryRowContext(ctx, `
SELECT status, model, display_name, `+created+`, started_at, ended_at, COALESCE(transcript_updated_at, updated_at)
FROM session_windows WHERE session_id = ?`, id).Scan(&status, &mdl, &name, &beganV, &startedV, &endedV, &updatedV)
		if err != nil {
			return err
		}
		// display_name is set for a session that came through a chat channel; a run from
		// the command line leaves it empty, and its first ask is its title
		sess.Title = name.String
		// The model by its own name, as each response names it: with the provider in front
		// the session and its calls would name one model two ways, and usage by model
		// would count them apart
		sess.Model = mdl.String
		began := kit.Int(beganV)
		if began <= 0 {
			began = kit.Int(startedV)
		}
		sess.StartedAt, sess.UpdatedAt = jsonmapMillis(began), jsonmapMillis(kit.Int(updatedV))
		// A run in flight is one begun and not ended: 2026.11 clears the status as a run
		// starts and writes it once the run is done, where earlier versions wrote running.
		// A Gateway stopped mid-run leaves its run so until it restarts and carries the run
		// on. A run with --local records neither time, and is not seen running.
		sess.Running = status.String == "running" || !status.Valid && startedV != nil && endedV == nil

		// A large event is stored compressed: event_json is NULL and event_zstd holds it.
		// The column came with that change, so a database from before it has only the text.
		packed := "NULL"
		if cols, err := kit.Columns(conn, "transcript_events"); err == nil && cols["event_zstd"] {
			packed = "event_zstd"
		}
		rows, err := conn.QueryContext(ctx, `SELECT event_json, `+packed+` FROM transcript_events WHERE session_id = ? ORDER BY seq`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		reader := &jsonmap.Reader{Unread: func(kind string) { unread = append(unread, kind) }}
		for rows.Next() {
			var text sql.NullString
			var zst []byte
			if err := rows.Scan(&text, &zst); err != nil {
				return err
			}
			event := []byte(text.String)
			if !text.Valid && len(zst) > 0 {
				if event, err = unpack(zst); err != nil {
					continue // one event that cannot be unpacked does not cost the session
				}
			}
			if m, ok := reader.Row(event); ok {
				msgs = append(msgs, m)
			}
			if m, ok := reader.Take(); ok {
				msgs = append(msgs, m)
			}
		}
		sess.Cwd = reader.Cwd
		if err := rows.Err(); err != nil {
			return err
		}
		// Cold storage moves a session's events out of transcript_events, to a file or a
		// table of its own, which this does not read: what was moved is missing from the
		// session. The session says so where it begins, which is where the events moved
		// come from, and doctor counts it
		if kit.HasTable(conn, "session_transcript_cold_archives") {
			var events any // whatever the column holds: see listDB
			if conn.QueryRowContext(ctx, `SELECT event_count FROM session_transcript_cold_archives WHERE session_id = ?`, id).Scan(&events) == nil {
				said := "Some of this session's events were moved to OpenClaw's cold storage, which asq does not read: they are not here."
				if n := kit.Int(events); n > 0 {
					said = fmt.Sprintf("%d of this session's events were moved to OpenClaw's cold storage, which asq does not read: they are not here.", n)
				}
				at := sess.StartedAt
				if len(msgs) > 0 && !msgs[0].At.IsZero() && (at.IsZero() || msgs[0].At.Before(at)) {
					at = msgs[0].At
				}
				msgs = append([]transcript.Message{{Role: transcript.System, At: at,
					Blocks: []transcript.Block{transcript.EventOf(model.EventColdStorage, said)}}}, msgs...)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.S = sess
	for _, m := range msgs {
		w.Add(m)
	}
	for _, kind := range unread {
		w.Unread(kind)
	}
	return nil
}

// OpenClaw caps an event at 4 MiB before compressing it; a frame that claims to unpack
// to more than this is not one of its events.
const maxEventBytes = 16 << 20

var unpacker, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(maxEventBytes))

// unpack decodes one compressed event.
func unpack(frame []byte) ([]byte, error) { return unpacker.DecodeAll(frame, nil) }

func jsonmapMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// readFile reads a session of the older layout: its transcript file, and what the index
// says about it.
func (s *Source) readFile(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	entry, _ := s.index.ByFile(ref.Key)
	w.S.Path = ref.Key
	w.S.ID = kit.First(entry.ID, strings.TrimSuffix(filepath.Base(ref.Key), ".jsonl"))
	if info := entry.Info; info != nil {
		w.S.Model = kit.Str(info["model"])
		w.S.UpdatedAt = entry.Updated()
		w.S.Running = kit.Str(info["status"]) == "running"
	}
	reader := &jsonmap.Reader{Model: w.S.Model, Unread: w.Unread}
	if err := reader.ReadFile(ctx, ref.Key, w); err != nil {
		return err
	}
	w.S.Cwd = reader.Cwd
	return nil
}
