package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
	"github.com/itswl/agent-session-query/internal/transcript"
)

type M = map[string]any

var start = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

// ms is an event's time, as the harness writes it: epoch milliseconds
func ms(offset time.Duration) int64 { return start.Add(offset).UnixMilli() }

func text(s string) []M { return []M{{"type": "text", "text": s}} }

func headerRow(id, cwd string) M {
	return M{"type": "session", "version": 4, "id": id, "cwd": cwd, "createdAt": start.Unix()}
}

func userRow(id string, at time.Duration, content any) M {
	return M{"type": "user/message", "surfaceOp": M{"op": "append"}, "time": ms(at),
		"data": M{"id": id, "role": "user", "source": M{"kind": "user"}, "content": content}}
}

func assistantRow(id string, at time.Duration, usage M, content ...M) M {
	return M{"type": "assistant/message", "time": ms(at), "data": M{
		"message": M{"id": id, "source": M{"kind": "model", "provider": "deepseek-account", "model": "deepseek-flash"}, "content": content},
		"usage":   usage}}
}

func resultRow(id, callID string, at time.Duration, isError bool, content any) M {
	return M{"type": "tool/result", "time": ms(at), "data": M{
		"message": M{"id": id, "role": "tool", "toolCallId": callID, "isError": isError, "content": content}}}
}

// transcriptRows is one session: the header, two turns' worth of events with a tool call,
// a message id written twice, and a compaction's replacement row.
func transcriptRows() []M {
	return []M{
		headerRow("dsh-1", "/w/proj"),
		userRow("u1", 0, text("fix the uploader")),
		assistantRow("a1", time.Second, M{"inputTokens": 100, "outputTokens": 20, "cacheReadTokens": 300, "cacheWriteTokens": 5, "totalTokens": 425},
			M{"type": "text", "text": "looking at it"},
			// the harness writes a call's arguments as a string holding JSON
			M{"type": "tool-call", "id": "c1", "name": "read", "arguments": `{"path":"a.go"}`}),
		resultRow("r1", "c1", 2*time.Second, false, text("package x")),
		assistantRow("a2", 3*time.Second, M{"inputTokens": 10, "outputTokens": 20}, M{"type": "text", "text": "done"}),
		// A repeated id is the same message written again
		assistantRow("a1", 4*time.Second, nil, M{"type": "text", "text": "dup"}),
		// A compaction replacement is model-visible only, never a transcript row
		M{"type": "user/message", "surfaceOp": M{"op": "replace", "startSeq": 1, "endSeq": 3}, "time": ms(5 * time.Second),
			"data": M{"id": "summary", "content": text("compacted summary")}},
	}
}

func encode(t testing.TB, rows []M) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// frames compresses data the way event batches arrive: small independent frames
func frames(t testing.TB, data []byte, size int) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for from := 0; from < len(data); from += size {
		out.Write(enc.EncodeAll(data[from:min(from+size, len(data))], nil))
	}
	return out.Bytes()
}

func put(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionDir(root string) string { return filepath.Join(root, "project", "session-abc") }

// halfFrame is the beginning of a frame still being written: a row cut off mid-line, with
// the frame itself cut short
func halfFrame(t testing.TB) []byte {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	tail := enc.EncodeAll([]byte(`{"type":"tool/result","time":1,"data":{"message":{"id":"partial",`), nil)
	return tail[:len(tail)/2]
}

// checkSession asserts what transcriptRows add up to, however they were stored
func checkSession(t *testing.T, s sourcetest.Session) {
	t.Helper()
	if s.Session.ID != "dsh-1" || s.Session.Cwd != "/w/proj" || s.Session.Model != "deepseek-flash" {
		t.Errorf("session = %+v", s.Session)
	}
	if s.Session.Title != "fix the uploader" || !s.Session.StartedAt.Equal(start) {
		t.Errorf("title %q, started %s", s.Session.Title, s.Session.StartedAt)
	}
	// the last message is the one at 3 s: the repeat and the replacement are not rows
	if !s.Session.UpdatedAt.Equal(start.Add(3 * time.Second)) {
		t.Errorf("updated %s", s.Session.UpdatedAt)
	}
	if s.Stats.Turns != 1 {
		t.Fatalf("turns = %d", s.Stats.Turns)
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt reply tool reply" {
		t.Errorf("items = %s", got)
	}
	if turn.Ask != "fix the uploader" || turn.Answer != "done" {
		t.Errorf("ask %q answer %q", turn.Ask, turn.Answer)
	}
	tools := sourcetest.Tools(turn)
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	tool := tools[0]
	if tool.Name != "read" || tool.Kind != model.ToolRead || tool.CallID != "c1" || len(tool.Files) != 1 || tool.Files[0] != "a.go" {
		t.Errorf("call = %+v", tool)
	}
	if !tool.Done || tool.Output != "package x" || tool.Status != model.StatusOK {
		t.Errorf("result = %+v", tool)
	}
	// inputTokens leaves the cache out already: the counters are the canonical ones as they are
	if len(turn.Calls) != 2 || turn.Calls[0].Usage == nil || *turn.Calls[0].Usage != (model.Usage{Input: 100, Output: 20, CacheRead: 300, CacheWrite: 5}) {
		t.Fatalf("calls = %+v", turn.Calls)
	}
	if want := (model.Usage{Input: 110, Output: 40, CacheRead: 300, CacheWrite: 5}); s.Stats.Usage != want {
		t.Errorf("usage = %+v, want %+v", s.Stats.Usage, want)
	}
	for _, it := range turn.Items {
		if it.Text == "dup" || it.Text == "compacted summary" {
			t.Errorf("a row that is not part of the transcript was read: %+v", it)
		}
	}
}

func TestSession(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl"), encode(t, transcriptRows()))
	src := Spec.Open(root)
	if !src.Exists() {
		t.Fatal("the sessions directory exists")
	}
	s := sourcetest.One(t, src)
	checkSession(t, s)
	sourcetest.NothingUnread(t, s)
	if s.Session.Path != sessionDir(root) {
		t.Errorf("path = %s", s.Session.Path)
	}
}

// TestZstdFrames: a compressed transcript is a chain of independent frames with a
// half-written tail; the complete frames read, the tail is dropped with its partial line.
func TestZstdFrames(t *testing.T) {
	root := t.TempDir()
	data := append(frames(t, encode(t, transcriptRows()), 97), halfFrame(t)...)
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl.zstd"), data)
	s := sourcetest.One(t, Spec.Open(root))
	checkSession(t, s)
	sourcetest.NothingUnread(t, s)
}

// TestZstdTailCutAnywhere: the harness appends to the file while it is read, so the last
// frame can end at any byte — inside its magic number, its header, its blocks
func TestZstdTailCutAnywhere(t *testing.T) {
	complete := frames(t, encode(t, transcriptRows()), 97)
	enc, _ := zstd.NewWriter(nil)
	next := enc.EncodeAll(bytes.Repeat([]byte(`{"type":"assistant/message","time":1,"data":{"message":{"id":"more","content":[{"type":"text","text":"x"}]}}}`+"\n"), 3), nil)
	for cut := 0; cut < len(next); cut++ {
		root := t.TempDir()
		put(t, filepath.Join(sessionDir(root), "session.v4.jsonl.zstd"), append(append([]byte{}, complete...), next[:cut]...))
		s := sourcetest.One(t, Spec.Open(root))
		if s.Stats.Turns != 1 || s.Stats.Calls != 2 || s.Session.ID != "dsh-1" {
			t.Fatalf("a tail cut at %d of %d bytes: session %+v stats %+v", cut, len(next), s.Session, s.Stats)
		}
		// a line cut short is the append still being written, not a record of some other kind
		sourcetest.NothingUnread(t, s)
	}
	// The whole next frame is complete rows, and read
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl.zstd"), append(append([]byte{}, complete...), next...))
	s := sourcetest.One(t, Spec.Open(root))
	if s.Stats.Calls != 3 {
		t.Errorf("a complete frame was not read: %+v", s.Stats)
	}
	sourcetest.NothingUnread(t, s)
}

// TestPartialLastLine: a plain transcript's last line may be an append still being written
func TestPartialLastLine(t *testing.T) {
	root := t.TempDir()
	data := append(encode(t, transcriptRows()), []byte(`{"type":"assistant/message","time":1,"data":{"message":{"id":"half","content":[{"type":"te`)...)
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl"), data)
	s := sourcetest.One(t, Spec.Open(root))
	checkSession(t, s)
	sourcetest.NothingUnread(t, s)

	// a row whose newline is not written yet is complete all the same
	whole := bytes.TrimSuffix(encode(t, append(transcriptRows(), assistantRow("a3", 6*time.Second, M{"outputTokens": 1}, M{"type": "text", "text": "late"}))), []byte("\n"))
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl"), whole)
	s = sourcetest.One(t, Spec.Open(root))
	if s.Stats.Calls != 3 || s.Turn(t, 1).Answer != "late" {
		t.Errorf("stats = %+v", s.Stats)
	}
	sourcetest.NothingUnread(t, s)
}

// TestFirstFrameStillBeingWritten: a session that has only just begun may hold nothing but
// half a frame — that is an empty session for now, not a broken one
func TestFirstFrameStillBeingWritten(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v1.jsonl.zstd"), halfFrame(t))
	s := sourcetest.One(t, Spec.Open(root))
	if s.Session.ID != "session-abc" || s.Stats.Turns != 0 {
		t.Errorf("session = %+v stats = %+v", s.Session, s.Stats)
	}
	sourcetest.NothingUnread(t, s)
}

// TestNotZstd: bytes that are no zstd stream are a failure to read, which is not an empty session
func TestNotZstd(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v1.jsonl.zstd"), []byte("this is plain text, not a zstd stream\n"))
	src := Spec.Open(root)
	refs, err := src.Scan(context.Background())
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs %v, err %v", refs, err)
	}
	if err := src.Read(context.Background(), refs[0], transcript.New(nil)); err == nil {
		t.Error("garbage read as a transcript")
	}
}

