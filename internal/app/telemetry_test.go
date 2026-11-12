package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/otlp"
	"github.com/itswl/agent-session-query/internal/source"
)

// TestBuildSessionSpans: one trace per round, a root span, a generation per assistant
// message, a tool span paired with its result, and the deterministic ids that make a
// re-export an update rather than a duplicate.
func TestBuildSessionSpans(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "otel-1", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)
	rec := api.activeSources()[0].List()[0]
	messages := buildTestMessages(api, rec)

	spans := buildSessionSpans(rec, messages, false)
	if len(spans) != 4 {
		t.Fatalf("spans = %d, want root + 2 generations + 1 tool", len(spans))
	}
	byName := map[string][]int{}
	for i, span := range spans {
		byName[span.Name] = append(byName[span.Name], i)
		if span.TraceID == "" || span.SpanID == "" {
			t.Fatalf("span %q has no ids", span.Name)
		}
	}
	if len(byName["round 1"]) != 1 || len(byName["model"]) != 2 || len(byName["Bash"]) != 1 {
		t.Fatalf("span names = %v", byName)
	}
	rootIndex := byName["round 1"][0]
	rootSpan := spans[rootIndex]
	if rootSpan.ParentID != "" {
		t.Fatalf("root has a parent: %v", rootSpan.ParentID)
	}
	for _, span := range spans {
		if span.Name != "round 1" && span.ParentID != rootSpan.SpanID {
			t.Fatalf("%q parent = %q, want the root %q", span.Name, span.ParentID, rootSpan.SpanID)
		}
		if span.TraceID != rootSpan.TraceID {
			t.Fatalf("%q is in another trace", span.Name)
		}
	}

	tool := spans[byName["Bash"][0]]
	if !tool.Error || !strings.Contains(tool.Status, "build failed") {
		t.Fatalf("failed tool = %+v", tool)
	}
	if attrValue(tool, "timing.source") != "derived" {
		t.Fatalf("tool timing = %v", attrValue(tool, "timing.source"))
	}
	gen := spans[byName["model"][0]]
	if attrValue(gen, "timing.source") != "inferred" {
		t.Fatalf("generation timing = %v", attrValue(gen, "timing.source"))
	}
	if attrValue(gen, "gen_ai.usage.input_tokens") != int64(100) && attrValue(gen, "gen_ai.usage.input_tokens") != int64(50) {
		t.Fatalf("generation usage = %v", attrValue(gen, "gen_ai.usage.input_tokens"))
	}

	// Deterministic ids: the same session rendered twice sends the same spans
	again := buildSessionSpans(rec, messages, false)
	for i := range spans {
		if spans[i].SpanID != again[i].SpanID || spans[i].TraceID != again[i].TraceID {
			t.Fatalf("span %d changed id between renders", i)
		}
	}

	// Content off: no body attributes; on: prompts and outputs, with secrets masked
	for _, span := range spans {
		if attrValue(span, "langfuse.observation.input") != nil {
			t.Fatalf("metadata export carried a body on %q", span.Name)
		}
	}
	withBodies := buildSessionSpans(rec, messages, true)
	found := false
	for _, span := range withBodies {
		if attrValue(span, "langfuse.observation.input") != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("--otel-content=io exported no input bodies")
	}
}

// buildTestMessages reads one session whole, as the exporter does
func buildTestMessages(api *SessionQueryAPI, rec source.Record) []map[string]any {
	exporter := newOTelExporter(api, otelConfig{})
	return exporter.allMessages(api.activeSources()[0], rec)
}

// attrValue looks one attribute up by key
func attrValue(span otlp.Span, key string) any {
	for _, a := range span.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

// TestOTelExportOnce: the exporter posts OTLP JSON to the endpoint, remembers the version
// it sent, and only the resend flag makes it send the same session again.
func TestOTelExportOnce(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "otel-2", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	var mu sync.Mutex
	requests, sentSpans := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Basic test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("body: %v", err)
		}
		mu.Lock()
		requests++
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				sentSpans += len(ss.Spans)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "otel.json")
	cfg := otelConfig{
		endpoint: server.URL, headers: map[string]string{"Authorization": "Basic test"},
		since: time.Now().Add(-time.Hour), statePath: statePath,
	}
	exporter := newOTelExporter(api, cfg)
	sessions, spans := exporter.exportOnce(context.Background(), cfg.since, time.Time{})
	if sessions != 1 || spans != 4 {
		t.Fatalf("first pass = %d sessions / %d spans, want 1 / 4", sessions, spans)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state was not written: %v", err)
	}

	sessions, _ = exporter.exportOnce(context.Background(), cfg.since, time.Time{})
	if sessions != 0 {
		t.Fatalf("second pass re-sent %d sessions", sessions)
	}

	exporter.cfg.resend = true
	sessions, _ = exporter.exportOnce(context.Background(), cfg.since, time.Time{})
	if sessions != 1 {
		t.Fatalf("resend exported %d sessions", sessions)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 || sentSpans != 8 {
		t.Fatalf("collector saw %d requests / %d spans, want 2 / 8", requests, sentSpans)
	}
}

