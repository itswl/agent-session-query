package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/otlp"
	"github.com/itswl/agent-session-query/internal/source"
)

// OTLP export: the sessions this service reads, rendered as traces and sent to any OTLP
// collector (Langfuse included). The export is generic — one span model for every source,
// attributes from the OpenTelemetry GenAI conventions plus a few langfuse.* names — and it
// never branches per provider. Timing is the only place the sources really differ, and
// every span says where its times came from: recorded (the CLI wrote a start/end or a
// duration), derived (start and end are real row times, the span between them is the
// elapsed time), or inferred (the start is the previous event, because the CLI records
// only completion).

const otelMaxMessages = 500_000 // a hard stop for a pathological transcript

type otelConfig struct {
	endpoint  string
	headers   map[string]string
	content   bool // request/response bodies on (off by default)
	metrics   bool // also send OTLP sums to /v1/metrics
	since     time.Time
	every     time.Duration
	settle    time.Duration
	statePath string // empty means no state: every run re-sends its window
	resend    bool
	dryRun    bool
}

func (c otelConfig) enabled() bool { return c.endpoint != "" }

// otelExporter runs the export. One pass at a time: a slow collector or a big backfill must
// not overlap with the next tick.
type otelExporter struct {
	api    *SessionQueryAPI
	cfg    otelConfig
	client *http.Client
	mu     sync.Mutex
	state  *otelState
}

// otelState remembers, per session, the version that was exported. A single UpdatedAt is
// enough: a session's content only moves when its newest row does, and a session that grew
// is exported again under the same deterministic span ids.
type otelState struct {
	Version  int                    `json:"version"`
	LastRun  string                 `json:"lastRun,omitempty"`
	Sessions map[string]otelSession `json:"sessions,omitempty"`
}

// otelSession is how far one session has been exported: its version for the fast skip,
// and a fingerprint per round so a session that grew sends only the rounds that changed.
type otelSession struct {
	UpdatedAt string            `json:"updatedAt,omitempty"`
	At        string            `json:"at,omitempty"`
	Rounds    map[string]string `json:"rounds,omitempty"`
}

func newOTelExporter(api *SessionQueryAPI, cfg otelConfig) *otelExporter {
	e := &otelExporter{
		api:    api,
		cfg:    cfg,
		client: &http.Client{Timeout: 20 * time.Second},
		state:  &otelState{Version: 2, Sessions: map[string]otelSession{}},
	}
	if cfg.statePath != "" {
		e.loadState()
	}
	return e
}

// run exports the backfill window once, then the settled changes on every tick
func (e *otelExporter) run(ctx context.Context) {
	e.exportOnce(ctx, e.cfg.since, time.Time{})
	if e.cfg.every <= 0 {
		return
	}
	ticker := time.NewTicker(e.cfg.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.exportOnce(ctx, e.cfg.since, time.Now().Add(-e.cfg.settle))
		}
	}
}