// TestHighestGeneration: a session directory keeps several rotating generations; the
// highest is the current transcript
func TestHighestGeneration(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(root)
	put(t, filepath.Join(dir, "session.v3.jsonl"), encode(t, []M{headerRow("old", "/w"), userRow("u", 0, text("old title"))}))
	put(t, filepath.Join(dir, "session.v10.jsonl.zstd"), frames(t, encode(t, []M{headerRow("cur", "/w"), userRow("u2", 0, text("current title"))}), 50))
	put(t, filepath.Join(dir, "session.v9.jsonl"), encode(t, []M{headerRow("nine", "/w"), userRow("u3", 0, text("v9 is not newer than v10"))}))
	put(t, filepath.Join(dir, "events.log"), []byte("not a transcript"))
	s := sourcetest.One(t, Spec.Open(root))
	if s.Session.ID != "cur" || s.Session.Title != "current title" {
		t.Errorf("session = %+v: v10 is the highest, not v9", s.Session)
	}
	sourcetest.NothingUnread(t, s)
}

func TestGeneration(t *testing.T) {
	cases := map[string]struct {
		version int
		ok      bool
	}{
		"session.v4.jsonl":       {4, true},
		"session.v12.jsonl.zstd": {12, true},
		"session.v0.jsonl":       {0, true},
		"session.vX.jsonl":       {},
		"session.v-1.jsonl":      {},
		"transcript.jsonl":       {},
		"session.v4.json":        {},
	}
	for name, want := range cases {
		if v, ok := generation(name); v != want.version || ok != want.ok {
			t.Errorf("generation(%q) = %d, %v; want %d, %v", name, v, ok, want.version, want.ok)
		}
	}
}

// TestScan: one session per directory, keyed by the directory; the stamp is the current
// generation's, so a rotation moves it and another session's writes do not
func TestScan(t *testing.T) {
	root := t.TempDir()
	one := filepath.Join(root, "p1", "s1")
	two := filepath.Join(root, "p2", "s2")
	put(t, filepath.Join(one, "session.v1.jsonl"), encode(t, []M{headerRow("one", "/w"), userRow("u", 0, text("first"))}))
	put(t, filepath.Join(two, "session.v1.jsonl"), encode(t, []M{headerRow("two", "/w"), userRow("u", 0, text("second"))}))
	// none of these is a session
	put(t, filepath.Join(root, "p1", "stray.jsonl"), []byte("x"))
	put(t, filepath.Join(root, "p1", "empty", "notes.txt"), []byte("no transcript in here"))
	put(t, filepath.Join(root, "README"), []byte("x"))

	src := Spec.Open(root)
	scan := func() map[string]string {
		refs, err := src.Scan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range refs {
			if r.ModTime.IsZero() {
				t.Errorf("%s has no modification time", r.Key)
			}
			out[r.Key] = r.Stamp
		}
		return out
	}
	before := scan()
	if len(before) != 2 || before[one] == "" || before[two] == "" {
		t.Fatalf("refs = %v: a session is a directory holding a transcript", before)
	}
	if again := scan(); again[one] != before[one] || again[two] != before[two] {
		t.Errorf("stamps moved with nothing changed: %v / %v", before, again)
	}

	// one session is appended to: its stamp moves, the other's holds
	put(t, filepath.Join(one, "session.v1.jsonl"), encode(t, []M{headerRow("one", "/w"), userRow("u", 0, text("first")), assistantRow("a", time.Second, nil, M{"type": "text", "text": "ok"})}))
	appended := scan()
	if appended[one] == before[one] || appended[two] != before[two] {
		t.Errorf("after an append: %v / %v", before, appended)
	}

	// rotation: a new generation, however small, is the session's current transcript
	put(t, filepath.Join(one, "session.v2.jsonl"), []byte("x"))
	rotated := scan()
	if rotated[one] == appended[one] || rotated[two] != before[two] {
		t.Errorf("after a rotation: %v / %v", appended, rotated)
	}
}

// TestRotationMovesTheStampEvenWhenTheFileLooksTheSame
func TestRotationMovesTheStampEvenWhenTheFileLooksTheSame(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "s")
	body := encode(t, []M{headerRow("one", "/w")})
	path := filepath.Join(dir, "session.v1.jsonl")
	put(t, path, body)
	src := Spec.Open(root)
	refs, _ := src.Scan(context.Background())
	if len(refs) != 1 {
		t.Fatal(refs)
	}
	// the next generation, the same bytes at the same time
	next := filepath.Join(dir, "session.v2.jsonl")
	put(t, next, body)
	info, _ := os.Stat(path)
	if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if again, _ := src.Scan(context.Background()); len(again) != 1 || again[0].Stamp == refs[0].Stamp || again[0].Key != refs[0].Key {
		t.Errorf("before %+v after %+v", refs, again)
	}
}

