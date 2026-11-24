package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

type wireSpan struct {
	TraceID    string `json:"traceId"`
	SpanID     string `json:"spanId"`
	ParentID   string `json:"parentSpanId"`
	Name       string `json:"name"`
	Start      string `json:"startTimeUnixNano"`
	End        string `json:"endTimeUnixNano"`
	Attributes []struct {
		Key   string `json:"key"`
		Value struct {
			String *string `json:"stringValue"`
			Int    *string `json:"intValue"`
			Bool   *bool   `json:"boolValue"`
			Array  *struct {
				Values []struct {
					String string `json:"stringValue"`
				} `json:"values"`
			} `json:"arrayValue"`
		} `json:"value"`
	} `json:"attributes"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

func (s wireSpan) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key != key {
			continue
		}
		switch {
		case a.Value.String != nil:
			return *a.Value.String
		case a.Value.Int != nil:
			return *a.Value.Int
		case a.Value.Bool != nil && *a.Value.Bool:
			return "true"
		case a.Value.Array != nil:
			var parts []string
			for _, v := range a.Value.Array.Values {
				parts = append(parts, v.String)
			}
			return strings.Join(parts, ",")
		}
	}
	return ""
}

func decode(t *testing.T, bodies ...[]byte) []wireSpan {
	t.Helper()
	var out []wireSpan
	for _, body := range bodies {
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
			var req struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []wireSpan `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			}
			if err := json.Unmarshal(line, &req); err != nil {
				t.Fatalf("not an OTLP request: %v", err)
			}
			out = append(out, req.ResourceSpans[0].ScopeSpans[0].Spans...)
		}
	}
	return out
}

// draw exports the fixture as a dry run and returns the spans and the raw requests.
func draw(t *testing.T, a *app.App, level Level) ([]wireSpan, string) {
	t.Helper()
	var buf bytes.Buffer
	res, err := Export(context.Background(), a.Index, Options{Level: level, Filter: index.Filter{Stores: a.Stores()}, DryRun: &buf})
	if err != nil || res.Turns != 4 || res.Sessions != 3 {
		t.Fatalf("export = %+v, %v: want the four turns of the three transcripts", res, err)
	}
	return decode(t, buf.Bytes()), buf.String()
}

func pick(t *testing.T, spans []wireSpan, kind, name string) wireSpan {
	t.Helper()
	for _, s := range spans {
		if s.attr("langfuse.observation.type") == kind && strings.Contains(s.Name, name) {
			return s
		}
	}
	t.Fatalf("no %s span named like %q", kind, name)
	return wireSpan{}
}

