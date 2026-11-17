// Package antigravity reads Antigravity's sessions: the agy CLI's and the IDE's, which write
// one shape under ~/.gemini, one directory per conversation.
//
//	<gemini>/antigravity-cli/brain/<id>/.system_generated/logs/transcript_full.jsonl   agy
//	<gemini>/antigravity/brain/<id>/.system_generated/logs/transcript_full.jsonl       the IDE
//	<gemini>/antigravity-cli/conversation_summaries.db                                 beside each brain,
//	<gemini>/antigravity/conversation_summaries.db                                     its index
//
// The transcript is a JSONL of steps that is rewritten while the conversation runs — a
// step's completed line can be corrected later — so the latest line for a step_index is the
// truth, and the file is read into a map before it is walked. The index carries each
// conversation's title, workspace and last-active time, which no transcript does.
//
// An API-key provider can run agy with a private --gemini_dir (cc-switch's profiles are the
// common case); that directory has the same shape, and is what a --path points at.
package antigravity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Spec describes Antigravity. Its location is the gemini directory, which holds both
// brains: the IDE alone or the CLI alone is enough, the other is simply empty.
var Spec = sources.Spec{
	Name:  "antigravity",
	Title: "Antigravity",
	// No variable moves it: ~/.gemini is where both write, until agy is given another
	// --gemini_dir
	Locate:  func(env sources.Env) string { return filepath.Join(env.Home, ".gemini") },
	Open:    func(location string) sources.Source { return open(location) },
	Movable: true,
	Resume:  "agy --conversation",
	Store:   "~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript_full.jsonl (agy; the IDE's is antigravity/brain)",
}

// Source is a gemini directory's two brains.
type Source struct {
	dir    string
	brains []*brain // the CLI's first: where an id is in both, it is the CLI's
}

// brain is one root of conversations and the index beside it.
type brain struct {
	dir  string // …/brain
	db   string // …/conversation_summaries.db
	sums kit.Snapshot[map[string]summary]
}

func open(dir string) *Source {
	of := func(name string) *brain {
		base := filepath.Join(dir, name)
		return &brain{dir: filepath.Join(base, "brain"), db: filepath.Join(base, "conversation_summaries.db")}
	}
	return &Source{dir: dir, brains: []*brain{of("antigravity-cli"), of("antigravity")}}
}

func (s *Source) Location() string { return s.dir }

func (s *Source) Exists() bool {
	for _, b := range s.brains {
		if kit.Exists(b.dir) {
			return true
		}
	}
	return false
}

// summary is one conversation_summaries row.
type summary struct {
	title   string
	cwd     string
	updated time.Time
	// raw is the row as written, so a Ref's stamp moves when this row does and not when
	// another conversation's does
	raw string
}

// summaries is what each brain's index says, read once per version of the database — an
// index nobody has written to costs a few stats, not a query — and in the brains' order.
//
// The index is the CLI's answer to "what is this", not the store: a database that is
// missing, locked past the timeout, or of a schema that has moved leaves its conversations
// as they are, titled from what they say.
func (s *Source) summaries(ctx context.Context) []map[string]summary {
	out := make([]map[string]summary, len(s.brains))
	for i, b := range s.brains {
		if !kit.Exists(b.db) {
			continue
		}
		// On a failed read this is what the index last said, which is better than nothing:
		// a session read while the database is briefly locked keeps its title and its
		// directory instead of being stored without them under a stamp that will not move
		out[i], _ = b.sums.Load(b.db, func() (map[string]summary, error) { return readSummaries(ctx, b.db) })
	}
	return out
}

func summaryOf(all []map[string]summary, id string) (summary, bool) {
	for _, m := range all {
		if sum, ok := m[id]; ok {
			return sum, true
		}
	}
	return summary{}, false
}