// exportOnce walks the sessions updated in the window and exports the ones the state does
// not already know at their current version. settledBefore, when set, leaves a session
// that is still being written for a later tick.
func (e *otelExporter) exportOnce(ctx context.Context, since, settledBefore time.Time) (sessions, spans int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	started := time.Now()
	acc := newOTelMetricAcc()
	for _, src := range e.api.activeSources() {
		for _, rec := range e.api.recordsOf(src) {
			if ctx.Err() != nil {
				return sessions, spans
			}
			if rec.Archived || !withinWindow(rec.UpdatedAt, since, time.Time{}) {
				continue
			}
			if !settledBefore.IsZero() {
				at, ok := source.ParseTimestamp(rec.UpdatedAt)
				if !ok || at.After(settledBefore) {
					continue // still being written: next tick
				}
			}
			key := rec.Source + "|" + rec.SessionID
			if !e.cfg.resend && e.state.known(key, rec.UpdatedAt) {
				continue
			}
			messages := e.allMessages(src, rec)
			if len(messages) == 0 {
				continue
			}
			built := buildSessionSpans(rec, messages, e.cfg.content)
			if len(built) == 0 {
				continue
			}
			kept, changed := e.roundsToSend(key, built)
			if len(kept) == 0 {
				e.state.mark(key, rec.UpdatedAt, nil) // nothing new: remember the version
				continue
			}
			if e.cfg.dryRun {
				body, err := otlp.Encode(kept)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[WARN] otel: %s: %v\n", key, err)
					continue
				}
				fmt.Printf("=== %s %s: %d spans ===\n%s\n", rec.Source, rec.SessionID, len(kept), body)
			} else if err := e.send(ctx, kept); err != nil {
				fmt.Fprintf(os.Stderr, "[WARN] otel: %s: %v\n", key, err)
				continue // a failed session is not marked: the next run retries it
			}
			e.state.mark(key, rec.UpdatedAt, changed)
			sessions++
			spans += len(kept)
			if e.cfg.metrics {
				acc.add(kept)
			}
		}
	}
	if e.cfg.statePath != "" && !e.cfg.dryRun {
		e.state.LastRun = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		e.saveState()
	}
	if e.cfg.metrics && !e.cfg.dryRun {
		if metrics := acc.metrics(); len(metrics) > 0 {
			if err := otlp.SendMetrics(ctx, e.client, e.cfg.endpoint, e.cfg.headers, metrics, started); err != nil {
				fmt.Fprintf(os.Stderr, "[WARN] otel metrics: %v\n", err)
			}
		}
	}
	return sessions, spans
}

// roundsToSend drops the rounds the state already has at this fingerprint, so a session
// that grew sends only what changed instead of the whole conversation again.
func (e *otelExporter) roundsToSend(key string, spans []otlp.Span) ([]otlp.Span, map[string]string) {
	groups := groupRounds(spans)
	rounds := make([]int64, 0, len(groups))
	for round := range groups {
		rounds = append(rounds, round)
	}
	sort.Slice(rounds, func(i, j int) bool { return rounds[i] < rounds[j] })
	kept := []otlp.Span{}
	changed := map[string]string{}
	for _, round := range rounds {
		fp := roundFingerprint(groups[round])
		index := strconv.FormatInt(round, 10)
		if !e.cfg.resend && e.state.roundFP(key, index) == fp {
			continue
		}
		kept = append(kept, groups[round]...)
		changed[index] = fp
	}
	return kept, changed
}

// groupRounds buckets a session's spans by the round they belong to
func groupRounds(spans []otlp.Span) map[int64][]otlp.Span {
	out := map[int64][]otlp.Span{}
	for _, span := range spans {
		round, _ := source.ToFloat(spanAttribute(span, "round.index"))
		out[int64(round)] = append(out[int64(round)], span)
	}
	return out
}