func TestATurnBecomesATrace(t *testing.T) {
	a, _ := fixture.Open(t)
	spans, _ := draw(t, a, LevelAsks)

	root := pick(t, spans, "agent", "fix the flaky test")
	if root.ParentID != "" || root.attr("langfuse.session.id") != fixture.Alpha || root.attr("asq.turn") != "1" || root.attr("asq.session") != "claude/"+fixture.Alpha {
		t.Errorf("root = %+v", root)
	}
	if got := root.attr("langfuse.trace.tags"); got != "claude,project:alpha,tool-failure" {
		t.Errorf("tags = %q", got)
	}
	if root.attr("langfuse.observation.level") != "WARNING" || root.attr("gen_ai.operation.name") != "invoke_agent" || root.attr("vcs.ref.head.name") != "main" {
		t.Errorf("root attributes = %+v", root.Attributes)
	}
	if root.Start >= root.End {
		t.Errorf("the turn's span is %s → %s", root.Start, root.End)
	}

	var gens, tools []wireSpan
	for _, s := range spans {
		if s.TraceID != root.TraceID {
			continue
		}
		switch s.attr("langfuse.observation.type") {
		case "generation":
			gens = append(gens, s)
		case "tool":
			tools = append(tools, s)
		}
	}
	if len(gens) != 2 || len(tools) != 1 {
		t.Fatalf("turn 1 = %d generations, %d tools; want 2 and 1", len(gens), len(tools))
	}
	first := gens[0]
	if first.Name != "chat claude-opus-5" || first.ParentID != root.SpanID || first.attr("gen_ai.provider.name") != "anthropic" || first.attr("gen_ai.response.id") != "msg_a1" {
		t.Errorf("generation = %s %+v", first.Name, first.Attributes)
	}
	// The conventions count cached tokens inside the input; the record keeps them apart
	if first.attr("gen_ai.usage.input_tokens") != "1050" || first.attr("gen_ai.usage.output_tokens") != "40" ||
		first.attr("gen_ai.usage.cache_read.input_tokens") != "900" || first.attr("gen_ai.usage.cache_creation.input_tokens") != "50" {
		t.Errorf("usage = %+v", first.Attributes)
	}
	if first.Start != root.Start {
		t.Errorf("a turn's first response was asked for when the turn was: %s, not %s", root.Start, first.Start)
	}
	tool := tools[0]
	if tool.Name != "execute_tool Bash" || tool.ParentID != first.SpanID || tool.Status.Code != 2 || tool.attr("gen_ai.tool.call.id") != "toolu_test" || tool.attr("asq.tool.status") != "error" {
		t.Errorf("tool = %s under %s: %+v", tool.Name, tool.ParentID, tool.Attributes)
	}
	if gens[1].Start != tool.End {
		t.Errorf("the response after a call was asked for when the call's result arrived: %s, not %s", tool.End, gens[1].Start)
	}

	seen := map[string]bool{}
	for _, s := range spans {
		if seen[s.SpanID] {
			t.Errorf("span id %s used twice", s.SpanID)
		}
		seen[s.SpanID] = true
		if len(s.TraceID) != 32 || len(s.SpanID) != 16 {
			t.Errorf("ids = %q %q", s.TraceID, s.SpanID)
		}
	}
	for _, s := range spans {
		if s.ParentID != "" && !seen[s.ParentID] {
			t.Errorf("%s hangs beneath a span that was not sent", s.Name)
		}
	}
}

// A turn whose work ended on an error is a trace that failed: tagged so, at the level of an
// error, with what the error said where what was answered leaves too, and only that it
// failed where no text of the conversation does.
func TestATurnThatEndedOnAnErrorIsATraceThatFailed(t *testing.T) {
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s := &index.Session{Session: model.Session{Source: "dsh", Agent: "dsh", ID: "session-recorded", Cwd: "/work/site"}, Ref: "dsh/session-recorded"}
	turn := &model.Turn{Index: 1, StartedAt: at, EndedAt: at.Add(3 * time.Second), Ask: "deploy it", Interrupted: true,
		Error: "402 Insufficient Balance (insufficient_balance)\n  at send (client.js:42)"}
	for _, c := range []struct {
		level  Level
		status string
	}{{LevelNone, "ended on an error"}, {LevelAsks, "402 Insufficient Balance (insufficient_balance)"}} {
		root := spansOf(s, turn, place{group: s.ID}, c.level)[0]
		attrs := map[string]any{}
		for _, a := range root.Attrs {
			attrs[a.Key] = a.Value
		}
		tags, _ := attrs["langfuse.trace.tags"].([]string)
		if !root.Error || root.Status != c.status || attrs["langfuse.observation.level"] != "ERROR" || attrs["langfuse.observation.status_message"] != c.status ||
			attrs["asq.error"] != true || strings.Join(tags, ",") != "dsh,project:site,error,interrupted" {
			t.Errorf("level %d: error %v, status %q, attributes %+v", c.level, root.Error, root.Status, attrs)
		}
	}
}

func TestASubagentHangsBeneathTheCallThatStartedIt(t *testing.T) {
	a, _ := fixture.Open(t)
	spans, _ := draw(t, a, LevelAsks)
	turn2 := pick(t, spans, "agent", "现在给数据库加索引")
	call := pick(t, spans, "tool", "execute_tool Agent")
	sub := pick(t, spans, "agent", "check the other tables")
	if sub.TraceID != turn2.TraceID || sub.ParentID != call.SpanID {
		t.Errorf("the subagent's turn is in trace %s beneath %s; want %s beneath %s", sub.TraceID, sub.ParentID, turn2.TraceID, call.SpanID)
	}
	if sub.attr("langfuse.session.id") != fixture.Alpha || sub.attr("gen_ai.agent.name") != "Explore" || !strings.Contains(sub.attr("langfuse.trace.tags"), "subagent") {
		t.Errorf("subagent = %+v", sub.Attributes)
	}
	gen := pick(t, spans, "generation", "claude-haiku-5")
	if gen.TraceID != turn2.TraceID || gen.ParentID != sub.SpanID {
		t.Error("the subagent's own response is not beneath it")
	}
}