// readSummaries is the one query behind every conversation's title.
func readSummaries(ctx context.Context, path string) (map[string]summary, error) {
	const table = "conversation_summaries"
	var out map[string]summary
	err := kit.ReadDB(ctx, path, func(db *sql.DB) error {
		out = map[string]summary{}
		if !kit.HasTable(db, table) {
			return nil // an index that has not been written to yet
		}
		cols, err := kit.Columns(db, table)
		if err != nil {
			return err
		}
		if !cols["conversation_id"] {
			return fmt.Errorf("%s has no conversation_id", table)
		}
		col := func(name string) string {
			if cols[name] {
				return name
			}
			return "NULL"
		}
		rows, err := db.QueryContext(ctx, `SELECT conversation_id, `+col("title")+`, `+col("workspace_uris")+`, `+
			col("last_modified_time")+` FROM `+table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, title, uris, modified sql.NullString
			if err := rows.Scan(&id, &title, &uris, &modified); err != nil {
				return err
			}
			if !validID(id.String) {
				continue
			}
			out[id.String] = summary{
				title: kit.NullStr(title),
				cwd:   workspace(kit.NullStr(uris)),
				// ISO in some builds, a naive "2006-01-02 15:04:05.999" in others
				updated: kit.Time(kit.NullStr(modified)),
				raw:     strings.Join([]string{kit.NullStr(title), kit.NullStr(uris), kit.NullStr(modified)}, "\x00"),
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// workspace turns workspace_uris — a JSON array of file:// URIs — into the first path.
func workspace(uris string) string {
	var list []string
	if json.Unmarshal([]byte(uris), &list) != nil {
		return ""
	}
	for _, u := range list {
		rest, ok := strings.CutPrefix(u, "file://")
		if !ok {
			continue
		}
		p, err := url.PathUnescape(rest)
		if err != nil {
			return ""
		}
		// file:///C:/work → /C:/work, where the drive letter does not take a slash
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		return filepath.FromSlash(p)
	}
	return ""
}

// validID: a conversation's directory is named by its id, not by anything.
func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func transcriptPath(dir string) string {
	return filepath.Join(dir, ".system_generated", "logs", "transcript_full.jsonl")
}

// Scan lists the conversations of both brains, one Ref per brain directory. The stamp is
// the transcript's and the conversation's own row of the index: the title lives there, and
// it is not written into the transcript.
func (s *Source) Scan(ctx context.Context) ([]sources.Ref, error) {
	sums := s.summaries(ctx)
	var refs []sources.Ref
	seen := map[string]bool{}
	for _, b := range s.brains {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(b.dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			id := entry.Name()
			if !entry.IsDir() || !validID(id) || seen[id] {
				continue
			}
			dir := filepath.Join(b.dir, id)
			stamp, mod := kit.Stamp(transcriptPath(dir))
			if stamp == "-" {
				continue // a conversation with only the binary store: nothing to read
			}
			seen[id] = true
			sum, _ := summaryOf(sums, id)
			// The index's time moves when the conversation does, even while the
			// transcript's write is still in flight
			if !sum.updated.IsZero() {
				mod = sum.updated
			}
			refs = append(refs, sources.Ref{Key: dir, Stamp: stamp + "|" + sum.raw, ModTime: mod})
		}
	}
	return refs, nil
}

// step is one line of a transcript. The counters are agy's own and are read loosely.
type step struct {
	// Read as whatever it is and checked afterwards: a step is placed by its index, and
	// one whose index is not a number has no place
	Index     any    `json:"step_index"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CreatedAt any    `json:"created_at"`
	Model     string `json:"model"`
	Content   any    `json:"content"`

	InputTokens      any `json:"input_tokens"`
	OutputTokens     any `json:"output_tokens"`
	CacheReadTokens  any `json:"cache_read_tokens"`
	CacheWriteTokens any `json:"cache_write_tokens"`
}

// Read parses one conversation: the index's word on what it is called and where it was
// held, then its steps in order.
func (s *Source) Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error {
	id := filepath.Base(ref.Key)
	w.S.ID = id
	w.S.Path = ref.Key
	if sum, ok := summaryOf(s.summaries(ctx), id); ok {
		// What the CLI itself calls the conversation, where it held it, and when it last
		// moved; a conversation the index does not know is titled from its first ask and
		// timed by its steps
		w.S.Title = sum.title
		w.S.Cwd = sum.cwd
		w.S.UpdatedAt = sum.updated
	}

	// The CLI rewrites a step's line as the turn finishes: the file order is the order of
	// the writes, and the last write for an index is the step
	byIndex := map[int64]*step{}
	n := 0
	err := kit.Lines(transcriptPath(ref.Key), func(line []byte) bool {
		if n++; n&1023 == 0 && ctx.Err() != nil {
			return false
		}
		// A field of an unexpected type costs that field, not the step: agy changes what
		// it writes, and a step is worth more than its strictest reading
		var st step
		err := json.Unmarshal(line, &st)
		var mistyped *json.UnmarshalTypeError
		if index, ok := kit.Num(st.Index); ok && (err == nil || errors.As(err, &mistyped)) {
			byIndex[int64(index)] = &st
		}
		return true
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	indexes := make([]int64, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })

	for _, index := range indexes {
		st := byIndex[index]
		m := transcript.Message{ID: strconv.FormatInt(index, 10), At: kit.Time(st.CreatedAt)}
		text := kit.ContentText(st.Content)
		switch st.Type {
		case "USER_INPUT":
			if text = userRequest(text); strings.TrimSpace(text) == "" {
				continue
			}
			m.Role = transcript.User
			m.Blocks = []transcript.Block{transcript.TextOf(text)}
		case "PLANNER_RESPONSE":
			m.Role = transcript.Assistant
			m.Model = st.Model
			m.Usage = st.usage()
			if text != "" {
				m.Blocks = []transcript.Block{transcript.TextOf(text)}
			}
		case "RUN_COMMAND", "COMMAND_STATUS", "SEND_COMMAND_INPUT", "READ_TERMINAL",
			"VIEW_FILE", "VIEW_FILE_OUTLINE", "VIEW_CODE_ITEM", "VIEW_CONTENT_CHUNK",
			"LIST_DIRECTORY", "FIND", "GREP_SEARCH", "CODE_ACTION",
			"READ_URL_CONTENT", "SEARCH_WEB", "MCP_TOOL", "LIST_RESOURCES", "READ_RESOURCE",
			"BROWSER_SUBAGENT", "GENERATE_IMAGE",
			"CHECKPOINT", "TASK_BOUNDARY", "EPHEMERAL_MESSAGE",
			"CONVERSATION_HISTORY", "KNOWLEDGE_ARTIFACTS":
			// The steps of agy's tools — a command and its status, input and terminal; a file
			// viewed, listed, found, searched or edited; a page read, a web search; an MCP tool
			// or resource; the browser subagent; an image made — and of agy itself: a
			// checkpoint, a task's boundary, a message it puts before the model, the history
			// and knowledge it brings in. Each is shown under its type as what the step says,
			// not taken apart into a call and its result.
			//
			// Only RUN_COMMAND and CHECKPOINT are in the samples of agy's transcripts this
			// reader was built from; the rest are the Cascade step types agy's names come from.
			// An event is what each of these is, so a name agy never writes costs nothing.
			// NOTIFY_USER, by its name agy's word to the person in task mode, and ERROR_MESSAGE,
			// by its name a failure, are not among them: shown as an event, either would be
			// misread, so each is counted below until what it holds has been seen
			m.Role = transcript.System
			m.Blocks = []transcript.Block{transcript.EventOf(st.Type, text)}
		default:
			// A step of a type agy had not written when this reader was made, or one this
			// reader does not yet read as what it is. It stays visible under that name,
			// neither dropped nor passed off as the model's reply, and it is counted: a reply
			// renamed would otherwise read as an event with nothing to say so
			w.Unread("step:" + st.Type)
			m.Role = transcript.System
			m.Blocks = []transcript.Block{transcript.EventOf(st.Type, text)}
		}
		w.Add(m)
	}
	return nil
}

// usage is a finished model step's counters in the canonical shape. agy records
// input_tokens, output_tokens, cache_read_tokens and cache_write_tokens, and nothing about
// thinking. Measured when the source was added: input_tokens excludes the cache, which is
// counted beside it, and output_tokens includes the thinking. Those are the canonical
// shape's own terms, so the counters are taken as recorded — where a CLI that counts the
// cache inside its input (Codex, Gemini) has it taken out — and Reasoning stays unset
// rather than guessed at.
//
// A step that is not DONE is the CLI's work in progress: its counters are provisional, and
// the line that finishes the step carries the real ones. A counter is read by the one rule
// for what a count is (kit.Count): one below zero or past model.MaxTokens counts 0, as one
// never recorded, and a step with nothing counted has no usage.
func (st *step) usage() *model.Usage {
	if st.Status != "DONE" {
		return nil
	}
	u := model.Usage{
		Input:      kit.Count(st.InputTokens),
		Output:     kit.Count(st.OutputTokens),
		CacheRead:  kit.Count(st.CacheReadTokens),
		CacheWrite: kit.Count(st.CacheWriteTokens),
	}
	if u.IsZero() {
		return nil
	}
	return &u
}

// userRequest is what the person typed: agy wraps it in <USER_REQUEST> and appends context
// blocks of its own (metadata, settings changes) after it.
func userRequest(content string) string {
	_, rest, found := strings.Cut(content, "<USER_REQUEST>")
	if !found {
		return content
	}
	inner, _, found := strings.Cut(rest, "</USER_REQUEST>")
	if !found || strings.TrimSpace(inner) == "" {
		return content
	}
	return strings.TrimSpace(inner)
}