// TestOTelConfigFlags: the flags validate into one config — headers decoded, content
// spelled two ways, "all" opening the window, an endpoint that is not a URL refused.
func TestOTelConfigFlags(t *testing.T) {
	cfg, err := otelConfigFromFlags("https://cloud.langfuse.com/api/public/otel",
		"Authorization=Basic%20abc,x-langfuse-ingestion-version=4", "io", "all", 10*time.Minute, 2*time.Minute, "off", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.headers["Authorization"] != "Basic abc" || cfg.headers["x-langfuse-ingestion-version"] != "4" {
		t.Fatalf("headers = %v", cfg.headers)
	}
	if !cfg.content || !cfg.since.IsZero() || cfg.statePath != "" {
		t.Fatalf("config = %+v", cfg)
	}
	if _, err := otelConfigFromFlags("ftp://example.com", "", "none", "7d", 0, 0, "", false, false, false); err == nil {
		t.Fatal("a non-http endpoint was accepted")
	}
	if _, err := otelConfigFromFlags("https://example.com", "", "everything", "7d", 0, 0, "", false, false, false); err == nil {
		t.Fatal("an unknown --otel-content was accepted")
	}
}

// appendRound adds one more ask and answer to a fixture session, the way a CLI appends a
// turn: the file grows and the session's UpdatedAt moves.
func appendRound(t *testing.T, path string, at time.Time) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stamp := func(d time.Duration) string { return at.Add(d).UTC().Format("2006-01-02T15:04:05Z") }
	lines := []string{
		`{"type":"user","timestamp":"` + stamp(0) + `","message":{"role":"user","content":"and the docs"}}`,
		`{"type":"assistant","timestamp":"` + stamp(time.Second) + `","message":{"role":"assistant","usage":{"input_tokens":20,"output_tokens":2},"content":[{"type":"text","text":"docs updated"}]}}`,
	}
	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOTelRoundIncremental: a session that grew sends only the round that changed, not
// the whole conversation again — the point of the per-round fingerprints.
func TestOTelRoundIncremental(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "otel-3", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	var mu sync.Mutex
	var rounds []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name       string `json:"name"`
						Attributes []struct {
							Key   string `json:"key"`
							Value struct {
								StringValue string `json:"stringValue"`
								IntValue    string `json:"intValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("body: %v", err)
		}
		mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, span := range ss.Spans {
					round := ""
					for _, a := range span.Attributes {
						if a.Key == "round.index" {
							round = a.Value.IntValue
						}
					}
					rounds = append(rounds, span.Name+"@"+round)
				}
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := otelConfig{
		endpoint: server.URL, since: time.Now().Add(-time.Hour),
		statePath: filepath.Join(t.TempDir(), "otel.json"),
	}
	exporter := newOTelExporter(api, cfg)
	exporter.exportOnce(context.Background(), cfg.since, time.Time{})

	// The session grows by one round
	path := filepath.Join(root, "proj", "otel-3.jsonl")
	appendRound(t, path, time.Now().Add(-30*time.Minute))
	secondClient := source.NewClaudeSource(root) // a fresh source sees the new file
	api.sources = []source.SessionSource{secondClient}
	exporter.api = api
	exporter.exportOnce(context.Background(), cfg.since, time.Time{})

	mu.Lock()
	defer mu.Unlock()
	seenSecond := map[string]bool{}
	for _, name := range rounds {
		seenSecond[name] = true
	}
	if !seenSecond["round 2@2"] {
		t.Fatalf("the new round was not exported: %v", rounds)
	}
	if len(rounds) != 6 { // 4 + the new round's root and generation
		t.Fatalf("spans sent = %d (%v), want 6 (the first round not repeated)", len(rounds), rounds)
	}
}

// TestOTelMetrics: with metrics on, the pass posts delta sums for rounds, tokens and tool
// failures to /v1/metrics.
func TestOTelMetrics(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "otel-4", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	var mu sync.Mutex
	metricNames := map[string]bool{}
	tokenTypes := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ResourceMetrics []struct {
				ScopeMetrics []struct {
					Metrics []struct {
						Name string `json:"name"`
						Sum  struct {
							DataPoints []struct {
								Attributes []struct {
									Key   string `json:"key"`
									Value struct {
										StringValue string `json:"stringValue"`
									} `json:"value"`
								} `json:"attributes"`
							} `json:"dataPoints"`
						} `json:"sum"`
					} `json:"metrics"`
				} `json:"scopeMetrics"`
			} `json:"resourceMetrics"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("metrics body: %v", err)
		}
		mu.Lock()
		for _, rm := range payload.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, metric := range sm.Metrics {
					metricNames[metric.Name] = true
					for _, point := range metric.Sum.DataPoints {
						for _, a := range point.Attributes {
							if a.Key == "gen_ai.token.type" {
								tokenTypes[a.Value.StringValue] = true
							}
						}
					}
				}
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := otelConfig{
		endpoint: server.URL, since: time.Now().Add(-time.Hour),
		statePath: filepath.Join(t.TempDir(), "otel.json"), metrics: true,
	}
	exporter := newOTelExporter(api, cfg)
	exporter.exportOnce(context.Background(), cfg.since, time.Time{})

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"agent_session.rounds", "agent_session.tokens", "agent_session.tool.failures"} {
		if !metricNames[want] {
			t.Fatalf("metric %q missing from %v", want, metricNames)
		}
	}
	if !tokenTypes["input"] || !tokenTypes["output"] {
		t.Fatalf("token types = %v", tokenTypes)
	}
}

// writeSplitResponseSession is a Claude Code transcript the way the CLI writes one: a
// response arrives as one row per content block, each row repeating the response's id,
// model and whole usage, with the tool results between them.
//
// Round 1 asks; the response (msg_a) says something and calls two tools, the first fails
// and the second works, then a second response (msg_b) answers — the round went on.
// Round 2 asks and its one tool call fails last. Round 3 asks and is answered two hours
// after the ask: no model call takes that long. Days later the CLI resumes the session
// and writes a <synthetic> row, which no model answered.
func writeSplitResponseSession(t *testing.T, root, id string, base time.Time) string {
	t.Helper()
	at := func(d time.Duration) string { return base.Add(d).UTC().Format("2006-01-02T15:04:05Z") }
	// msg_a's rows report a running output count, as CLI 2.1.284–2.1.304 wrote it: the
	// response's figure is the last row's 40, not the first row's 1
	usageA := func(out int) string {
		return `"usage":{"input_tokens":100,"output_tokens":` + strconv.Itoa(out) + `,"cache_read_input_tokens":5000}`
	}
	usageB := `"usage":{"input_tokens":7,"output_tokens":3}`
	path := filepath.Join(root, "proj", id+".jsonl")
	write(t, path,
		`{"type":"user","uuid":"u1","sessionId":"`+id+`","cwd":"/w/proj","timestamp":"`+at(0)+`","message":{"role":"user","content":"fix the build\nsecond line of the ask"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"`+at(10*time.Second)+`","message":{"id":"msg_a","role":"assistant","model":"claude-test-1",`+usageA(1)+`,"content":[{"type":"text","text":"Looking at it."}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"`+at(11*time.Second)+`","message":{"id":"msg_a","role":"assistant","model":"claude-test-1",`+usageA(25)+`,"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make"}}]}}`,
		`{"type":"user","uuid":"r1","timestamp":"`+at(12*time.Second)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"make: error"}]}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"`+at(13*time.Second)+`","message":{"id":"msg_a","role":"assistant","model":"claude-test-1",`+usageA(40)+`,"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"make clean"}}]}}`,
		`{"type":"user","uuid":"r2","timestamp":"`+at(14*time.Second)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}}`,
		`{"type":"assistant","uuid":"a4","timestamp":"`+at(20*time.Second)+`","message":{"id":"msg_b","role":"assistant","model":"claude-test-1",`+usageB+`,"content":[{"type":"text","text":"Fixed."}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"`+at(time.Minute)+`","message":{"role":"user","content":"deploy it"}}`,
		`{"type":"assistant","uuid":"a5","timestamp":"`+at(time.Minute+5*time.Second)+`","message":{"id":"msg_c","role":"assistant","model":"claude-test-1",`+usageB+`,"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"deploy"}}]}}`,
		`{"type":"user","uuid":"r3","timestamp":"`+at(time.Minute+6*time.Second)+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","is_error":true,"content":"denied"}]}}`,
		`{"type":"user","uuid":"u3","timestamp":"`+at(10*time.Minute)+`","message":{"role":"user","content":"think about it"}}`,
		`{"type":"assistant","uuid":"a6","timestamp":"`+at(10*time.Minute+2*time.Hour)+`","message":{"id":"msg_d","role":"assistant","model":"claude-test-2",`+usageB+`,"content":[{"type":"text","text":"Done thinking."}]}}`,
		`{"type":"assistant","uuid":"a7","timestamp":"`+at(15*24*time.Hour)+`","message":{"id":"syn","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`,
	)
	return path
}

// spansIn picks the spans of one round, by its round index
func spansIn(spans []otlp.Span, round int64) []otlp.Span {
	var out []otlp.Span
	for _, span := range spans {
		if n, _ := source.ToFloat(attrValue(span, "round.index")); int64(n) == round {
			out = append(out, span)
		}
	}
	return out
}

func kindOf(span otlp.Span) string { return source.ToStr(attrValue(span, "langfuse.observation.type")) }

// TestClaudeSplitResponseUsage: the source carries a split response's usage on its first
// row only — so the rounds API, the brief and the export each count the response once —
// and every row says which response and which model it is.
func TestClaudeSplitResponseUsage(t *testing.T) {
	root := t.TempDir()
	writeSplitResponseSession(t, root, "split-1", time.Now().Add(-20*24*time.Hour))
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)
	rec := api.activeSources()[0].List()[0]
	withUsage := map[string]int{}
	for _, m := range buildTestMessages(api, rec) {
		if source.ToStr(m["role"]) != "assistant" {
			continue
		}
		if m["response"] == nil || m["model"] == nil {
			t.Fatalf("assistant row lacks response/model: %v", m)
		}
		if m["usage"] != nil {
			withUsage[source.ToStr(m["response"])]++
		}
	}
	if withUsage["msg_a"] != 1 {
		t.Fatalf("msg_a carries usage on %d rows, want 1", withUsage["msg_a"])
	}
	out, err := api.sessionRounds("split-1", "")
	if err != nil {
		t.Fatal(err)
	}
	usage := out["rounds"].([]map[string]any)[0]["usage"].(map[string]any)
	if usage["inputTokens"] != int64(107) || usage["outputTokens"] != int64(43) {
		t.Fatalf("round 1 usage = %v, want 107 in / 43 out (msg_a once + msg_b)", usage)
	}
}

// TestOTelGenerationFidelity: one generation per response with its model and its usage
// once; a synthetic row is no generation and does not stretch its round; an inferred
// start never reaches past the cap; a round that recovered from a failed call is a
// warning and one that ended on it an error; the ask names the round only when content
// is exported.
func TestOTelGenerationFidelity(t *testing.T) {
	root := t.TempDir()
	base := time.Now().Add(-20 * 24 * time.Hour)
	writeSplitResponseSession(t, root, "split-2", base)
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)
	rec := api.activeSources()[0].List()[0]
	messages := buildTestMessages(api, rec)
	spans := buildSessionSpans(rec, messages, true)

	round1 := spansIn(spans, 1)
	var gens []otlp.Span
	var rootSpan otlp.Span
	for _, span := range round1 {
		switch kindOf(span) {
		case "generation":
			gens = append(gens, span)
		case "span":
			rootSpan = span
		}
	}
	if len(gens) != 2 {
		t.Fatalf("round 1 generations = %d, want 2 (msg_a's three rows folded into one, msg_b)", len(gens))
	}
	var input, output int64
	for _, gen := range gens {
		if attrValue(gen, "gen_ai.request.model") != "claude-test-1" || gen.Name != "model claude-test-1" {
			t.Fatalf("generation model = %v / %q", attrValue(gen, "gen_ai.request.model"), gen.Name)
		}
		n, _ := source.ToFloat(attrValue(gen, "gen_ai.usage.input_tokens"))
		o, _ := source.ToFloat(attrValue(gen, "gen_ai.usage.output_tokens"))
		input, output = input+int64(n), output+int64(o)
	}
	if input != 107 || output != 43 {
		t.Fatalf("round 1 tokens = %d in / %d out, want 107 / 43", input, output)
	}
	// Keyed by the response's first row, as that row's generation was before rows folded
	if want := otlp.NewSpanID(rec.Source, rec.SessionID, "1", "msg", "a1"); gens[0].SpanID != want {
		t.Fatalf("msg_a generation id = %s, want the first row's %s", gens[0].SpanID, want)
	}
	if want := otlp.NewSpanID(rec.Source, rec.SessionID, "1", "msg", "a4"); gens[1].SpanID != want {
		t.Fatalf("msg_b generation id = %s, want its row's %s", gens[1].SpanID, want)
	}
	if !strings.Contains(source.ToStr(attrValue(gens[0], "langfuse.observation.output")), "Looking at it.") {
		t.Fatalf("msg_a output = %v", attrValue(gens[0], "langfuse.observation.output"))
	}
	if rootSpan.Error || attrValue(rootSpan, "langfuse.observation.level") != "WARNING" {
		t.Fatalf("a round that recovered from a failed call: error=%v level=%v", rootSpan.Error, attrValue(rootSpan, "langfuse.observation.level"))
	}
	if attrValue(rootSpan, "langfuse.trace.name") != "fix the build" {
		t.Fatalf("trace name = %v, want the ask's first line", attrValue(rootSpan, "langfuse.trace.name"))
	}

	for _, span := range spansIn(spans, 2) {
		if kindOf(span) == "span" && !span.Error {
			t.Fatalf("a round that ended on a failed call is not an error: %+v", span)
		}
		if kindOf(span) == "generation" && !strings.Contains(source.ToStr(attrValue(span, "langfuse.observation.output")), `"tool_calls"`) {
			t.Fatalf("a tool-only response has no output: %v", attrValue(span, "langfuse.observation.output"))
		}
	}

	for _, span := range spansIn(spans, 3) {
		if span.End.Sub(span.Start) > 3*time.Hour {
			t.Fatalf("%q spans %v: the synthetic row stretched the round", span.Name, span.End.Sub(span.Start))
		}
		if kindOf(span) == "generation" {
			if span.Start != span.End {
				t.Fatalf("an inferred start two hours back was kept: %v", span.End.Sub(span.Start))
			}
			if attrValue(span, "gen_ai.request.model") != "claude-test-2" {
				t.Fatalf("round 3 model = %v", attrValue(span, "gen_ai.request.model"))
			}
		}
		if strings.Contains(span.Name, "synthetic") {
			t.Fatalf("the synthetic row became a span: %q", span.Name)
		}
	}

	// Metadata only: the ask stays home, the round is named by the session's title
	for _, span := range buildSessionSpans(rec, messages, false) {
		if attrValue(span, "langfuse.trace.name") != rec.ShortKey {
			t.Fatalf("metadata export named a trace %v, want the session title %q", attrValue(span, "langfuse.trace.name"), rec.ShortKey)
		}
	}
}