func TestHowMuchLeavesTheMachine(t *testing.T) {
	a, _ := fixture.Open(t)
	_, none := draw(t, a, LevelNone)
	_, asks := draw(t, a, LevelAsks)
	_, all := draw(t, a, LevelAll)

	for _, private := range []string{"flaky", "数据库", "map order", "ECONNREFUSED", "go test", "schema.sql", "/work/alpha", "rotate", "Fix flaky test"} {
		if strings.Contains(none, private) {
			t.Errorf("with no content asked for, the export carries %q", private)
		}
	}
	for _, kept := range []string{"claude-opus-5", "Bash", "toolu_test", "project:alpha", "claude turn 1"} {
		if !strings.Contains(none, kept) {
			t.Errorf("the metadata lost %q", kept)
		}
	}
	for _, said := range []string{"fix the flaky test", "map order", "/work/alpha"} {
		if !strings.Contains(asks, said) {
			t.Errorf("asks: no %q", said)
		}
	}
	for _, between := range []string{"ECONNREFUSED", "go test ./...", "the order of a map", "schema.sql"} {
		if strings.Contains(asks, between) {
			t.Errorf("asks carries what happened in between: %q", between)
		}
		if !strings.Contains(all, between) {
			t.Errorf("all: no %q", between)
		}
	}
	for name, body := range map[string]string{"none": none, "asks": asks, "all": all} {
		if strings.Contains(body, fixture.Secret) {
			t.Errorf("%s sends the key a transcript held", name)
		}
	}
	if !strings.Contains(all, "[redacted]") {
		t.Error("the place the key was is not marked")
	}
	// A body is held to a size, keeping its end
	for _, s := range decode(t, []byte(all)) {
		if s.Name == "execute_tool Bash" && strings.Contains(s.attr("langfuse.observation.input"), "make build") {
			if out := s.attr("langfuse.observation.output"); !strings.HasSuffix(out, "build finished") || len(out) > maxBody+200 {
				t.Errorf("the long output: %d bytes, ends %q", len(out), out[max(0, len(out)-20):])
			}
		}
	}
	// The same turn is the same spans, every time
	if _, again := draw(t, a, LevelAll); again != all {
		t.Error("two exports of the same history differ: ids must come from what a span is")
	}
}

// collector stands in for an OTLP endpoint.
type collector struct {
	mu     sync.Mutex
	bodies [][]byte
	auth   string
	fail   int // answer this many requests with a 503 first
	*httptest.Server
}

func newCollector(t *testing.T) *collector {
	c := &collector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "not here", http.StatusNotFound)
			return
		}
		if c.fail > 0 {
			c.fail--
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		c.auth = r.Header.Get("Authorization")
		c.bodies = append(c.bodies, body)
		w.Write([]byte("{}"))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *collector) turns(t *testing.T) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range decode(t, c.bodies...) {
		if s.attr("langfuse.observation.type") == "agent" {
			n++
		}
	}
	return n
}