func TestScanOfAMissingStore(t *testing.T) {
	src := Spec.Open(filepath.Join(t.TempDir(), "never-made"))
	if src.Exists() {
		t.Error("a store that is not there exists")
	}
	if refs, err := src.Scan(context.Background()); err != nil || len(refs) != 0 {
		t.Errorf("refs %v, err %v: a store that is not there is empty, not broken", refs, err)
	}
}

// TestSameGenerationBothWays: while the harness compresses a generation both files are there,
// and the compressed one — the later name — is the one read
func TestSameGenerationBothWays(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(root)
	put(t, filepath.Join(dir, "session.v2.jsonl"), encode(t, []M{headerRow("plain", "/w")}))
	put(t, filepath.Join(dir, "session.v2.jsonl.zstd"), frames(t, encode(t, []M{headerRow("packed", "/w")}), 40))
	s := sourcetest.One(t, Spec.Open(root))
	if s.Session.ID != "packed" {
		t.Errorf("id = %q", s.Session.ID)
	}
	sourcetest.NothingUnread(t, s)
}

// TestNoHeader: a transcript that opens without its header is still a session, named by its directory
func TestNoHeader(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v2.jsonl"), encode(t, []M{userRow("u1", 0, text("hello"))}))
	s := sourcetest.One(t, Spec.Open(root))
	if s.Session.ID != "session-abc" || s.Session.Cwd != "" || !s.Session.StartedAt.Equal(start) {
		// StartedAt comes from the first message, there being no createdAt
		t.Errorf("session = %+v", s.Session)
	}
	sourcetest.NothingUnread(t, s)
}

// TestInjectedUserRows: a user row the harness wrote itself is context, and no ask
func TestInjectedUserRows(t *testing.T) {
	root := t.TempDir()
	injected := userRow("u0", 0, text("Project instructions: be brief."))
	injected["data"].(M)["source"] = M{"kind": "context"}
	put(t, filepath.Join(sessionDir(root), "session.v1.jsonl"), encode(t, []M{
		headerRow("dsh-ctx", "/w"),
		injected,
		userRow("u1", time.Second, "a bare string is one text block"),
		assistantRow("a1", 2*time.Second, nil, M{"type": "reasoning", "text": "thinking it over"}, M{"type": "text", "text": "answer"},
			M{"type": "tool-call", "id": "c9", "name": "bash", "arguments": M{"command": "ls"}}),
	}))
	s := sourcetest.One(t, Spec.Open(root))
	if got := sourcetest.Kinds(s.Turn(t, 0)); got != "context" {
		t.Errorf("turn 0 = %s", got)
	}
	turn := s.Turn(t, 1)
	if s.Stats.Turns != 1 || turn.Ask != "a bare string is one text block" {
		t.Fatalf("turns = %+v", s.Turns)
	}
	if got := sourcetest.Kinds(turn); got != "prompt thinking reply tool" {
		t.Errorf("items = %s", got)
	}
	// arguments that arrive as an object are an object
	if tool := sourcetest.Tools(turn)[0]; tool.Command != "ls" || tool.Done {
		t.Errorf("tool = %+v", tool)
	}
	sourcetest.NothingUnread(t, s)
}

// TestToolFailure: isError is how a result says it failed
func TestToolFailure(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v1.jsonl"), encode(t, []M{
		headerRow("dsh-f", "/w"),
		userRow("u1", 0, text("build")),
		assistantRow("a1", time.Second, nil, M{"type": "tool-call", "id": "c1", "name": "bash", "arguments": `{"command":"make"}`}),
		resultRow("r1", "c1", 2*time.Second, true, text("make: *** failed")),
	}))
	s := sourcetest.One(t, Spec.Open(root))
	turn := s.Turn(t, 1)
	if tool := sourcetest.Tools(turn)[0]; tool.Status != model.StatusError || tool.Output != "make: *** failed" || tool.Name != "bash" || turn.Failures != 1 {
		t.Errorf("tool = %+v, failures %d", sourcetest.Tools(turn)[0], turn.Failures)
	}
	sourcetest.NothingUnread(t, s)
}

func TestLocate(t *testing.T) {
	if got := Spec.Locate(sourcetest.Env("/home/u")); got != filepath.Join("/home/u", ".dsh", "sessions") {
		t.Errorf("default = %s", got)
	}
	if got := Spec.Locate(sourcetest.Env("/home/u", "DSH_HOME", "/moved")); got != filepath.Join("/moved", "sessions") {
		t.Errorf("relocated = %s", got)
	}
}

