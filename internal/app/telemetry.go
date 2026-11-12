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
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// priorFormat is the span format of a cursor written by an earlier export, when it is
	// older than otelSpanFormat; 0 when the cursor is current or new
	priorFormat int
}

// otelSpanFormat is the shape of the spans a transcript becomes. It moves when the same
// transcript would now be sent as spans with other ids or other contents: format 2 folds
// the rows of one response into one generation, where format 1 (a cursor with no format)
// drew a generation per row, each repeating Claude Code's usage; format 3 hangs a subagent
// beneath the call that started it, in its parent's trace, where format 2 gave it a trace
// of its own; format 4 leaves out the rows a fork copied from its parent, so the fork's
// own rounds are numbered from 1, where format 3 sent the copy as its first rounds. A
// backend that updates by id keeps what it got before beside the new spans, so a cursor of
// an earlier format is reported once, with the way to clean up (docs/telemetry.md,
// "Upgrading").
const otelSpanFormat = 4

// otelState remembers, per session, the version that was exported. A single UpdatedAt is
// enough: a session's content only moves when its newest row does, and a session that grew
// is exported again under the same deterministic span ids.
type otelState struct {
	Version int    `json:"version"`
	Format  int    `json:"format,omitempty"` // otelSpanFormat when written; absent is 1
	LastRun string `json:"lastRun,omitempty"`
	// CheckedUntil is, per source (its mode: claude, claude:box2), where the last pass that
	// walked that source to its end began — less --otel-settle, and never past a session it
	// failed to send. Everything in that pass's window updated before this instant was seen;
	// what changed after it may not have been. The next pass reaches back to it when
	// --otel-since alone would not (see windowFrom), so exports run weeks apart still see the
	// sessions that changed in between and then went quiet. Per source, because a source is
	// often exported on its own (--mode pi): one mark for all would let one source's run
	// stand for another's.
	CheckedUntil map[string]string      `json:"checkedUntil,omitempty"`
	Sessions     map[string]otelSession `json:"sessions,omitempty"`
}

// otelSession is how far one session has been exported: its version for the fast skip,
// and a fingerprint per round so a session that grew sends only the rounds that changed.
type otelSession struct {
	UpdatedAt string            `json:"updatedAt,omitempty"`
	At        string            `json:"at,omitempty"`
	Rounds    map[string]string `json:"rounds,omitempty"`
}