// TestOTelStoppedPassKeepsCursor: a pass stopped part-way still saves what it sent, so a
// re-run goes on instead of starting over.
func TestOTelStoppedPassKeepsCursor(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "stop-1", "proj")
	writeActivitySession(t, root, "stop-2", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	requests, metricsPosts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/v1/metrics" {
			metricsPosts++ // after the walk, with the run already stopped
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		requests++
		n := requests
		mu.Unlock()
		if n == 2 {
			cancel() // the run is stopped while the second is on the wire: that one is not sent
			select { // the client gives up at once; the server may not notice the closed connection
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusOK) // the first lands, and so does the re-run
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "otel.json")
	cfg := otelConfig{endpoint: server.URL, since: time.Now().Add(-time.Hour), statePath: statePath, metrics: true}
	sessions, _ := newOTelExporter(api, cfg).exportOnce(ctx, cfg.since, time.Time{})
	if sessions != 1 {
		t.Fatalf("stopped pass sent %d sessions, want 1", sessions)
	}
	mu.Lock()
	posted := metricsPosts
	mu.Unlock()
	if posted != 1 {
		t.Fatalf("a stopped pass posted metrics %d times, want once for the session it sent", posted)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("a stopped pass wrote no cursor: %v", err)
	}
	var state otelState
	if err := json.Unmarshal(raw, &state); err != nil || len(state.Sessions) != 1 {
		t.Fatalf("cursor = %s (%v), want the one session sent", raw, err)
	}

	again, _ := newOTelExporter(api, cfg).exportOnce(context.Background(), cfg.since, time.Time{})
	if again != 1 {
		t.Fatalf("the re-run sent %d sessions, want only the one left", again)
	}
}