// currentRows is a session as a real harness (0.2.2) writes it, with what the samples
// before it did not show: surfaceOp as a bare string, the title it gives a session — a
// stopgap cut from the ask, then one a model wrote — a command's exit code, which is not on
// its result at all but on the job that ran it, and the rows it writes that are no part of
// the conversation (the system prompt, a turn's bounds, a call as it was run, a job as it
// began).
func currentRows() []M {
	return []M{
		{"type": "session", "version": 4, "id": "session-real", "cwd": "/w/proj", "createdAt": ms(0)},
		{"type": "turn/start", "seq": 5, "time": ms(0), "data": M{"turn": 1}},
		{"type": "system/message", "seq": 8, "time": ms(time.Second), "surfaceOp": "append",
			"data": M{"message": M{"role": "system", "content": text("the system prompt"), "source": M{"kind": "system-prompt"}, "id": "sys"}}},
		{"type": "user/message", "seq": 9, "time": ms(time.Second), "surfaceOp": "append",
			"data": M{"id": "u1", "role": "user", "source": M{"kind": "user"}, "content": text("read the notes, then list the missing directory")}},
		{"type": "user/message", "seq": 10, "time": ms(time.Second), "surfaceOp": "append",
			"data": M{"id": "ctx", "role": "user", "source": M{"kind": "runtime-context", "form": "snapshot"}, "content": text("Current working directory: /w/proj")}},
		{"type": "session/title", "seq": 13, "time": ms(time.Second), "data": M{"title": "read the notes, then", "source": M{"kind": "fallback"}}},
		{"type": "session/title", "seq": 17, "time": ms(2 * time.Second), "data": M{"title": "Read notes and list a directory", "source": M{"kind": "provider"}}},
		// A later stopgap does not take the place of a title a model wrote
		{"type": "session/title", "seq": 18, "time": ms(2 * time.Second), "data": M{"title": "read the notes", "source": M{"kind": "fallback"}}},
		{"type": "assistant/message", "seq": 18, "time": ms(2 * time.Second), "surfaceOp": "append", "data": M{
			"message": M{"id": "a1", "role": "assistant", "source": M{"kind": "model", "provider": "deepseek-official", "model": "deepseek-flash"},
				"content": []M{{"type": "reasoning", "text": ""}, {"type": "text", "text": "I'll read the file and run the command."},
					{"type": "tool-call", "id": "call_read", "name": "read", "arguments": `{"file_path": "notes.txt"}`},
					{"type": "tool-call", "id": "call_ls", "name": "bash", "arguments": `{"command": "ls /missing"}`}}},
			"usage": M{"inputTokens": 5596, "outputTokens": 103, "cacheReadTokens": 0, "cacheWriteTokens": 0, "totalTokens": 5699}}},
		{"type": "tool/call", "seq": 19, "time": ms(2 * time.Second), "data": M{"callId": "call_read", "name": "read", "arguments": `{"file_path": "notes.txt"}`}},
		{"type": "tool/result", "seq": 20, "time": ms(2 * time.Second), "surfaceOp": "append", "data": M{
			"message": M{"id": "r1", "role": "tool", "toolCallId": "call_read", "isError": false, "content": text("the deploy runs at noon")}}},
		{"type": "job/registered", "seq": 22, "time": ms(2 * time.Second), "data": M{"jobId": "bash-1", "kind": "bash", "toolCall": M{"callId": "call_ls"}, "startedAt": ms(2 * time.Second)}},
		{"type": "job/settled", "seq": 23, "time": ms(2*time.Second + 13*time.Millisecond), "data": M{"jobId": "bash-1", "kind": "bash", "toolCall": M{"callId": "call_ls"},
			"startedAt": ms(2 * time.Second), "finishedAt": ms(2*time.Second + 13*time.Millisecond), "status": "completed", "detail": "exit code: 1"}},
		// isError is false: as far as the harness is concerned the tool ran
		{"type": "tool/result", "seq": 24, "time": ms(2*time.Second + 14*time.Millisecond), "surfaceOp": "append", "data": M{
			"message": M{"id": "r2", "role": "tool", "toolCallId": "call_ls", "isError": false, "content": text("[stderr]\nls: /missing: No such file or directory\n[exit code: 1]")}}},
		{"type": "assistant/message", "seq": 28, "time": ms(3 * time.Second), "surfaceOp": "append", "data": M{
			"message": M{"id": "a2", "role": "assistant", "source": M{"kind": "model", "model": "deepseek-flash"}, "content": text("The notes say the deploy runs at noon; the directory is not there.")},
			"usage":   M{"inputTokens": 164, "outputTokens": 66, "cacheReadTokens": 5632, "cacheWriteTokens": 0, "totalTokens": 5862}}},
		// A compaction's rewrite of what the model sees: not a row anyone was shown
		{"type": "user/message", "seq": 40, "time": ms(4 * time.Second), "surfaceOp": "replace",
			"data": M{"id": "u-rewrite", "role": "user", "source": M{"kind": "user"}, "content": text("a rewritten history that opens no turn")}},
		{"type": "turn/end", "seq": 30, "time": ms(3 * time.Second), "data": M{"turn": 1, "reason": M{"kind": "completed"}}},
	}
}

func TestWhatTheCurrentHarnessWrites(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "project", "session-real", "session.v4.jsonl"), encode(t, currentRows()))
	s := sourcetest.One(t, Spec.Open(root))
	// every row it writes is one the adapter knows, the ones it passes over included
	sourcetest.NothingUnread(t, s)

	if s.Session.Title != "Read notes and list a directory" || s.Session.ID != "session-real" || s.Session.Cwd != "/w/proj" {
		t.Errorf("session = %+v", s.Session)
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d: the rewrite of a compaction opens none", len(s.Turns))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt context reply tool tool reply" {
		t.Errorf("items = %s", got)
	}
	tools := sourcetest.Tools(turn)
	if read := tools[0]; read.Name != "read" || read.Status != model.StatusOK || read.Exit != nil {
		t.Errorf("read = %+v", read)
	}
	ls := tools[1]
	if ls.Name != "bash" || ls.Command != "ls /missing" || ls.Status != model.StatusError || ls.Exit == nil || *ls.Exit != 1 || ls.DurMs != 13 {
		t.Errorf("the command exited 1, and only its job says so: %+v", ls)
	}
	want := model.Usage{Input: 5760, Output: 169, CacheRead: 5632}
	if s.Stats.Usage != want || s.Stats.Calls != 2 || turn.Failures != 1 {
		t.Errorf("usage = %+v in %d calls, %d failed; want %+v in 2, 1 failed", s.Stats.Usage, s.Stats.Calls, turn.Failures, want)
	}
	// The turn's end is written, and the next move is the person's
	if live := s.Session.Live; live == nil || live.State != model.LiveIdle || !live.Since.Equal(start.Add(3*time.Second)) {
		t.Errorf("live = %+v, want idle since the turn's end", live)
	}
}

// ev is a row the harness writes of its own work: everything it writes past the header has
// a type, a seq, a time and data.
func ev(typ string, seq int, at time.Duration, data M) M {
	return M{"type": typ, "seq": seq, "time": ms(at), "data": data}
}

// promptID is the id the harness gave the prompt of the recorded sessions: the inbox's
// splice puts it in, and the user/message after it writes it again.
const promptID = "6f1c2a9e-3b7d-4e21-9a58-0c4d2e7b1f63"

const recordedAsk = "what does the deploy script do?"

// opening is how both sessions recorded on the harness begin — their rows in order, in the
// shapes they were written, with values of the test's own — up to where the one went on to
// an answer and the other to a failure: the header, the settings the session
// runs with, the prompt spliced in from the inbox and the inbox emptied, the turn's and its
// first step's start, the system prompt, the prompt written again as a user/message with
// the runtime context after it, the request made, and the title the harness gives the
// session while a model writes a better one.
func opening() []M {
	prompt := text(recordedAsk)
	return []M{
		{"type": "session", "version": 4, "id": "session-recorded", "cwd": "/w/proj", "createdAt": ms(0)},
		ev("permission/preset", 1, 0, M{"preset": "workspace-write"}),
		ev("sandbox/mode", 2, 0, M{"mode": "workspace-write"}),
		ev("approval/policy", 3, 0, M{"policy": "ask"}),
		ev("agent/inbox/spliced", 4, 10*time.Second, M{"target": "next-turn", "start": 0,
			"inserted": []M{{"id": promptID, "role": "user", "source": M{"kind": "user"}, "content": prompt}}}),
		ev("turn/start", 5, 10*time.Second, M{"turn": 1}),
		ev("agent/inbox/spliced", 6, 10*time.Second, M{"target": "next-turn", "start": 0, "removedCount": 1, "inserted": []M{}}),
		ev("step/start", 7, 10*time.Second, M{"turn": 1, "step": 1}),
		{"type": "system/message", "seq": 8, "time": ms(10 * time.Second), "surfaceOp": "append",
			"data": M{"message": M{"role": "system", "content": text("the system prompt"), "source": M{"kind": "system-prompt"}, "id": "sys"}}},
		{"type": "user/message", "seq": 9, "time": ms(10 * time.Second), "surfaceOp": "append",
			"data": M{"id": promptID, "role": "user", "source": M{"kind": "user"}, "content": prompt}},
		{"type": "user/message", "seq": 10, "time": ms(10 * time.Second), "surfaceOp": "append",
			"data": M{"id": "ctx", "role": "user", "source": M{"kind": "runtime-context", "form": "snapshot"}, "content": text("Current working directory: /w/proj")}},
		ev("request/header", 11, 10*time.Second, M{"reason": "initial", "header": M{
			"config":          M{"provider": "deepseek-official", "model": "deepseek-flash", "maxTokens": 32768, "reasoningEffort": "high"},
			"adapterDefaults": M{"reasoningEffort": false, "maxTokens": false},
			"tools":           []M{{"name": "read", "description": "Read a file.", "parameters": M{"type": "object"}}}}}),
		ev("request/context", 12, 10*time.Second, M{"provider": "deepseek-official", "model": "deepseek-flash", "contextWindow": 1000000}),
		ev("session/title", 13, 10*time.Second, M{"title": "what does the deploy", "source": M{"kind": "fallback"}}),
		ev("session/title-llm-request", 14, 10*time.Second, M{"titleProvider": "deepseek-official", "messageSeqs": []int{9},
			"route": M{"provider": "deepseek-official", "model": "deepseek-flash"}, "system": "Give the conversation a short title.",
			"messages": []M{{"role": "user", "content": prompt}}, "maxTokens": 64, "reasoningEffort": "low"}),
	}
}

