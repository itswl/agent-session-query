package app

import (
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