// TestOTelMeasuredGeneration: a source that measured the call (Pi's durationMs, counted
// from the message's creation) draws the generation as it happened, marked recorded.
func TestOTelMeasuredGeneration(t *testing.T) {
	rec := source.Record{Source: "pi", SessionID: "pi-measured", ShortKey: "measured"}
	messages := []map[string]any{
		{"role": "user", "timestamp": "2026-10-01T10:00:00Z", "content": []map[string]any{source.TextBlock("go")}},
		{"role": "assistant", "timestamp": "2026-10-01T10:00:05Z", "model": "glm-x", "durationMs": float64(4200),
			"content": []map[string]any{source.TextBlock("done")}},
	}
	for _, span := range buildSessionSpans(rec, messages, false) {
		if kindOf(span) != "generation" {
			continue
		}
		if span.End.Sub(span.Start) != 4200*time.Millisecond || attrValue(span, "timing.source") != "recorded" {
			t.Fatalf("measured generation = %v long, timing %v", span.End.Sub(span.Start), attrValue(span, "timing.source"))
		}
		return
	}
	t.Fatal("no generation")
}

// TestOTelGenerationChatIO: a generation's input is what reached the model since its last
// call — the ask for the first, the tool results for the next, never the ask again — and
// its output is the response as an assistant message with its tool calls, so a backend
// renders the round as the conversation it was.
func TestOTelGenerationChatIO(t *testing.T) {
	root := t.TempDir()
	writeSplitResponseSession(t, root, "split-io", time.Now().Add(-20*24*time.Hour))
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)
	rec := api.activeSources()[0].List()[0]
	var gens []otlp.Span
	for _, span := range spansIn(buildSessionSpans(rec, buildTestMessages(api, rec), true), 1) {
		if kindOf(span) == "generation" {
			gens = append(gens, span)
		}
	}
	if len(gens) != 2 {
		t.Fatalf("generations = %d", len(gens))
	}
	decode := func(span otlp.Span, key string, into any) {
		t.Helper()
		if err := json.Unmarshal([]byte(source.ToStr(attrValue(span, key))), into); err != nil {
			t.Fatalf("%s of %q is not chat JSON: %v", key, span.Name, err)
		}
	}
	var firstIn, secondIn []map[string]any
	decode(gens[0], "langfuse.observation.input", &firstIn)
	decode(gens[1], "langfuse.observation.input", &secondIn)
	if len(firstIn) != 1 || firstIn[0]["role"] != "user" || !strings.HasPrefix(source.ToStr(firstIn[0]["content"]), "fix the build") {
		t.Fatalf("first input = %v, want the ask alone", firstIn)
	}
	if len(secondIn) != 2 || secondIn[0]["role"] != "tool" || secondIn[0]["tool_call_id"] != "t1" || secondIn[1]["tool_call_id"] != "t2" {
		t.Fatalf("second input = %v, want the two tool results and not the ask again", secondIn)
	}
	var reply map[string]any
	decode(gens[0], "langfuse.observation.output", &reply)
	calls, _ := reply["tool_calls"].([]any)
	if reply["role"] != "assistant" || reply["content"] != "Looking at it." || len(calls) != 2 {
		t.Fatalf("first output = %v, want the words and both tool calls", reply)
	}
	fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Bash" || !strings.Contains(source.ToStr(fn["arguments"]), "make") {
		t.Fatalf("tool call = %v", calls[0])
	}
}