// completedRows is the session recorded on the harness whose turn was answered.
func completedRows() []M {
	return append(opening(),
		ev("session-log-deepseek/delivery-accepted", 15, 11*time.Second, M{"sessionId": "session-recorded", "sessionFormatVersion": 4, "throughSeq": 14}),
		ev("session/title", 16, 12*time.Second, M{"title": "What the deploy script does", "source": M{"kind": "provider"}}),
		ev("session-log-deepseek/delivery-accepted", 17, 12*time.Second, M{"sessionId": "session-recorded", "sessionFormatVersion": 4, "throughSeq": 16}),
		M{"type": "assistant/message", "seq": 18, "time": ms(16 * time.Second), "surfaceOp": "append", "data": M{
			"message": M{"id": "a1", "role": "assistant", "source": M{"kind": "model", "provider": "deepseek-official", "model": "deepseek-flash"},
				"content": text("It builds the image and pushes it.")},
			"usage": M{"inputTokens": 900, "outputTokens": 40, "cacheReadTokens": 0, "cacheWriteTokens": 0, "totalTokens": 940}}},
		ev("step/end", 19, 17*time.Second, M{"turn": 1, "step": 1}),
		ev("turn/end", 20, 17*time.Second, M{"turn": 1, "reason": M{"kind": "completed"}}),
	)
}

// The failure both of the failed session's rows carry
const (
	failureMessage = "402 Insufficient Balance: the account has no credit left for this request."
	failureCode    = "insufficient_balance"
	failureText    = failureMessage + " (" + failureCode + ")"
)

func failureData() M { return M{"message": failureMessage, "code": failureCode} }

// failedAttempt is the failed session's call to the model, which came to its failure
// instead of a message.
func failedAttempt() M {
	return ev("assistant/attempt", 15, 13*time.Second, M{"turn": 1, "step": 1, "stream": []M{
		{"type": "chunk", "time": ms(13 * time.Second), "chunk": M{"type": "finish", "reason": M{"kind": "error", "failure": failureData()}}}}})
}

// failedRows is the session recorded on the harness whose turn failed: its one call to the
// model came to an attempt that failed instead of a message, and the turn ended in the
// same error.
func failedRows() []M {
	return append(opening(),
		failedAttempt(),
		ev("step/end", 16, 13*time.Second, M{"turn": 1, "step": 1}),
		ev("turn/end", 17, 13*time.Second, M{"turn": 1, "reason": M{"kind": "error", "error": failureData()}}),
	)
}

// events is a session's events of a kind, every turn's, in order.
func events(s sourcetest.Session, kind string) []model.Item {
	var out []model.Item
	for _, turn := range s.Turns {
		for _, it := range turn.Items {
			if it.Kind == model.Event && it.Event == kind {
				out = append(out, it)
			}
		}
	}
	return out
}

func prompts(s sourcetest.Session) int {
	n := 0
	for _, turn := range s.Turns {
		for _, it := range turn.Items {
			if it.Kind == model.Prompt {
				n++
			}
		}
	}
	return n
}

func readRows(t *testing.T, rows []M) sourcetest.Session {
	t.Helper()
	root := t.TempDir()
	put(t, filepath.Join(root, "project", "session-recorded", "session.v4.jsonl.zstd"), frames(t, encode(t, rows), 300))
	return sourcetest.One(t, Spec.Open(root))
}

// TestACompletedTurnAsTheHarnessRecordsIt: of every row the harness writes, none is left
// not understood; its settings, and the step its turn took, are events; the prompt the
// inbox spliced in is read once; and the turn's end says it is over.
func TestACompletedTurnAsTheHarnessRecordsIt(t *testing.T) {
	s := readRows(t, completedRows())
	sourcetest.NothingUnread(t, s)
	sourcetest.Invariants(t, s)
	if s.Session.Title != "What the deploy script does" || s.Session.Cwd != "/w/proj" || s.Stats.Turns != 1 {
		t.Fatalf("session = %+v, %d turns", s.Session, s.Stats.Turns)
	}
	if n := prompts(s); n != 1 {
		t.Errorf("%d prompts: the splice that put the prompt in is not the prompt a second time", n)
	}

	// The settings come before the first ask, as one event
	zero := s.Turn(t, 0)
	if got := sourcetest.Kinds(zero); got != "event" {
		t.Fatalf("turn 0 = %s", got)
	}
	// They are written in every session: routine, as each step is
	if it := zero.Items[0]; it.Event != "settings" || it.Text != "Permissions workspace-write · sandbox workspace-write · approval ask" || !it.At.Equal(start) || !it.Routine {
		t.Errorf("settings = %+v", it)
	}

	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt context reply event" {
		t.Fatalf("items = %s", got)
	}
	if turn.Ask != recordedAsk || turn.Answer != "It builds the image and pushes it." || turn.Interrupted {
		t.Errorf("ask %q answer %q interrupted %v", turn.Ask, turn.Answer, turn.Interrupted)
	}
	// The step is drawn where it ended, with how long it took; it is where the turn's work
	// ended
	stepEnd := start.Add(17 * time.Second)
	if it := turn.Items[3]; it.Event != "step" || it.Text != "Step 1 · 7s" || !it.At.Equal(stepEnd) || !it.Routine {
		t.Errorf("step = %+v", it)
	}
	if !turn.EndedAt.Equal(stepEnd) {
		t.Errorf("the turn ended %s, want at the step's end", turn.EndedAt)
	}
	// The first request names the model the session began with, which is no change
	if got := events(s, model.EventModelChange); len(got) != 0 {
		t.Errorf("model changes = %+v", got)
	}
	if live := s.Session.Live; live == nil || live.State != model.LiveIdle || !live.Since.Equal(stepEnd) {
		t.Errorf("live = %+v, want idle since the turn's end", live)
	}
	if s.Stats.Usage != (model.Usage{Input: 900, Output: 40}) || s.Stats.Calls != 1 {
		t.Errorf("usage = %+v in %d calls", s.Stats.Usage, s.Stats.Calls)
	}
}