func TestATurnIsSentOnce(t *testing.T) {
	h := fixture.Write(t)
	cfg := h.Config(t)
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	c := newCollector(t)
	opts := Options{Endpoint: c.URL, Headers: map[string]string{"Authorization": "Basic abc"}, Level: LevelAsks,
		Filter: index.Filter{Stores: a.Stores()}, Ledger: filepath.Join(t.TempDir(), "state", "ledger.db")}
	export := func(a *app.App, o Options) Result {
		t.Helper()
		res, err := Export(ctx, a.Index, o)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := export(a, opts); res.Turns != 4 || res.Current != 0 || c.turns(t) != 4 || c.auth != "Basic abc" {
		t.Fatalf("first export = %+v, the collector has %d turns", res, c.turns(t))
	}
	if res := export(a, opts); res.Turns != 0 || res.Current != 4 || res.Requests != 0 {
		t.Errorf("second export = %+v: nothing changed, nothing to send", res)
	}

	// A session grows: its new turn, answered, is sent, and the turns that did not change are
	// not (one still being answered would wait: see TestATurnUnderWayWaits)
	path := filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","uuid":"b-u9","timestamp":"` + h.Now.Format(time.RFC3339) + `","sessionId":"` + fixture.Beta + `","cwd":"/work/beta","message":{"role":"user","content":"and tell the team"}}` + "\n")
	f.WriteString(`{"type":"assistant","uuid":"b-a9","timestamp":"` + h.Now.Format(time.RFC3339) + `","sessionId":"` + fixture.Beta + `","cwd":"/work/beta","message":{"id":"msg_b9","role":"assistant","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"Told them."}],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n")
	f.Close()
	if _, err := a.Sync(ctx, index.SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if res := export(a, opts); res.Turns != 1 || res.Current != 4 {
		t.Errorf("after a session grew = %+v, want its one new turn", res)
	}

	// The index is a cache; throwing it away must not send everything again
	a.Close()
	if err := index.Remove(cfg.IndexPath); err != nil {
		t.Fatal(err)
	}
	b, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if res := export(b, opts); res.Turns != 0 || res.Current != 5 {
		t.Errorf("after the index was rebuilt = %+v: the ledger is not part of the cache", res)
	}

	// Asked to, or asked for more of the conversation, it sends again
	again := opts
	again.Resend = true
	if res := export(b, again); res.Turns != 5 {
		t.Errorf("resend = %+v", res)
	}
	more := opts
	more.Level = LevelAll
	if res := export(b, more); res.Turns != 5 {
		t.Errorf("a different level = %+v: what the collector has is not what was asked for now", res)
	}
	// Another collector has been sent nothing
	other := newCollector(t)
	elsewhere := opts
	elsewhere.Endpoint = other.URL + "/v1/traces"
	if res := export(b, elsewhere); res.Turns != 5 || other.turns(t) != 5 {
		t.Errorf("a second collector = %+v", res)
	}
}

func TestACollectorThatIsDown(t *testing.T) {
	a, _ := fixture.Open(t)
	c := newCollector(t)
	opts := Options{Endpoint: c.URL, Filter: index.Filter{Stores: a.Stores()}, Ledger: filepath.Join(t.TempDir(), "ledger.db")}

	c.fail = 1 // one bad answer is tried again
	if res, err := Export(context.Background(), a.Index, opts); err != nil || res.Turns != 4 {
		t.Fatalf("a collector that hiccuped = %+v, %v", res, err)
	}

	c.fail = 100
	opts.Resend = true
	res, err := Export(context.Background(), a.Index, opts)
	if err == nil || res.Turns != 0 {
		t.Fatalf("a collector that is down = %+v, %v", res, err)
	}
	// What was not taken is not written down as sent
	c.mu.Lock()
	c.fail = 0
	c.mu.Unlock()
	opts.Resend = false
	opts.Level = LevelAsks // a level nothing was recorded at
	if res, err := Export(context.Background(), a.Index, opts); err != nil || res.Turns != 4 {
		t.Errorf("once it is back = %+v, %v", res, err)
	}

	gone := opts
	gone.Endpoint = "http://127.0.0.1:1"
	gone.Client = &http.Client{Timeout: time.Second}
	if _, err := Export(context.Background(), a.Index, gone); err == nil {
		t.Error("nothing listening, and no error")
	}
}

func TestATurnStillBeingWrittenWaits(t *testing.T) {
	a, _ := fixture.Open(t)
	var buf bytes.Buffer
	// Alpha's second turn and its subagent ran half an hour ago; the rest, an hour and more
	res, err := Export(context.Background(), a.Index, Options{Filter: index.Filter{Stores: a.Stores()}, Settle: 45 * time.Minute, DryRun: &buf})
	if err != nil || res.Turns != 2 {
		t.Fatalf("export = %+v, %v: want the two turns that have been quiet for 45 minutes", res, err)
	}
	res, _ = Export(context.Background(), a.Index, Options{Filter: index.Filter{Stores: a.Stores()}, Window: index.Window{Since: time.Now().Add(-24 * time.Hour)}, DryRun: io.Discard})
	if res.Turns != 3 {
		t.Errorf("the last day = %+v, want alpha's two turns and its subagent's", res)
	}
	res, _ = Export(context.Background(), a.Index, Options{Filter: index.Filter{Stores: a.Stores(), Project: "beta"}, DryRun: io.Discard})
	if res.Turns != 1 {
		t.Errorf("one project = %+v", res)
	}
}

// refusal is a store of one session whose one turn came to a provider's refusal at at. The
// CLI marks every turn over, and has not marked this one: until long enough after at, the
// session is said to be at work on it.
type refusal struct{ at time.Time }

func (refusal) Location() string { return "test" }
func (refusal) Exists() bool     { return true }
func (r refusal) Scan(context.Context) ([]sources.Ref, error) {
	return []sources.Ref{{Key: "k", Stamp: "1", ModTime: r.at}}, nil
}
func (r refusal) Read(_ context.Context, _ sources.Ref, w *transcript.Writer) error {
	w.S.ID, w.Tells = "refused", transcript.TellsByMarks
	w.Add(transcript.Message{Role: transcript.User, At: r.at, Blocks: []transcript.Block{transcript.TextOf("deploy it")}})
	w.Add(transcript.Message{Role: transcript.System, At: r.at.Add(time.Second), Blocks: []transcript.Block{transcript.EventOf(model.EventError, "402 Insufficient Balance")}})
	return nil
}

// A turn the agent is at has not ended, and is not sent: sent, it would go out with nothing
// of how it ended, and the mark that it is over, which moves no hash, would not send it
// again. It goes once the session no longer stands so.
func TestATurnUnderWayWaits(t *testing.T) {
	ix, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	store := []index.Store{{Name: "dsh", Spec: sources.Spec{Name: "dsh", Reads: 1}, Source: refusal{at: time.Now().Add(-time.Minute)}}}
	if _, err := ix.Sync(ctx, store, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(t.TempDir(), "ledger.db")
	c := newCollector(t)
	opts := Options{Endpoint: c.URL, Ledger: ledger, Filter: index.Filter{}}
	if res, err := Export(ctx, ix, opts); err != nil || res.Turns != 0 {
		t.Fatalf("at work: %+v, %v; want nothing sent", res, err)
	}

	// The session past being said to be at work: the turn has ended, on its error
	store[0].Source = refusal{at: time.Now().Add(-index.WorkingFor - time.Minute)}
	store[0].Spec.Reads = 2
	if _, err := ix.Sync(ctx, store, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := Export(ctx, ix, opts)
	if err != nil || res.Turns != 1 {
		t.Fatalf("once it ended: %+v, %v; want the turn sent", res, err)
	}
	root := pick(t, decode(t, c.bodies...), "agent", "dsh turn 1")
	if root.attr("langfuse.observation.level") != "ERROR" || root.attr("asq.error") != "true" {
		t.Errorf("the turn sent = %+v", root.Attributes)
	}
}

func TestDestinations(t *testing.T) {
	for in, want := range map[string]string{
		"https://Cloud.Langfuse.com/api/public/otel":            "https://cloud.langfuse.com/api/public/otel",
		"https://cloud.langfuse.com/api/public/otel/":           "https://cloud.langfuse.com/api/public/otel",
		"https://cloud.langfuse.com/api/public/otel/v1/traces":  "https://cloud.langfuse.com/api/public/otel",
		"https://user:secret@collector.example:4318/?key=value": "https://collector.example:4318",
		"http://localhost:4318":                                 "http://localhost:4318",
	} {
		if got := destOf(in); got != want {
			t.Errorf("destOf(%q) = %q, want %q", in, got, want)
		}
	}
	if tracesURL("http://c:4318") != "http://c:4318/v1/traces" || tracesURL("http://c:4318/v1/traces/") != "http://c:4318/v1/traces" {
		t.Error("the traces path is appended once")
	}
	for in, want := range map[string]Level{"": LevelNone, "none": LevelNone, "asks": LevelAsks, "ALL": LevelAll, "io": LevelAll} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLevel("everything"); err == nil {
		t.Error("an unknown level is an error: sending more than was meant is the mistake to prevent")
	}
}