// TestOTelTagsAndFirstToken: a round's root carries trace tags — where it came from and how
// it went — and a measured generation with a first-token time carries
// completion_start_time, the time to first token a backend shows.
func TestOTelTagsAndFirstToken(t *testing.T) {
	root := t.TempDir()
	writeSplitResponseSession(t, root, "tags-1", time.Now().Add(-20*24*time.Hour))
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)
	rec := api.activeSources()[0].List()[0]
	tagsOf := func(round int64) []string {
		for _, span := range spansIn(buildSessionSpans(rec, buildTestMessages(api, rec), false), round) {
			if kindOf(span) == "span" {
				tags, _ := attrValue(span, "langfuse.trace.tags").([]string)
				return tags
			}
		}
		return nil
	}
	if got := strings.Join(tagsOf(1), ","); got != "claude,project:proj,tool-failure" {
		t.Fatalf("round 1 tags = %q, want the source, the project and the recovered failure", got)
	}
	if got := strings.Join(tagsOf(2), ","); got != "claude,project:proj,tool-failure,failed" {
		t.Fatalf("round 2 tags = %q", got)
	}

	oc := source.Record{Source: "opencode", SessionID: "oc-ttft", ShortKey: "ttft"}
	messages := []map[string]any{
		{"role": "user", "timestamp": "2026-10-01T10:00:00Z", "content": []map[string]any{source.TextBlock("go")}},
		{"role": "assistant", "timestamp": "2026-10-01T10:00:01Z", "model": "personal/deepseek-flash",
			"durationMs": float64(4800), "firstTokenMs": float64(1080), "content": []map[string]any{source.TextBlock("done")}},
	}
	for _, span := range buildSessionSpans(oc, messages, false) {
		if kindOf(span) == "generation" {
			if got := attrValue(span, "langfuse.observation.completion_start_time"); got != "2026-10-01T10:00:02.08Z" {
				t.Fatalf("completion_start_time = %v, want 1.08 s after the call began", got)
			}
			return
		}
	}
	t.Fatal("no generation")
}