// TestAFailedTurnAsTheHarnessRecordsIt: the call that failed is an error event, saying what
// failed; the turn's end, which says the same again, adds no second one; and the turn is
// over, failed, which no CLI's error but Gemini CLI's marks as interrupted.
func TestAFailedTurnAsTheHarnessRecordsIt(t *testing.T) {
	s := readRows(t, failedRows())
	sourcetest.NothingUnread(t, s)
	sourcetest.Invariants(t, s)
	if s.Stats.Turns != 1 || prompts(s) != 1 {
		t.Fatalf("%d turns, %d prompts", s.Stats.Turns, prompts(s))
	}
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt context event event" {
		t.Fatalf("items = %s", got)
	}
	failed := start.Add(13 * time.Second)
	if it := turn.Items[2]; it.Event != "error" || it.Text != failureText || !it.At.Equal(failed) || it.Routine {
		t.Errorf("error = %+v", it)
	}
	if it := turn.Items[3]; it.Event != "step" || it.Text != "Step 1 · 3s" {
		t.Errorf("step = %+v", it)
	}
	if got := events(s, "error"); len(got) != 1 {
		t.Errorf("errors = %+v: the turn's end says what the attempt said", got)
	}
	if turn.Answer != "" || turn.Interrupted || turn.Failures != 0 || s.Stats.Calls != 0 {
		t.Errorf("answer %q interrupted %v failures %d calls %d", turn.Answer, turn.Interrupted, turn.Failures, s.Stats.Calls)
	}
	if live := s.Session.Live; live == nil || live.State != model.LiveIdle || !live.Since.Equal(failed) {
		t.Errorf("live = %+v, want idle since the turn's end", live)
	}
}

// TestAFailedCallTriedAgain: a call to the model fails — a provider's passing error — and
// the harness makes it again, which is answered. The failure is an error event where it
// came, and only that: the turn goes on to its answer and ends as one that did, neither
// failed nor interrupted, and the error is not where the turn stopped.
func TestAFailedCallTriedAgain(t *testing.T) {
	rows := append(opening(),
		failedAttempt(),
		M{"type": "assistant/message", "seq": 16, "time": ms(16 * time.Second), "surfaceOp": "append", "data": M{
			"message": M{"id": "a1", "role": "assistant", "source": M{"kind": "model", "provider": "deepseek-official", "model": "deepseek-flash"},
				"content": text("It builds the image and pushes it.")},
			"usage": M{"inputTokens": 900, "outputTokens": 40}}},
		ev("step/end", 17, 17*time.Second, M{"turn": 1, "step": 1}),
		ev("turn/end", 18, 17*time.Second, M{"turn": 1, "reason": M{"kind": "completed"}}),
	)
	s := readRows(t, rows)
	sourcetest.NothingUnread(t, s)
	sourcetest.Invariants(t, s)
	turn := s.Turn(t, 1)
	if got := sourcetest.Kinds(turn); got != "prompt context event reply event" {
		t.Fatalf("items = %s", got)
	}
	if it := turn.Items[2]; it.Event != "error" || it.Text != failureText || !it.At.Equal(start.Add(13*time.Second)) {
		t.Errorf("error = %+v", it)
	}
	if got := events(s, "error"); len(got) != 1 {
		t.Errorf("errors = %+v: the turn that ended completed adds none", got)
	}
	if it := turn.Items[4]; it.Event != "step" || it.Text != "Step 1 · 7s" {
		t.Errorf("step = %+v", it)
	}
	ended := start.Add(17 * time.Second)
	if turn.Answer != "It builds the image and pushes it." || turn.Interrupted || turn.Failures != 0 || s.Stats.Calls != 1 {
		t.Errorf("answer %q interrupted %v failures %d calls %d", turn.Answer, turn.Interrupted, turn.Failures, s.Stats.Calls)
	}
	if !turn.EndedAt.Equal(ended) {
		t.Errorf("the turn ended %s, want at the step's end", turn.EndedAt)
	}
	if live := s.Session.Live; live == nil || live.State != model.LiveIdle || !live.Since.Equal(ended) {
		t.Errorf("live = %+v, want idle since the turn's end", live)
	}
}

// TestHarnessRowsOfALaterTurn: the person changes a setting and the model between turns.
// The change of setting is an event that names it, at the end of the turn before, which
// it does not stretch; settings written again as they were draw nothing; the model is a
// model change in the turn whose request went to it, and a request to the same model is
// none; and each step of a turn that called a tool is an event of its own.
func TestHarnessRowsOfALaterTurn(t *testing.T) {
	second := func(at time.Duration) time.Duration { return 30*time.Second + at }
	rows := append(completedRows(),
		ev("permission/preset", 21, second(0), M{"preset": "workspace-write"}),
		ev("sandbox/mode", 22, second(0), M{"mode": "read-only", "note": "a field the adapter does not read"}),
		ev("approval/policy", 23, second(0), M{"policy": "ask"}),
		ev("turn/start", 24, second(5*time.Second), M{"turn": 2}),
		ev("step/start", 25, second(5*time.Second), M{"turn": 2, "step": 1}),
		userRow("u2", second(5*time.Second), text("now run it")),
		ev("request/context", 27, second(5*time.Second), M{"provider": "deepseek-official", "model": "deepseek-pro", "contextWindow": 1000000, "systemPromptUpdate": "the system prompt, updated"}),
		M{"type": "assistant/message", "seq": 28, "time": ms(second(6 * time.Second)), "data": M{
			"message": M{"id": "a2", "source": M{"kind": "model", "model": "deepseek-pro"},
				"content": []M{{"type": "tool-call", "id": "c1", "name": "bash", "arguments": `{"command": "./deploy.sh"}`}}},
			"usage": M{"inputTokens": 10, "outputTokens": 5}}},
		resultRow("r1", "c1", second(8*time.Second), false, text("pushed")),
		ev("step/end", 30, second(8*time.Second), M{"turn": 2, "step": 1}),
		ev("step/start", 31, second(8*time.Second), M{"turn": 2, "step": 2}),
		ev("request/context", 32, second(8*time.Second), M{"provider": "deepseek-official", "model": "deepseek-pro", "contextWindow": 1000000}),
		M{"type": "assistant/message", "seq": 33, "time": ms(second(9 * time.Second)), "data": M{
			"message": M{"id": "a3", "source": M{"kind": "model", "model": "deepseek-pro"}, "content": text("Deployed.")},
			"usage":   M{"inputTokens": 20, "outputTokens": 3}}},
		ev("step/end", 34, second(70*time.Second), M{"turn": 2, "step": 2}),
		// The settings again, as they stand: nothing changed, and nothing is drawn
		ev("sandbox/mode", 35, second(70*time.Second), M{"mode": "read-only"}),
		ev("approval/policy", 36, second(70*time.Second), M{"policy": "ask"}),
		ev("turn/end", 37, second(70*time.Second), M{"turn": 2, "reason": M{"kind": "completed"}}),
	)
	s := readRows(t, rows)
	sourcetest.NothingUnread(t, s)
	sourcetest.Invariants(t, s)

	first := s.Turn(t, 1)
	if got := sourcetest.Kinds(first); got != "prompt context reply event event" {
		t.Fatalf("turn 1 = %s", got)
	}
	// A change of setting is no matter of course, as the settings a session opens with are
	if it := first.Items[4]; it.Event != "settings" || it.Text != "Sandbox workspace-write → read-only" || it.Routine {
		t.Errorf("the change = %+v", it)
	}
	if !first.EndedAt.Equal(start.Add(17 * time.Second)) {
		t.Errorf("turn 1 ended %s: a setting the person changed after it is not its work", first.EndedAt)
	}
	if got := events(s, "settings"); len(got) != 2 {
		t.Errorf("settings = %+v: the ones written again as they were draw nothing", got)
	}

	turn := s.Turn(t, 2)
	if got := sourcetest.Kinds(turn); got != "prompt event tool event reply event" {
		t.Fatalf("turn 2 = %s", got)
	}
	if it := turn.Items[1]; it.Event != model.EventModelChange || it.Text != "deepseek-official/deepseek-pro" || it.Routine {
		t.Errorf("model change = %+v", it)
	}
	if got := events(s, model.EventModelChange); len(got) != 1 {
		t.Errorf("model changes = %+v", got)
	}
	steps := events(s, "step")
	if len(steps) != 3 || steps[1].Text != "Step 1 · 3s" || steps[2].Text != "Step 2 · 1m02s" {
		t.Errorf("steps = %+v", steps)
	}
	if live := s.Session.Live; live == nil || live.State != model.LiveIdle {
		t.Errorf("live = %+v", live)
	}
}