// roundFingerprint is what a round looked like when it was exported: its spans' ids,
// names, times, status and attributes in build order
func roundFingerprint(spans []otlp.Span) string {
	h := sha256.New()
	for _, span := range spans {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%t\x00", span.SpanID, span.Name,
			span.Start.UnixNano(), span.End.UnixNano(), span.Error)
		for _, a := range span.Attributes {
			fmt.Fprintf(h, "%s=%v\x00", a.Key, a.Value)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// spanAttribute reads one attribute off a span
func spanAttribute(span otlp.Span, key string) any {
	for _, a := range span.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

// otelMetricAcc folds a pass's spans into OTLP sums. Deltas: every pass reports what it
// sent, and a collector adds them up.
type otelMetricAcc struct {
	rounds   map[string]*otlp.MetricPoint
	duration map[string]*otlp.MetricPoint
	tokens   map[string]*otlp.MetricPoint
	failures map[string]*otlp.MetricPoint
}

func newOTelMetricAcc() *otelMetricAcc {
	return &otelMetricAcc{
		rounds:   map[string]*otlp.MetricPoint{},
		duration: map[string]*otlp.MetricPoint{},
		tokens:   map[string]*otlp.MetricPoint{},
		failures: map[string]*otlp.MetricPoint{},
	}
}

func (a *otelMetricAcc) add(spans []otlp.Span) {
	for _, span := range spans {
		kind := source.ToStr(spanAttribute(span, "langfuse.observation.type"))
		src := source.ToStr(spanAttribute(span, "session.source"))
		model := source.ToStr(spanAttribute(span, "gen_ai.request.model"))
		switch kind {
		case "span":
			if !strings.HasPrefix(span.Name, "round ") {
				continue
			}
			point(a.rounds, src, []otlp.Attribute{{Key: "session.source", Value: src}}).Value++
			ms := span.End.Sub(span.Start).Milliseconds()
			if ms > 0 {
				point(a.duration, src, []otlp.Attribute{{Key: "session.source", Value: src}}).Value += ms
			}
		case "generation":
			for _, pair := range []struct{ attr, kind string }{
				{"gen_ai.usage.input_tokens", "input"},
				{"gen_ai.usage.output_tokens", "output"},
				{"gen_ai.usage.cache_read.input_tokens", "cache_read"},
				{"gen_ai.usage.cache_creation.input_tokens", "cache_creation"},
				{"gen_ai.usage.reasoning.output_tokens", "reasoning"},
			} {
				if n, ok := source.ToFloat(spanAttribute(span, pair.attr)); ok && n > 0 {
					key := src + "|" + model + "|" + pair.kind
					attrs := []otlp.Attribute{
						{Key: "session.source", Value: src},
						{Key: "gen_ai.request.model", Value: model},
						{Key: "gen_ai.token.type", Value: pair.kind},
					}
					point(a.tokens, key, attrs).Value += int64(n)
				}
			}
		case "tool":
			if !span.Error {
				continue
			}
			tool := source.ToStr(spanAttribute(span, "gen_ai.tool.name"))
			key := src + "|" + tool
			attrs := []otlp.Attribute{
				{Key: "session.source", Value: src},
				{Key: "gen_ai.tool.name", Value: tool},
			}
			point(a.failures, key, attrs).Value++
		}
	}
}

// point returns the accumulator's point for key, creating it with attrs
func point(points map[string]*otlp.MetricPoint, key string, attrs []otlp.Attribute) *otlp.MetricPoint {
	p, ok := points[key]
	if !ok {
		p = &otlp.MetricPoint{Attributes: attrs}
		points[key] = p
	}
	return p
}

func (a *otelMetricAcc) metrics() []otlp.Metric {
	out := []otlp.Metric{}
	out = append(out, metricFrom("agent_session.rounds", "1", true, a.rounds)...)
	out = append(out, metricFrom("agent_session.round.duration", "ms", false, a.duration)...)
	out = append(out, metricFrom("agent_session.tokens", "1", true, a.tokens)...)
	out = append(out, metricFrom("agent_session.tool.failures", "1", true, a.failures)...)
	return out
}

// metricFrom turns one accumulator into a metric with its points in a stable order
func metricFrom(name, unit string, monotonic bool, points map[string]*otlp.MetricPoint) []otlp.Metric {
	if len(points) == 0 {
		return nil
	}
	keys := make([]string, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	metric := otlp.Metric{Name: name, Unit: unit, Monotonic: monotonic}
	for _, key := range keys {
		metric.Points = append(metric.Points, *points[key])
	}
	return []otlp.Metric{metric}
}

// allMessages reads a session whole. The rounds API deliberately reads only the tail;
// an export is a record of the session, so it pages back until the start.
func (e *otelExporter) allMessages(src source.SessionSource, rec source.Record) []map[string]any {
	const page = 2000
	all := []map[string]any{}
	for offset := 0; offset < otelMaxMessages; offset += page {
		batch := src.Messages(rec, source.MessageQuery{Limit: page, FromEnd: true, Offset: offset})
		if len(batch) == 0 {
			break
		}
		all = append(batch, all...)
		if len(batch) < page {
			break
		}
	}
	return all
}

// send posts the spans in batches: a collector's request limit is real, and a session's
// tool output can be large
func (e *otelExporter) send(ctx context.Context, spans []otlp.Span) error {
	const batchSpans = 250
	const batchBytes = 8 << 20
	for start := 0; start < len(spans); {
		end := start
		size := 0
		for end < len(spans) && end-start < batchSpans {
			size += spanSize(spans[end])
			if size > batchBytes && end > start {
				break
			}
			end++
		}
		if err := otlp.Send(ctx, e.client, e.cfg.endpoint, e.cfg.headers, spans[start:end]); err != nil {
			return err
		}
		start = end
	}
	return nil
}

// spanSize approximates a span's encoded size: its string attributes dominate
func spanSize(s otlp.Span) int {
	n := 200
	for _, a := range s.Attributes {
		if text, ok := a.Value.(string); ok {
			n += len(text)
		}
	}
	return n
}

// buildSessionSpans renders one session as traces: one trace per round, a root span for
// the round, and generation / tool / event spans beneath it. Every source goes through
// this one function — the differences live in the messages, not in the exporter.
func buildSessionSpans(rec source.Record, messages []map[string]any, content bool) []otlp.Span {
	base := []string{rec.Source, rec.SessionID}
	var spans []otlp.Span
	roundNo := 0
	var root *otlp.Span
	var traceID, rootID string
	var ask, lastAssistant string
	var start, lastWork, lastEvent time.Time
	var failed bool
	tools := map[string]*otlp.Span{}
	lastModel := rec.Model

	flush := func() {
		if root == nil {
			return
		}
		end := lastWork
		if end.IsZero() || end.Before(start) {
			end = start
		}
		root.Start, root.End = start, end
		if failed {
			root.Error, root.Status = true, "a tool call failed in this round"
		}
		if content {
			root.Attributes = append(root.Attributes, otlp.Attribute{Key: "langfuse.observation.input", Value: contentText(ask)})
			root.Attributes = append(root.Attributes, otlp.Attribute{Key: "langfuse.observation.output", Value: contentText(lastAssistant)})
		}
		spans = append(spans, *root)
		for id, tool := range tools {
			if tool.End.Before(tool.Start) {
				tool.End = end
			}
			spans = append(spans, *tool)
			delete(tools, id)
		}
		root = nil
	}

	for _, m := range messages {
		role := source.ToStr(m["role"])
		at, ok := source.ParseTimestamp(source.ToStr(m["timestamp"]))
		if !ok {
			at = lastEvent
		}
		starting := isRoundStart(m)
		if starting {
			flush()
			roundNo++
			ask, lastAssistant = strings.TrimSpace(messageText(m["content"])), ""
			round := strconv.Itoa(roundNo)
			traceID = otlp.NewID(append(base, round, "trace")...)
			rootID = otlp.NewSpanID(append(base, round, "root")...)
			start, lastWork, lastEvent = at, at, at
			failed = false
			tools = map[string]*otlp.Span{}
			root = &otlp.Span{
				TraceID: traceID, SpanID: rootID, Name: "round " + round, Kind: 1, Start: at, End: at,
				Attributes: commonAttributes(rec, roundNo, "span", "derived"),
			}
		}
		if root == nil {
			lastEvent = at
			continue
		}
		if role == "assistant" {
			text := strings.TrimSpace(messageText(m["content"]))
			genStart := lastEvent
			if genStart.IsZero() || genStart.After(at) {
				genStart = at
			}
			gen := otlp.Span{
				TraceID:  traceID,
				SpanID:   otlp.NewSpanID(append(base, strconv.Itoa(roundNo), "msg", messageKey(m, role))...),
				ParentID: rootID, Name: "model", Kind: 1, Start: genStart, End: at,
				Attributes: commonAttributes(rec, roundNo, "generation", "inferred"),
			}
			if model := source.FirstNonEmpty(source.ToStr(m["model"]), lastModel); model != "" {
				gen.Name = "model " + model
				gen.Attributes = append(gen.Attributes, otlp.Attribute{Key: "gen_ai.request.model", Value: model})
				lastModel = model
			}
			gen.Attributes = append(gen.Attributes,
				otlp.Attribute{Key: "gen_ai.operation.name", Value: "chat"})
			gen.Attributes = append(gen.Attributes, usageAttributes(m["usage"])...)
			if content {
				gen.Attributes = append(gen.Attributes,
					otlp.Attribute{Key: "langfuse.observation.input", Value: contentText(ask)},
					otlp.Attribute{Key: "langfuse.observation.output", Value: contentText(text)})
			}
			spans = append(spans, gen)
			if text != "" {
				lastAssistant = text
			}
			if at.After(lastWork) {
				lastWork = at
			}
		}
		for _, block := range contentBlocks(m["content"]) {
			switch source.ToStr(block["type"]) {
			case "toolCall":
				name, id := source.ToStr(block["name"]), source.ToStr(block["id"])
				key := id
				if key == "" {
					key = "call:" + strconv.Itoa(len(spans))
				}
				tool := otlp.Span{
					TraceID: traceID, SpanID: otlp.NewSpanID(append(base, strconv.Itoa(roundNo), "tool", key)...),
					ParentID: rootID, Name: source.FirstNonEmpty(name, "tool"), Kind: 1, Start: at, End: at,
					Attributes: commonAttributes(rec, roundNo, "tool", "derived"),
				}
				tool.Attributes = append(tool.Attributes,
					otlp.Attribute{Key: "gen_ai.operation.name", Value: "execute_tool"})
				if name != "" {
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "gen_ai.tool.name", Value: name})
				}
				if id != "" {
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "gen_ai.tool.call.id", Value: id})
				}
				if command := commandText(block["arguments"]); command != "" {
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "tool.command", Value: contentText(command)})
				}
				if content {
					raw, _ := json.Marshal(block["arguments"])
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "langfuse.observation.input", Value: contentText(string(raw))})
				}
				if id != "" {
					tools[id] = &tool
				} else {
					spans = append(spans, tool) // unpaired: it has no result to wait for
				}
				if at.After(lastWork) {
					lastWork = at
				}
			case "toolResult":
				id := source.ToStr(block["callId"])
				var tool *otlp.Span
				if id != "" {
					tool = tools[id]
					delete(tools, id)
				}
				if tool == nil {
					continue // a result whose call is in an earlier window or was never drawn
				}
				tool.End = at
				if ms, ok := source.ToFloat(block["durationMs"]); ok && ms > 0 {
					tool.End = tool.Start.Add(time.Duration(ms) * time.Millisecond)
					setAttribute(tool, "timing.source", "recorded")
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "tool.duration_ms", Value: int64(ms)})
				}
				if status := source.ToStr(block["status"]); status != "" {
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "tool.status", Value: status})
				}
				if code, ok := source.ToFloat(block["exitCode"]); ok {
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "tool.exit_code", Value: int64(code)})
				}
				output := source.ToStr(block["content"])
				if source.IsFailedResult(block) {
					tool.Error = true
					tool.Status = contentText(output)
					failed = true
				}
				if content {
					tool.Attributes = append(tool.Attributes, otlp.Attribute{Key: "langfuse.observation.output", Value: contentText(output)})
				}
				spans = append(spans, *tool)
				if at.After(lastWork) {
					lastWork = at
				}
			case "event":
				kind := source.ToStr(block["kind"])
				event := otlp.Span{
					TraceID:  traceID,
					SpanID:   otlp.NewSpanID(append(base, strconv.Itoa(roundNo), "event", strconv.Itoa(len(spans)))...),
					ParentID: rootID, Name: source.FirstNonEmpty(kind, "event"), Kind: 1, Start: at, End: at,
					Attributes: commonAttributes(rec, roundNo, "event", "recorded"),
				}
				if kind != "" {
					event.Attributes = append(event.Attributes, otlp.Attribute{Key: "event.name", Value: kind})
				}
				if content {
					event.Attributes = append(event.Attributes, otlp.Attribute{Key: "langfuse.observation.output", Value: contentText(source.ToStr(block["content"]))})
				}
				spans = append(spans, event)
				if at.After(lastWork) {
					lastWork = at
				}
			}
		}
		if at.After(lastEvent) {
			lastEvent = at
		}
	}
	flush()
	return spans
}