// TestOTelRecordedCost: a cost the CLI worked out goes with the generation; a zero (a CLI
// with no price for the model) is not sent, so the backend's own price can apply
func TestOTelRecordedCost(t *testing.T) {
	rec := source.Record{Source: "opencode", SessionID: "oc-cost", ShortKey: "cost"}
	messages := []map[string]any{
		{"role": "user", "timestamp": "2026-10-01T10:00:00Z", "content": []map[string]any{source.TextBlock("go")}},
		{"role": "assistant", "timestamp": "2026-10-01T10:00:01Z", "id": "a1", "usage": map[string]any{"inputTokens": 10, "estimatedCostUsd": 0.0123},
			"content": []map[string]any{source.TextBlock("paid")}},
		{"role": "assistant", "timestamp": "2026-10-01T10:00:02Z", "id": "a2", "usage": map[string]any{"inputTokens": 10, "estimatedCostUsd": 0.0},
			"content": []map[string]any{source.TextBlock("unpriced")}},
	}
	var costs []any
	for _, span := range buildSessionSpans(rec, messages, false) {
		if kindOf(span) == "generation" {
			costs = append(costs, attrValue(span, "langfuse.observation.cost_details"))
		}
	}
	if len(costs) != 2 || costs[0] != `{"total":0.0123}` || costs[1] != nil {
		t.Fatalf("cost_details = %v, want the recorded cost on the first and none on the zero", costs)
	}
}