// TestATurnUnderWay: a session read while the harness writes it. A step not yet ended draws
// nothing — how long it takes is not known, and an event drawn now would be gone at the
// next read, its place taken by the step's end — and the turn, its end not written, is in
// flight: at work, and then waiting on the call it made.
func TestATurnUnderWay(t *testing.T) {
	s := readRows(t, opening())
	sourcetest.NothingUnread(t, s)
	if got := events(s, "step"); len(got) != 0 {
		t.Errorf("steps = %+v: the step has not ended", got)
	}
	if live := s.Session.Live; live == nil || live.State != model.LiveWorking || !live.Since.Equal(start.Add(10*time.Second)) {
		t.Errorf("live = %+v, want working since the ask", live)
	}

	rows := append(opening(), M{"type": "assistant/message", "seq": 15, "time": ms(12 * time.Second), "data": M{
		"message": M{"id": "a1", "source": M{"kind": "model", "model": "deepseek-flash"},
			"content": []M{{"type": "text", "text": "I'll read it."}, {"type": "tool-call", "id": "c1", "name": "read", "arguments": `{"file_path": "deploy.sh"}`}}},
		"usage": M{"inputTokens": 10, "outputTokens": 5}}})
	s = readRows(t, rows)
	if live := s.Session.Live; live == nil || live.State != model.LiveWaiting || live.Tool != "read" {
		t.Errorf("live = %+v, want waiting on the read", live)
	}

	// A session of nothing but its header and the settings it opens with is one opened and
	// left before anything was asked: it holds nothing, as such a session of any CLI does,
	// and is listed with them
	s = readRows(t, opening()[:4])
	sourcetest.NothingUnread(t, s)
	if s.Stats.Items != 0 || s.Stats.Turns != 0 {
		t.Errorf("%d items in %d turns: the settings alone are no conversation", s.Stats.Items, s.Stats.Turns)
	}
	// Once it goes on, its settings are drawn, at the place nothing took before
	s = readRows(t, opening()[:5])
	if got := events(s, "settings"); len(got) != 1 || got[0].Seq != 0 || got[0].Text != "Permissions workspace-write · sandbox workspace-write · approval ask" {
		t.Errorf("settings = %+v", got)
	}
}

// TestHowATurnEnds: what a turn's end says beside its being over.
func TestHowATurnEnds(t *testing.T) {
	attempt := func(seq int, kind, message string) M {
		return ev("assistant/attempt", seq, 13*time.Second, M{"turn": 1, "step": 1, "stream": []M{
			{"type": "chunk", "time": ms(12 * time.Second), "chunk": M{"type": "text-delta", "text": "partial"}},
			{"type": "chunk", "time": ms(13 * time.Second), "chunk": M{"type": "finish", "reason": M{"kind": kind, "failure": M{"message": message}}}}}})
	}
	end := func(reason M) M { return ev("turn/end", 17, 14*time.Second, M{"turn": 1, "reason": reason}) }
	cases := []struct {
		name        string
		rows        []M
		errors      []string // the error events' texts
		interrupted bool
		unread      map[string]int
	}{
		{name: "an error no attempt said", rows: []M{end(M{"kind": "error", "error": M{"message": "the tool runner crashed"}})},
			errors: []string{"the tool runner crashed"}},
		{name: "an error other than the attempt's", rows: []M{attempt(15, "error", "timed out"), end(M{"kind": "error", "error": failureData()})},
			errors: []string{"timed out", failureText}},
		{name: "an error that says nothing", rows: []M{attempt(15, "error", ""), end(M{"kind": "error"})},
			errors: []string{"The call to the model failed", "The turn failed"}},
		{name: "an attempt that did not fail", rows: []M{attempt(15, "stop", ""), end(M{"kind": "completed"})}},
		{name: "stopped", rows: []M{end(M{"kind": "aborted"})}, interrupted: true},
		{name: "a reason of a kind not known", rows: []M{end(M{"kind": "max-steps"})}, unread: map[string]int{"reason:max-steps": 1}},
		{name: "no reason", rows: []M{ev("turn/end", 17, 14*time.Second, M{"turn": 1})}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := readRows(t, append(opening(), c.rows...))
			if !maps.Equal(s.Unread, c.unread) {
				t.Errorf("unread = %v, want %v", s.Unread, c.unread)
			}
			var got []string
			for _, it := range events(s, "error") {
				got = append(got, it.Text)
			}
			if !slices.Equal(got, c.errors) {
				t.Errorf("errors = %q, want %q", got, c.errors)
			}
			turn := s.Turn(t, 1)
			if turn.Interrupted != c.interrupted || (len(events(s, model.EventInterrupted)) > 0) != c.interrupted {
				t.Errorf("interrupted %v, events %+v", turn.Interrupted, events(s, model.EventInterrupted))
			}
			// However it ended, it is over
			if live := s.Session.Live; live == nil || live.State != model.LiveIdle {
				t.Errorf("live = %+v", live)
			}
		})
	}
}

// TestATranscriptWithoutTurnBounds: a transcript that does not bound its turns says nothing
// of where the latest stands that has been checked, and no state is made up for it.
func TestATranscriptWithoutTurnBounds(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl"), encode(t, transcriptRows()))
	if s := sourcetest.One(t, Spec.Open(root)); s.Session.Live != nil {
		t.Errorf("live = %+v", s.Session.Live)
	}
}

// A response with no words — the model answered with nothing, or only its reasoning was
// kept elsewhere — still cost what it cost.
func TestAResponseWithNoContentStillCounts(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl"), encode(t, []M{
		headerRow("dsh-empty", "/w/proj"),
		userRow("u1", 0, text("think about it")),
		assistantRow("a1", time.Second, M{"inputTokens": 900, "outputTokens": 70}),
		assistantRow("a2", 2*time.Second, M{"inputTokens": 10, "outputTokens": 5}, M{"type": "text", "text": "done"}),
	}))
	s := sourcetest.One(t, Spec.Open(root))
	if want := (model.Usage{Input: 910, Output: 75}); s.Stats.Usage != want || s.Stats.Calls != 2 {
		t.Errorf("usage = %+v in %d calls, want %+v in 2", s.Stats.Usage, s.Stats.Calls, want)
	}
	sourcetest.NothingUnread(t, s)
}