// commonAttributes is what every span carries: the session it belongs to, its round and
// the provenance of its times
func commonAttributes(rec source.Record, round int, kind, timing string) []otlp.Attribute {
	attrs := []otlp.Attribute{
		{Key: "langfuse.session.id", Value: rec.SessionID},
		{Key: "langfuse.trace.name", Value: rec.ShortKey},
		{Key: "langfuse.observation.type", Value: kind},
		{Key: "session.source", Value: rec.Source},
		{Key: "round.index", Value: int64(round)},
		{Key: "timing.source", Value: timing},
	}
	if project := rec.ProjectName(); project != "" {
		attrs = append(attrs, otlp.Attribute{Key: "session.project", Value: project})
	}
	if rec.Model != "" && kind == "generation" {
		attrs = append(attrs, otlp.Attribute{Key: "gen_ai.response.model", Value: rec.Model})
	}
	return attrs
}

// usageAttributes maps a message's normalized counters onto the GenAI names
func usageAttributes(raw any) []otlp.Attribute {
	usage, ok := raw.(map[string]any)
	if !ok || len(usage) == 0 {
		return nil
	}
	pairs := []struct{ key, attr string }{
		{"inputTokens", "gen_ai.usage.input_tokens"},
		{"outputTokens", "gen_ai.usage.output_tokens"},
		{"cacheReadTokens", "gen_ai.usage.cache_read.input_tokens"},
		{"cacheWriteTokens", "gen_ai.usage.cache_creation.input_tokens"},
		{"reasoningTokens", "gen_ai.usage.reasoning.output_tokens"},
	}
	out := []otlp.Attribute{}
	for _, pair := range pairs {
		if n, ok := source.ToFloat(usage[pair.key]); ok && n > 0 {
			out = append(out, otlp.Attribute{Key: pair.attr, Value: int64(n)})
		}
	}
	return out
}