// TestOTelSpanFormatUpgrade: a cursor written before spans were one generation per response
// (no format) is reported once and saved as the current format; a current or new cursor
// is not reported.
func TestOTelSpanFormatUpgrade(t *testing.T) {
	root := t.TempDir()
	writeActivitySession(t, root, "fmt-1", "proj")
	api := newSessionQueryAPI([]source.SessionSource{source.NewClaudeSource(root)}, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "otel.json")
	cfg := otelConfig{endpoint: server.URL, since: time.Now().Add(-time.Hour), statePath: statePath}
	if e := newOTelExporter(api, cfg); e.priorFormat != 0 {
		t.Fatalf("a new cursor reported format %d", e.priorFormat)
	}
	old := `{"version":2,"sessions":{"claude|elsewhere":{"updatedAt":"2026-10-01T10:00:00Z","rounds":{"1":"ab"}}}}`
	if err := os.WriteFile(statePath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newOTelExporter(api, cfg)
	if e.priorFormat != 1 {
		t.Fatalf("a cursor with no format reported %d, want 1", e.priorFormat)
	}
	e.exportOnce(context.Background(), cfg.since, time.Time{})
	var saved otelState
	raw, _ := os.ReadFile(statePath)
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Format != otelSpanFormat || len(saved.Sessions) != 2 {
		t.Fatalf("saved cursor = %s (%v), want format %d and both sessions", raw, err, otelSpanFormat)
	}
	if e := newOTelExporter(api, cfg); e.priorFormat != 0 {
		t.Fatalf("a current cursor reported format %d", e.priorFormat)
	}
}