func newOTelExporter(api *SessionQueryAPI, cfg otelConfig) *otelExporter {
	otlp.ServiceVersion = buildVersion // the release, not "dev", on everything sent
	e := &otelExporter{
		api:    api,
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second}, // a batch is up to 8 MiB
		state:  &otelState{Version: 2, Format: otelSpanFormat, Sessions: map[string]otelSession{}},
	}
	if cfg.statePath != "" {
		e.loadState()
	}
	if e.priorFormat > 0 {
		fmt.Fprintf(os.Stderr, "[WARN] otel: the cursor %s was written by an earlier export (span format %d, now %d). "+
			"The spans have changed shape since: a session it covers is sent again in full when it next changes, and "+
			"a backend that updates by id keeps what it got before beside the new spans. Delete the traces exported "+
			"before (or use a new project), then run --otel-export --otel-resend once; see \"Upgrading\" in "+
			"docs/telemetry.md\n",
			e.cfg.statePath, e.priorFormat, otelSpanFormat)
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

// windowFrom is where a pass's window over one source starts: --otel-since, or — when the
// cursor's checks of that source stop earlier — where they stop (reach). --otel-since is the
// window of a first pass; a cursor that has been kept knows where it last looked, and a
// session updated after that and quiet since has to be read however long ago it was. A run
// with --otel-resend asked for its window as given, and a cursor of an earlier format is
// being replaced: both keep --otel-since.
func (e *otelExporter) windowFrom(since time.Time, mode string) time.Time {
	if since.IsZero() || e.cfg.resend || e.priorFormat > 0 {
		return since
	}
	if until, ok := e.state.reach(mode); ok && until.Before(since) {
		return until
	}
	return since
}

// reach is where the cursor's checks of a source stop: its mark, when a pass has set one.
// A cursor written before the marks existed knows only when it last sent each session; a
// source it holds sessions of reaches a day before the latest of those (a session written
// during that pass may be older than its end), or before lastRun when no session says. A
// source the cursor holds nothing of has not been exported: its first pass takes
// --otel-since.
func (s *otelState) reach(mode string) (time.Time, bool) {
	if mark, ok := s.CheckedUntil[mode]; ok {
		return source.ParseTimestamp(mark)
	}
	var latest time.Time
	held := false
	for key, session := range s.Sessions {
		if !strings.HasPrefix(key, mode+"|") {
			continue
		}
		held = true
		if at, ok := source.ParseTimestamp(session.At); ok && at.After(latest) {
			latest = at
		}
	}
	if !held {
		return time.Time{}, false
	}
	if latest.IsZero() {
		last, ok := source.ParseTimestamp(s.LastRun)
		if !ok {
			return time.Time{}, false
		}
		latest = last
	}
	return latest.Add(-otelLegacyReachMargin), true
}

// otelLegacyReachMargin is how far before its last send a source of a cursor without marks
// is read: generous, since a session the cursor already holds costs a comparison
const otelLegacyReachMargin = 24 * time.Hour

// exportOnce walks the sessions updated in the window and exports the ones the state does
// not already know at their current version. settledBefore, when set, leaves a session
// that is still being written for a later tick.
func (e *otelExporter) exportOnce(ctx context.Context, since, settledBefore time.Time) (sessions, spans int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	started := time.Now()
	acc := newOTelMetricAcc()
	// A stopped pass (Ctrl-C, a server shutting down) still saves the cursor below: the
	// sessions it sent are not sent again by the next run
walk:
	for _, src := range e.api.activeSources() {
		links := newOTelLinks(e, src)
		mode, walked := src.Mode(), time.Now()
		from := e.windowFrom(since, mode)
		var retry time.Time // the earliest update among this source's sessions that failed to send
		for _, rec := range e.api.recordsOf(src) {
			if ctx.Err() != nil {
				break walk
			}
			if rec.Archived || !withinWindow(rec.UpdatedAt, from, time.Time{}) {
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
			if anchor, ok := links.anchorOf(rec, 0); ok {
				nestSubagent(built, rec, anchor) // beneath the call that started it
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
					retry = earlier(retry, rec.UpdatedAt)
					continue
				}
				fmt.Printf("=== %s %s: %d spans ===\n%s\n", rec.Source, rec.SessionID, len(kept), body)
			} else if err := e.send(ctx, kept); err != nil {
				fmt.Fprintf(os.Stderr, "[WARN] otel: %s: %v\n", key, err)
				retry = earlier(retry, rec.UpdatedAt)
				continue // a failed session is not marked: the next run retries it
			}
			e.state.mark(key, rec.UpdatedAt, changed)
			sessions++
			spans += len(kept)
			if e.cfg.metrics {
				acc.add(kept)
			}
		}
		if ctx.Err() == nil && !e.cfg.dryRun {
			e.state.checked(mode, from, walked, settledBefore, e.cfg.settle, retry) // walked to its end
		}
	}
	if e.cfg.statePath != "" && !e.cfg.dryRun {
		e.state.LastRun = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		e.saveState()
	}
	if e.cfg.metrics && !e.cfg.dryRun {
		if metrics := acc.metrics(); len(metrics) > 0 {
			// A stopped pass still reports what it sent — the stop was for the walk. It gets a
			// short deadline of its own, so a shutdown waits seconds for it, not the client's minute.
			mctx := ctx
			if ctx.Err() != nil {
				var cancel context.CancelFunc
				mctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
			}
			if err := otlp.SendMetrics(mctx, e.client, e.cfg.endpoint, e.cfg.headers, metrics, started); err != nil {
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
		case "span", "agent": // a round's root; a subagent's, nested under its call, is an agent
			if !strings.HasPrefix(span.Name, "round ") && !strings.HasPrefix(span.Name, "subagent ") {
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

// otelInferredCap bounds an inferred generation start. A model span runs from the event
// before it to its completion; when that gap is longer than this, the CLI sat idle or the
// session was resumed days later, and the gap is not a model call — the span is drawn at
// its completion instead.
const otelInferredCap = 30 * time.Minute

// otelSyntheticModel is the model Claude Code writes on a row it made itself — a resumed
// session's "No response requested.", an error it reports in the assistant's place. No
// model answered it: it is not a generation, and it must not stretch a round to the day
// the session was resumed.
const otelSyntheticModel = "<synthetic>"

// genBuild is one generation while its round is read: a response that arrives as several
// rows (Claude Code writes a row per content block) folds into it. Its input is what reached
// the model since its previous call — the ask, then the tool results — and its output the
// response, both as chat messages a backend renders as a conversation.
type genBuild struct {
	span     otlp.Span
	input    []map[string]any
	texts    []string
	calls    []map[string]any // OpenAI-style tool_calls: id, type, function{name, arguments}
	hasUsage bool
}

// otelPieceLimit caps one tool result or one set of tool arguments inside a generation's
// chat messages; the ask and the reply keep contentText's 256 KiB
const otelPieceLimit = 32 << 10

// contentPiece masks and cuts one piece of a chat message
func contentPiece(text string) string {
	text = contentText(text)
	if len(text) > otelPieceLimit {
		text = text[:otelPieceLimit] + "\n[cut]"
	}
	return text
}

// chatJSON renders chat messages for langfuse.observation.input / output
func chatJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// buildSessionSpans renders one session as traces: one trace per round, a root span for
// the round, and generation / tool / event spans beneath it. Every source goes through
// this one function — the differences live in the messages, not in the exporter.
//
// One generation per model response: rows that share a response id (the field the sources
// set when a CLI splits a response) are one generation, with the usage counted once. A
// round whose tool calls failed but which went on is a warning; only a round that ended on
// a failed call is an error.
func buildSessionSpans(rec source.Record, messages []map[string]any, content bool) []otlp.Span {
	base := []string{rec.Source, rec.SessionID}
	var spans []otlp.Span
	roundNo := 0
	var root *otlp.Span
	var traceID, rootID, traceName string
	var ask, lastAssistant string
	var start, lastWork, lastEvent time.Time
	var failures int
	var endedInFailure bool
	tools := map[string]*otlp.Span{}
	var gens []*genBuild
	byResponse := map[string]*genBuild{}
	var pending []map[string]any // what reached the model since its last call: the next generation's input
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
		switch {
		case failures > 0 && endedInFailure:
			root.Error, root.Status = true, fmt.Sprintf("%d tool call(s) failed; the round ended on a failure", failures)
		case failures > 0:
			root.Attributes = append(root.Attributes,
				otlp.Attribute{Key: "langfuse.observation.level", Value: "WARNING"},
				otlp.Attribute{Key: "langfuse.observation.status_message", Value: fmt.Sprintf("%d tool call(s) failed; the round went on", failures)})
		}
		if content {
			root.Attributes = append(root.Attributes, otlp.Attribute{Key: "langfuse.observation.input", Value: contentText(ask)})
			root.Attributes = append(root.Attributes, otlp.Attribute{Key: "langfuse.observation.output", Value: contentText(lastAssistant)})
		}
		root.Attributes = append(root.Attributes, otlp.Attribute{Key: "langfuse.trace.tags", Value: roundTags(rec, failures, endedInFailure)})
		spans = append(spans, *root)
		for _, g := range gens {
			if content {
				reply := map[string]any{"role": "assistant", "content": contentText(strings.Join(g.texts, "\n\n"))}
				if len(g.calls) > 0 {
					reply["tool_calls"] = g.calls
				}
				if len(g.input) > 0 {
					g.span.Attributes = append(g.span.Attributes, otlp.Attribute{Key: "langfuse.observation.input", Value: chatJSON(g.input)})
				}
				g.span.Attributes = append(g.span.Attributes, otlp.Attribute{Key: "langfuse.observation.output", Value: chatJSON(reply)})
			}
			spans = append(spans, g.span)
		}
		ids := make([]string, 0, len(tools))
		for id := range tools {
			ids = append(ids, id)
		}
		sort.Strings(ids) // the same round renders in the same order: its fingerprint holds
		for _, id := range ids {
			tool := tools[id]
			if tool.End.Before(tool.Start) {
				tool.End = end
			}
			spans = append(spans, *tool)
			delete(tools, id)
		}
		root, gens, byResponse, pending = nil, nil, map[string]*genBuild{}, nil
	}

	for _, m := range messages {
		if source.Truthy(m["inherited"]) {
			continue // a fork's copy of its parent's rows, exported with the parent
		}
		role := source.ToStr(m["role"])
		at, ok := source.ParseTimestamp(source.ToStr(m["timestamp"]))
		if !ok {
			at = lastEvent
		}
		model := source.ToStr(m["model"])
		if role == "assistant" && model == otelSyntheticModel {
			continue // no model answered it, and its time is not the round's
		}
		starting := isRoundStart(m)
		if starting {
			flush()
			roundNo++
			ask, lastAssistant = roundAsk(strings.TrimSpace(messageText(m["content"]))), ""
			traceName = rec.ShortKey
			if content {
				traceName = roundTitle(ask, rec.ShortKey) // the ask names the round; off, it stays home
			}
			round := strconv.Itoa(roundNo)
			traceID = otlp.NewID(append(base, round, "trace")...)
			rootID = otlp.NewSpanID(append(base, round, "root")...)
			start, lastWork, lastEvent = at, at, at
			failures, endedInFailure = 0, false
			pending = []map[string]any{{"role": "user", "content": contentText(ask)}}
			tools = map[string]*otlp.Span{}
			root = &otlp.Span{
				TraceID: traceID, SpanID: rootID, Name: "round " + round, Kind: 1, Start: at, End: at,
				Attributes: commonAttributes(rec, roundNo, "span", "derived", traceName),
			}
			if rec.ShortKey != "" {
				root.Attributes = append(root.Attributes, otlp.Attribute{Key: "session.title", Value: rec.ShortKey})
			}
		}
		if root == nil {
			lastEvent = at
			continue
		}
		// Words the person typed mid-round (a queued prompt) reach the model with its next call
		if role == "user" && !starting && !source.Truthy(m["injected"]) {
			if text := strings.TrimSpace(messageText(m["content"])); text != "" {
				pending = append(pending, map[string]any{"role": "user", "content": contentText(text)})
			}
		}
		var cur *genBuild
		if role == "assistant" {
			text := strings.TrimSpace(messageText(m["content"]))
			response := source.ToStr(m["response"])
			if response != "" {
				cur = byResponse[response]
			}
			// A source that measured the call (Pi's durationMs, from the message's creation)
			// gives the span outright; the rest only record completion, so the start is the
			// event before it, within the cap
			measured, _ := source.ToFloat(m["durationMs"])
			if cur == nil {
				genStart, genEnd, timing := lastEvent, at, "inferred"
				if genStart.IsZero() || genStart.After(at) || at.Sub(genStart) > otelInferredCap {
					genStart = at
				}
				if measured > 0 {
					genStart, genEnd, timing = at, at.Add(time.Duration(measured)*time.Millisecond), "recorded"
				}
				var firstToken time.Time // when the response began to arrive, where the source says
				if ms, _ := source.ToFloat(m["firstTokenMs"]); ms > 0 && measured > 0 {
					firstToken = at.Add(time.Duration(ms) * time.Millisecond)
				}
				// Keyed by the response's first row, as each row was before rows folded: a
				// response written as one row keeps the id it was first exported under
				key := messageKey(m, role)
				cur = &genBuild{span: otlp.Span{
					TraceID:  traceID,
					SpanID:   otlp.NewSpanID(append(base, strconv.Itoa(roundNo), "msg", key)...),
					ParentID: rootID, Name: "model", Kind: 1, Start: genStart, End: genEnd,
					Attributes: commonAttributes(rec, roundNo, "generation", timing, traceName),
				}}
				// The response's own model; the session's last model only when the row has none
				if name := source.FirstNonEmpty(model, lastModel); name != "" {
					cur.span.Name = "model " + name
					cur.span.Attributes = append(cur.span.Attributes,
						otlp.Attribute{Key: "gen_ai.request.model", Value: name},
						otlp.Attribute{Key: "gen_ai.response.model", Value: name})
				}
				cur.span.Attributes = append(cur.span.Attributes,
					otlp.Attribute{Key: "gen_ai.operation.name", Value: "chat"})
				if !firstToken.IsZero() {
					cur.span.Attributes = append(cur.span.Attributes, otlp.Attribute{
						Key: "langfuse.observation.completion_start_time", Value: firstToken.UTC().Format(time.RFC3339Nano)})
				}
				cur.input, pending = pending, nil // a later row of the same response takes nothing
				gens = append(gens, cur)
				if response != "" {
					byResponse[response] = cur
				}
			} else if at.After(cur.span.End) {
				cur.span.End = at // a later block of the same response
			}
			if model != "" {
				lastModel = model
			}
			if !cur.hasUsage {
				if usage := usageAttributes(rec.Source, m["usage"]); len(usage) > 0 {
					cur.span.Attributes = append(cur.span.Attributes, usage...)
					cur.hasUsage = true
				}
			}
			if text != "" {
				cur.texts = append(cur.texts, text)
				lastAssistant = text
				endedInFailure = false // the agent spoke after the failure
			}
			if at.After(lastWork) {
				lastWork = at
			}
			if cur.span.End.After(lastWork) {
				lastWork = cur.span.End // a measured call ends after the row's own time
			}
		}
		for _, block := range contentBlocks(m["content"]) {
			switch source.ToStr(block["type"]) {
			case "toolCall":
				name, id := source.ToStr(block["name"]), source.ToStr(block["id"])
				if cur != nil {
					args, _ := json.Marshal(block["arguments"])
					cur.calls = append(cur.calls, map[string]any{"id": id, "type": "function",
						"function": map[string]any{"name": name, "arguments": contentPiece(string(args))}})
				}
				key := id
				if key == "" {
					key = "call:" + strconv.Itoa(len(spans))
				}
				tool := otlp.Span{
					TraceID: traceID, SpanID: otlp.NewSpanID(append(base, strconv.Itoa(roundNo), "tool", key)...),
					ParentID: rootID, Name: source.FirstNonEmpty(name, "tool"), Kind: 1, Start: at, End: at,
					Attributes: commonAttributes(rec, roundNo, "tool", "derived", traceName),
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
				pending = append(pending, map[string]any{"role": "tool", "tool_call_id": id,
					"name": source.ToStr(block["toolName"]), "content": contentPiece(output)})
				if source.IsFailedResult(block) {
					tool.Error = true
					tool.Status = contentText(output)
					failures++
					endedInFailure = true
				} else {
					endedInFailure = false
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
					Attributes: commonAttributes(rec, roundNo, "event", "recorded", traceName),
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

// Subagents in their parent's trace.
//
// A Claude Code subagent is a transcript of its own and is exported as a session of its own,
// but it ran inside one of its parent's calls — an Agent call, or the Workflow call that
// started a workflow's agents — and drawn as a trace of its own it was cut off from that
// call: the parent round showed a call that took twenty minutes and nothing under it. Where
// the call is known, a subagent's rounds hang beneath the call's span in the parent round's
// trace. Measured here: 39 of 228 subagent transcripts name their call (toolUseId), and the
// 184 workflow agents are found through the run their Workflow call's result names; a team's
// members name neither and keep a trace of their own.

// otelAnchor is where a subagent's rounds hang: the trace of the parent round that made the
// call, the call's span, and the top-level session the trace belongs to
type otelAnchor struct {
	traceID, spanID, sessionID string
}

// otelParentCalls is what one parent transcript offers its subagents: each call's span,
// keyed by call id, and the call that started each workflow run
type otelParentCalls struct {
	calls map[string]otelAnchor
	runs  map[string]string
}

// otelLinks resolves the subagents of one source within one pass, reading each parent once
type otelLinks struct {
	e        *otelExporter
	src      source.SessionSource
	mains    map[string]source.Record // session id -> session
	agents   map[string]source.Record // session id + "|" + agent id -> subagent
	parents  map[string]*otelParentCalls
	resolved map[string]otelAnchorResult
}

type otelAnchorResult struct {
	anchor otelAnchor
	ok     bool
}

func newOTelLinks(e *otelExporter, src source.SessionSource) *otelLinks {
	return &otelLinks{e: e, src: src, parents: map[string]*otelParentCalls{}, resolved: map[string]otelAnchorResult{}}
}

// parentOf finds the transcript a subagent was started from: the agent that spawned it, or
// the session's main thread
func (l *otelLinks) parentOf(rec source.Record) (source.Record, bool) {
	if l.mains == nil {
		l.mains, l.agents = map[string]source.Record{}, map[string]source.Record{}
		for _, r := range l.e.api.recordsOf(l.src) {
			if r.IsSubagent() {
				l.agents[r.ParentSessionID+"|"+r.SessionID] = r
			} else {
				l.mains[r.SessionID] = r
			}
		}
	}
	if rec.ParentAgentID != "" {
		p, ok := l.agents[rec.ParentSessionID+"|"+rec.ParentAgentID]
		return p, ok
	}
	p, ok := l.mains[rec.ParentSessionID]
	return p, ok
}

// callsOf reads a parent once: its spans are built the way its own export builds them (the
// ids do not depend on whether bodies are sent), so a call's span here is the one sent
func (l *otelLinks) callsOf(parent source.Record) *otelParentCalls {
	if pc, ok := l.parents[parent.Key]; ok {
		return pc
	}
	pc := &otelParentCalls{calls: map[string]otelAnchor{}, runs: map[string]string{}}
	l.parents[parent.Key] = pc
	messages := l.e.allMessages(l.src, parent)
	for _, span := range buildSessionSpans(parent, messages, false) {
		if id := source.ToStr(spanAttribute(span, "gen_ai.tool.call.id")); id != "" {
			pc.calls[id] = otelAnchor{traceID: span.TraceID, spanID: span.SpanID}
		}
	}
	for _, m := range messages {
		for _, block := range contentBlocks(m["content"]) {
			run, call := source.ToStr(block["workflowRunId"]), source.ToStr(block["callId"])
			if run != "" && call != "" && pc.runs[run] == "" {
				pc.runs[run] = call // the first call to name a run is the one that started it
			}
		}
	}
	return pc
}

// anchorOf is where a subagent's rounds hang, when its call can be found. One spawned by
// another subagent hangs beneath that agent's call, in whichever trace that agent landed.
func (l *otelLinks) anchorOf(rec source.Record, depth int) (otelAnchor, bool) {
	if !rec.IsSubagent() || depth > 8 {
		return otelAnchor{}, false
	}
	if r, ok := l.resolved[rec.Key]; ok {
		return r.anchor, r.ok
	}
	l.resolved[rec.Key] = otelAnchorResult{} // a loop in the links resolves to nothing
	parent, ok := l.parentOf(rec)
	if !ok || parent.Archived {
		return otelAnchor{}, false
	}
	pc := l.callsOf(parent)
	call := rec.ParentToolUseID
	if call == "" && rec.WorkflowRunID != "" {
		call = pc.runs[rec.WorkflowRunID]
	}
	a, ok := pc.calls[call]
	if call == "" || !ok {
		return otelAnchor{}, false
	}
	a.sessionID = parent.SessionID
	if pa, ok := l.anchorOf(parent, depth+1); ok {
		a.traceID, a.sessionID = pa.traceID, pa.sessionID
	}
	l.resolved[rec.Key] = otelAnchorResult{anchor: a, ok: true}
	return a, true
}

// nestSubagent moves a subagent's spans beneath the call that started it. Every span takes
// the parent round's trace and the top-level session; a round's root becomes an agent
// observation under the call. The trace's name and tags belong to the parent's round, so
// the subagent's spans leave them out.
func nestSubagent(spans []otlp.Span, rec source.Record, a otelAnchor) {
	kind := source.FirstNonEmpty(rec.AgentType, "agent")
	for i := range spans {
		s := &spans[i]
		s.TraceID = a.traceID
		root := s.ParentID == ""
		if root {
			s.ParentID = a.spanID
			s.Name = "subagent " + kind
			if n, _ := source.ToFloat(spanAttribute(*s, "round.index")); n > 1 {
				s.Name += " · round " + strconv.Itoa(int(n))
			}
		}
		attrs := make([]otlp.Attribute, 0, len(s.Attributes)+2)
		for _, attr := range s.Attributes {
			switch attr.Key {
			case "langfuse.trace.name", "langfuse.trace.tags":
				continue
			case "langfuse.session.id":
				attr.Value = a.sessionID
			case "langfuse.observation.type":
				if root {
					attr.Value = "agent"
				}
			}
			attrs = append(attrs, attr)
		}
		attrs = append(attrs, otlp.Attribute{Key: "subagent.session.id", Value: rec.SessionID})
		if root && rec.AgentType != "" {
			attrs = append(attrs, otlp.Attribute{Key: "subagent.type", Value: rec.AgentType})
		}
		s.Attributes = attrs
	}
}

// roundTags are a round's trace tags, for filtering: where it came from (the source, the
// project, a subagent's own transcript) and how it went (a failed tool call, a round that
// ended on one)
func roundTags(rec source.Record, failures int, endedInFailure bool) []string {
	tags := []string{rec.Source}
	if project := filepath.Base(strings.TrimRight(rec.ProjectName(), "/\\")); project != "" && project != "." && project != "/" {
		tags = append(tags, "project:"+project) // the folder's name: a tag is a label, not a path
	}
	if rec.IsSubagent() {
		tags = append(tags, "subagent")
	}
	if failures > 0 {
		tags = append(tags, "tool-failure")
	}
	if endedInFailure {
		tags = append(tags, "failed")
	}
	return tags
}

// roundTitle names a round's trace by its ask: the first line, harness blocks stripped,
// secrets masked, cut to 100 characters. A round with no words (an image alone) takes the
// session's title.
func roundTitle(ask, session string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(source.StripHarnessBlocks(ask)), "\n")
	line = strings.TrimSpace(source.RedactSecrets(line))
	if line == "" {
		return session
	}
	if runes := []rune(line); len(runes) > 100 {
		line = string(runes[:100]) + "…"
	}
	return line
}

// commonAttributes is what every span carries: the session it belongs to, its round, the
// round's trace name and the provenance of its times
func commonAttributes(rec source.Record, round int, kind, timing, traceName string) []otlp.Attribute {
	attrs := []otlp.Attribute{
		{Key: "langfuse.session.id", Value: rec.SessionID},
		{Key: "langfuse.trace.name", Value: traceName},
		{Key: "langfuse.observation.type", Value: kind},
		{Key: "session.source", Value: rec.Source},
		{Key: "round.index", Value: int64(round)},
		{Key: "timing.source", Value: timing},
	}
	if project := rec.ProjectName(); project != "" {
		attrs = append(attrs, otlp.Attribute{Key: "session.project", Value: project})
	}
	return attrs
}

// usageShape is how a source's counters relate. The GenAI conventions count the cached input,
// read or written, inside gen_ai.usage.input_tokens and the reasoning inside
// gen_ai.usage.output_tokens — OpenAI's shape, and Codex's — and a backend reads them that
// way: Langfuse takes the cache back out of the input and the reasoning out of the output.
// A source that records them apart, sent as it records them, had them taken out a second
// time and floored at zero: on this machine's history Langfuse kept 2.67M of OpenCode's 5.70M
// input tokens and 0.47M of its 1.14M output, 0.21M of Pi's 0.86M input, and an input of 0 on
// every Claude Code generation. Such a source's parts are added back before they are sent.
type usageShape struct {
	cacheApart     bool // input counts only what was not cached
	reasoningApart bool // output counts only what was not reasoning
}

// usageShapes lists the sources that keep a part apart; the rest — Codex, Antigravity, and
// Hermes, which records no usage per message — are sent as recorded. Apart means: Anthropic's
// usage (Claude Code); the pi-ai shape Pi and OpenClaw record, input + output + cacheRead +
// cacheWrite = totalTokens; Grok's turn counters and dsh's message usage, the same sum;
// OpenCode, whose reasoning is reported beside an output it is not part of (and outweighs
// it, 1.70M to 1.14M here); Gemini, whose candidatesTokenCount leaves the thoughts out.
var usageShapes = map[string]usageShape{
	"claude":   {cacheApart: true},
	"pi":       {cacheApart: true},
	"openclaw": {cacheApart: true},
	"grok":     {cacheApart: true},
	"dsh":      {cacheApart: true},
	"opencode": {cacheApart: true, reasoningApart: true},
	"gemini":   {reasoningApart: true},
}

// usageAttributes maps a message's normalized counters onto the GenAI names, in the GenAI
// shape (see usageShape). src is the record's source; a labeled instance (claude:box2) has
// the shape of its mode.
func usageAttributes(src string, raw any) []otlp.Attribute {
	usage, ok := raw.(map[string]any)
	if !ok || len(usage) == 0 {
		return nil
	}
	count := func(key string) int64 {
		n, _ := source.ToFloat(usage[key])
		return int64(max(n, 0))
	}
	input, output := count("inputTokens"), count("outputTokens")
	cacheRead, cacheWrite, reasoning := count("cacheReadTokens"), count("cacheWriteTokens"), count("reasoningTokens")
	mode, _, _ := strings.Cut(src, ":")
	shape := usageShapes[mode]
	if shape.cacheApart {
		input += cacheRead + cacheWrite
	}
	if shape.reasoningApart {
		output += reasoning
	}
	out := []otlp.Attribute{}
	for _, pair := range []struct {
		attr string
		n    int64
	}{
		{"gen_ai.usage.input_tokens", input},
		{"gen_ai.usage.output_tokens", output},
		{"gen_ai.usage.cache_read.input_tokens", cacheRead},
		{"gen_ai.usage.cache_creation.input_tokens", cacheWrite},
		{"gen_ai.usage.reasoning.output_tokens", reasoning},
	} {
		if pair.n > 0 {
			out = append(out, otlp.Attribute{Key: pair.attr, Value: pair.n})
		}
	}
	// A cost the CLI worked out (Pi's usage.cost.total, OpenCode's cost) goes as is; a zero
	// is a CLI that had no price for the model, and is left for the backend to price
	if cost, ok := source.ToFloat(usage["estimatedCostUsd"]); ok && cost > 0 {
		out = append(out, otlp.Attribute{Key: "langfuse.observation.cost_details", Value: chatJSON(map[string]any{"total": cost})})
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

// checked moves a source's mark after a pass walked it to its end (a stopped pass has not
// seen the sessions it did not reach, and moves nothing). The new mark is where the walk
// began, less settle: a session written while a list was being read, or one a tick left to
// settle, was updated after it. A session the pass failed to send holds the mark at its own
// update, so the next pass still reaches it. A window that began after where the cursor's
// checks stopped (a resend over a shorter window) left the stretch between them unread: the
// mark stays.
func (s *otelState) checked(mode string, from, started, settledBefore time.Time, settle time.Duration, retry time.Time) {
	if prev, ok := s.reach(mode); ok && !from.IsZero() && from.After(prev) {
		return
	}
	until := started.Add(-settle)
	if !settledBefore.IsZero() && settledBefore.Before(until) {
		until = settledBefore
	}
	if !retry.IsZero() && retry.Before(until) {
		until = retry
	}
	if s.CheckedUntil == nil {
		s.CheckedUntil = map[string]string{}
	}
	s.CheckedUntil[mode] = until.UTC().Format(time.RFC3339)
}

// earlier is the earlier of at and a session's update time; a time that will not parse is
// in every window already (withinWindow) and changes nothing
func earlier(at time.Time, updatedAt string) time.Time {
	t, ok := source.ParseTimestamp(updatedAt)
	if !ok {
		return at
	}
	if at.IsZero() || t.Before(at) {
		return t.Add(-time.Second) // a window takes what is at its start; a second spare
	}
	return at
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
	// A cursor with no session yet (every pass so far found nothing to send) is still one:
	// its marks say how far back the sources were looked at
	if json.Unmarshal(data, &state) == nil && state.Version == 2 {
		if state.Sessions == nil {
			state.Sessions = map[string]otelSession{}
		}
		if state.Format < otelSpanFormat && len(state.Sessions) > 0 {
			e.priorFormat = max(state.Format, 1)
		}
		state.Format = otelSpanFormat // what this run sends, and what the saved cursor says
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
	// No deadline: a whole-history backfill takes as long as it takes (a fixed ten minutes
	// cut one off and, with it, the cursor). Ctrl-C stops it, and what was sent is kept.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sessions, spans := exporter.exportOnce(ctx, cfg.since, time.Time{})
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "[WARN] otel: stopped early; the %d sessions sent are in the cursor and the next run goes on from there\n", sessions)
	}
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