// contentText masks secrets and caps one body at 256 KiB, the same limit magpie uses
func contentText(text string) string {
	if text == "" {
		return ""
	}
	text = source.RedactSecrets(source.StripTerminalControls(text))
	const limit = 256 << 10
	if len(text) > limit {
		text = text[:limit] + "\n[cut]"
	}
	return text
}

func setAttribute(span *otlp.Span, key, value string) {
	for i := range span.Attributes {
		if span.Attributes[i].Key == key {
			span.Attributes[i].Value = value
			return
		}
	}
	span.Attributes = append(span.Attributes, otlp.Attribute{Key: key, Value: value})
}

// messageKey identifies a message within its round for a stable span id
func messageKey(m map[string]any, role string) string {
	if id := source.ToStr(m["id"]); id != "" {
		return id
	}
	if at := source.ToStr(m["timestamp"]); at != "" {
		return role + "@" + at
	}
	return role
}

// known reports whether this exact version of a session was exported already
func (s *otelState) known(key, updatedAt string) bool {
	if s == nil || s.Sessions == nil || updatedAt == "" {
		return false
	}
	session, ok := s.Sessions[key]
	return ok && session.UpdatedAt == updatedAt
}

// roundFP is the fingerprint last exported for one round, or empty
func (s *otelState) roundFP(key, round string) string {
	if s == nil || s.Sessions == nil {
		return ""
	}
	return s.Sessions[key].Rounds[round]
}