// TestUnknownKindsAreCounted: a row, or a part of a message, of a type the harness had not
// written when the adapter was made is passed over and counted under its name — as is a
// row that does not say what it is — and the rest of the session reads as it did.
func TestUnknownKindsAreCounted(t *testing.T) {
	root := t.TempDir()
	rows := transcriptRows()
	image := M{"type": "image", "mediaType": "image/png", "data": "iVBORw0KGgo="}
	// parts of kinds it has never seen, beside the ones it knows: in the ask, in the
	// result, where a part with no text is all there is to pass over, and in the answer
	ask, result, answer := rows[1]["data"].(M), rows[3]["data"].(M)["message"].(M), rows[4]["data"].(M)["message"].(M)
	ask["content"] = []M{{"type": "text", "text": "fix the uploader"}, image}
	result["content"] = []M{{"type": "text", "text": "package x"}, image}
	answer["content"] = []M{{"type": "redacted-reasoning", "data": "opaque"}, {"type": "text", "text": "done"}}
	// The repeat of a message and a compaction's rewrite are passed over knowingly, and
	// what they hold is not looked at, let alone counted
	repeat, rewrite := rows[5]["data"].(M)["message"].(M), rows[6]["data"].(M)
	repeat["content"] = []M{{"type": "text", "text": "dup"}, image}
	rewrite["content"] = []M{{"type": "text", "text": "compacted summary"}, image}
	// rows of kinds it has never seen, between the ask and what answers it
	rows = slices.Insert(rows, 2,
		M{"type": "session/fork", "time": ms(500 * time.Millisecond), "data": M{"from": "dsh-0"}},
		M{"time": ms(600 * time.Millisecond), "data": M{"note": "a row with no type"}})
	put(t, filepath.Join(sessionDir(root), "session.v4.jsonl"), encode(t, rows))

	s := sourcetest.One(t, Spec.Open(root))
	want := map[string]int{"row:session/fork": 1, "row:(untyped)": 1, "part:image": 2, "part:redacted-reasoning": 1}
	if !maps.Equal(s.Unread, want) {
		t.Errorf("unread = %v, want %v", s.Unread, want)
	}
	checkSession(t, s)
}

// TestCountsThatAreNoCounts: a token count below zero, or past anything one response could
// use (model.MaxTokens), is no count and is read as none — not as the most a count can be.
// Taken as it is, the one would take a session's total below zero, and two of the other
// would add up past what the total can hold and wrap below it.
func TestCountsThatAreNoCounts(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(sessionDir(root), "session.v1.jsonl"), encode(t, []M{
		headerRow("dsh-counts", "/w"),
		userRow("u1", 0, text("count")),
		assistantRow("a1", time.Second, M{"inputTokens": -5, "outputTokens": 3}, M{"type": "text", "text": "one"}),
		assistantRow("a2", 2*time.Second, M{"inputTokens": 9e18, "outputTokens": 1e300, "cacheReadTokens": 7}, M{"type": "text", "text": "two"}),
		assistantRow("a3", 3*time.Second, M{"inputTokens": 9e18, "outputTokens": 4}, M{"type": "text", "text": "three"}),
		// the bound itself is a count; one past it is not
		assistantRow("a4", 4*time.Second, M{"reasoningTokens": model.MaxTokens, "cacheWriteTokens": model.MaxTokens + 1}, M{"type": "text", "text": "four"}),
	}))
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	if want := (model.Usage{Output: 7, CacheRead: 7, Reasoning: model.MaxTokens}); s.Stats.Usage != want || s.Stats.Calls != 4 {
		t.Errorf("usage = %+v in %d calls, want %+v in 4", s.Stats.Usage, s.Stats.Calls, want)
	}
}

// TestARepeatIsCountedOnceEvenWhenTheFirstCopyAddedNothing: a message whose content is
// only a part the adapter does not know adds nothing to the transcript, and the copies of
// it written after are still the same message — the part is counted once, not once a copy.
// A later copy that does hold something is the one read, and its part is not counted again.
func TestARepeatIsCountedOnceEvenWhenTheFirstCopyAddedNothing(t *testing.T) {
	root := t.TempDir()
	image := M{"type": "image", "mediaType": "image/png", "data": "iVBORw0KGgo="}
	put(t, filepath.Join(sessionDir(root), "session.v1.jsonl"), encode(t, []M{
		headerRow("dsh-repeat", "/w"),
		userRow("u1", 0, text("look at this")),
		userRow("u2", time.Second, []M{image}),
		userRow("u2", time.Second, []M{image}),
		assistantRow("a1", 2*time.Second, nil, image),
		assistantRow("a1", 2*time.Second, nil, image),
		assistantRow("a1", 3*time.Second, M{"inputTokens": 4, "outputTokens": 2}, image, M{"type": "text", "text": "seen it"}),
		assistantRow("a1", 4*time.Second, M{"inputTokens": 40, "outputTokens": 20}, image, M{"type": "text", "text": "again"}),
	}))
	s := sourcetest.One(t, Spec.Open(root))
	if want := map[string]int{"part:image": 2}; !maps.Equal(s.Unread, want) {
		t.Errorf("unread = %v, want %v", s.Unread, want)
	}
	turn := s.Turn(t, 1)
	if turn.Answer != "seen it" || s.Stats.Calls != 1 || s.Stats.Usage != (model.Usage{Input: 4, Output: 2}) {
		t.Errorf("answer %q, usage %+v in %d calls; want \"seen it\", 4 in and 2 out in 1", turn.Answer, s.Stats.Usage, s.Stats.Calls)
	}
}

// Keys that mean one counter are read the same way however the map is walked.
func TestUsageOfKeysThatMeanOneCounter(t *testing.T) {
	raw := map[string]any{"input_tokens": 5.0, "prompt_tokens": 9.0, "output_tokens": 2.0, "completion_tokens": 1.0}
	for range 100 {
		if u := usageOf(raw); u.Input != 9 || u.Output != 2 {
			t.Fatalf("usage = %+v", u)
		}
	}
}

// A job's own times are a row's word: one that ran from the start of the calendar to its
// end ran for no duration a call can have, and none is read.
func TestTimesNoJobCouldSpan(t *testing.T) {
	rows := currentRows()
	for _, r := range rows {
		if r["type"] == "job/settled" {
			data := r["data"].(M)
			data["startedAt"], data["finishedAt"] = "0001-01-01T00:00:01Z", "9999-12-31T23:59:59Z"
		}
	}
	root := t.TempDir()
	put(t, filepath.Join(root, "project", "session-real", "session.v4.jsonl"), encode(t, rows))
	s := sourcetest.One(t, Spec.Open(root))
	sourcetest.Invariants(t, s)
	if tools := sourcetest.Tools(s.Turn(t, 1)); len(tools) != 2 || tools[1].DurMs != 0 || tools[1].Exit == nil {
		t.Errorf("the job's call = %+v", tools[1])
	}
}