// mark records the version and the round fingerprints that were sent; a nil map updates
// only the version (the session changed without changing its rounds)
func (s *otelState) mark(key, updatedAt string, rounds map[string]string) {
	if s.Sessions == nil {
		s.Sessions = map[string]otelSession{}
	}
	session := s.Sessions[key]
	session.UpdatedAt = updatedAt
	session.At = time.Now().UTC().Format(time.RFC3339)
	if rounds != nil {
		if session.Rounds == nil || len(rounds) > 5000 {
			session.Rounds = map[string]string{}
		}
		for round, fp := range rounds {
			session.Rounds[round] = fp
		}
	}
	s.Sessions[key] = session
	if len(s.Sessions) > 20000 {
		s.Sessions = map[string]otelSession{}
	}
}

// loadState reads the cursor file; a missing or unreadable one simply starts empty
func (e *otelExporter) loadState() {
	data, err := os.ReadFile(e.cfg.statePath)
	if err != nil {
		return
	}
	var state otelState
	// Version 1 remembered only a session's version. Dropping it re-reads one window once
	// rather than guessing which rounds it had seen.
	if json.Unmarshal(data, &state) == nil && state.Version == 2 && state.Sessions != nil {
		e.state = &state
	}
}

// saveState writes the cursor atomically: a half-written file would forget what was sent
func (e *otelExporter) saveState() {
	data, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(e.cfg.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] otel: state dir: %v\n", err)
		return
	}
	tmp := e.cfg.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] otel: state: %v\n", err)
		return
	}
	if err := os.Rename(tmp, e.cfg.statePath); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] otel: state: %v\n", err)
	}
}

// defaultOTelStatePath is where the cursor lives unless --otel-state says otherwise
func defaultOTelStatePath() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "agent-session-query", "otel.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "agent-session-query", "otel.json")
}

// parseOTelSince reads --otel-since: "all" is every session the service can see; anything
// else uses the same grammar as the API's since (7d / 48h / a date)
func parseOTelSince(raw string) (time.Time, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "all") {
		return time.Time{}, nil
	}
	return parseSince(strings.TrimSpace(raw))
}

// parseOTelHeaders reads the comma-separated name=value list, percent-decoded, the way
// magpie's --otel-headers does — a space or comma in a value has to be encoded.
func parseOTelHeaders(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		name, value, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("--otel-headers wants name=value pairs separated by commas, got %q", part)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("--otel-headers: %q: %w", part, err)
		}
		out[strings.TrimSpace(name)] = decoded
	}
	return out, nil
}

var errNoEndpoint = errors.New("--otel-export needs --otel-endpoint")

// otelConfigFromFlags validates the flags into one config. An empty endpoint is "off" and
// legitimate; the caller decides whether a one-shot export or a server start needs one.
func otelConfigFromFlags(endpoint, headers, content, since string, every, settle time.Duration, statePath string, resend, dryRun, metrics bool) (otelConfig, error) {
	cfg := otelConfig{
		endpoint: strings.TrimSpace(endpoint),
		since:    time.Now().Add(-7 * 24 * time.Hour),
		every:    every,
		settle:   settle,
		resend:   resend,
		dryRun:   dryRun,
		metrics:  metrics,
	}
	if cfg.endpoint != "" {
		u, err := url.Parse(cfg.endpoint)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return cfg, fmt.Errorf("--otel-endpoint must be an http or https base URL without credentials, query or fragment")
		}
	}
	parsedHeaders, err := parseOTelHeaders(headers)
	if err != nil {
		return cfg, err
	}
	cfg.headers = parsedHeaders
	switch strings.ToLower(strings.TrimSpace(content)) {
	case "", "none", "metadata":
		cfg.content = false
	case "io", "bodies", "all":
		cfg.content = true
	default:
		return cfg, fmt.Errorf("--otel-content must be none or io, got %q", content)
	}
	if strings.TrimSpace(since) != "" {
		at, err := parseOTelSince(since)
		if err != nil {
			return cfg, err
		}
		cfg.since = at
	}
	if cfg.every < 0 || cfg.settle < 0 {
		return cfg, errors.New("--otel-every and --otel-settle cannot be negative")
	}
	switch strings.ToLower(strings.TrimSpace(statePath)) {
	case "off", "none":
		cfg.statePath = ""
	default:
		cfg.statePath = statePath
		if cfg.statePath == "" {
			cfg.statePath = defaultOTelStatePath()
		}
	}
	return cfg, nil
}

// runOTelOnce is --otel-export: one pass over the window, then exit
func runOTelOnce(api *SessionQueryAPI, cfg otelConfig) int {
	exporter := newOTelExporter(api, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sessions, spans := exporter.exportOnce(ctx, cfg.since, time.Time{})
	if cfg.dryRun {
		fmt.Fprintf(os.Stderr, "Would export %d spans from %d sessions\n", spans, sessions)
		return 0
	}
	fmt.Printf("Exported %d spans from %d sessions\n", spans, sessions)
	return 0
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func contentLabel(on bool) string {
	if on {
		return "with"
	}
	return "metadata only, no"
}

func sinceLabel(t time.Time) string {
	if t.IsZero() {
		return "all"
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
