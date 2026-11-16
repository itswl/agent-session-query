// Package otel sends the record to an OpenTelemetry collector: every turn a trace.
//
// The wire is OTLP over HTTP in its JSON encoding — no protobuf dependency, one request
// shape every collector takes. The exporter is small on purpose: the decisions worth
// arguing about are in what a turn becomes (spans.go), not in how it travels.
package otel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
)

// Attr is one span attribute. A value is a string, an int64, a float64, a bool or a
// []string; anything else, and anything empty, is not sent.
type Attr struct {
	Key   string
	Value any
}

// Span is one OTLP span. Its times are the transcript's: the format carries explicit
// bounds, so a span drawn after the fact from a record is as good as one timed live.
type Span struct {
	TraceID  string // 32 hex characters
	SpanID   string // 16
	ParentID string // empty for a trace's root

	Name       string
	Kind       int // 1 internal, 3 client
	Start, End time.Time
	Attrs      []Attr
	Error      bool
	Status     string // what went wrong, when Error
}

const (
	kindInternal = 1
	kindClient   = 3
)

func digest(parts ...string) [32]byte {
	return sha256.Sum256([]byte(strings.Join(parts, "\x00")))
}

// traceID and spanID derive ids from what a span is, not from when it was sent: the same
// turn sends the same ids every time, so a turn that grew and is sent again replaces
// itself on a backend that keys by id instead of appearing twice.
func traceID(parts ...string) string {
	sum := digest(parts...)
	return hex.EncodeToString(sum[:16])
}

func spanID(parts ...string) string {
	sum := digest(parts...)
	return hex.EncodeToString(sum[:8])
}

// tracesURL is where spans are posted: the endpoint as given when it already names the
// traces path, and with /v1/traces appended when it is a collector's base address.
func tracesURL(endpoint string) string {
	target := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(target, "/v1/traces") {
		target += "/v1/traces"
	}
	return target
}

// send posts spans. It tries a second time after a server error or a failure to
// connect; a payload the collector refused is not offered again.
func send(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, spans []Span) error {
	body, err := encode(spans)
	if err != nil {
		return err
	}
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tracesURL(endpoint), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "asq/"+app.Version)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		said, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		last = fmt.Errorf("the collector answered %s: %s", resp.Status, strings.TrimSpace(string(said)))
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
			return last
		}
	}
	return last
}

// encode renders spans as an OTLP/HTTP JSON request body.
func encode(spans []Span) ([]byte, error) {
	encoded := make([]any, 0, len(spans))
	for _, s := range spans {
		end := s.End
		if end.Before(s.Start) {
			end = s.Start
		}
		span := map[string]any{
			"traceId":           s.TraceID,
			"spanId":            s.SpanID,
			"name":              s.Name,
			"kind":              s.Kind,
			"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(end.UnixNano(), 10),
			"attributes":        encodeAttrs(s.Attrs),
		}
		if s.ParentID != "" {
			span["parentSpanId"] = s.ParentID
		}
		if s.Error {
			status := map[string]any{"code": 2}
			if s.Status != "" {
				status["message"] = s.Status
			}
			span["status"] = status
		}
		encoded = append(encoded, span)
	}
	return json.Marshal(map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": encodeAttrs([]Attr{
				{"service.name", "asq"},
				{"service.version", app.Version},
			})},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "asq", "version": strconv.Itoa(Format)},
				"spans": encoded,
			}},
		}},
	})
}

func encodeAttrs(attrs []Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, a := range attrs {
		value := map[string]any{}
		switch v := a.Value.(type) {
		case string:
			if v == "" {
				continue
			}
			value["stringValue"] = v
		case int:
			value["intValue"] = strconv.Itoa(v) // OTLP JSON spells a 64-bit integer as a string
		case int64:
			value["intValue"] = strconv.FormatInt(v, 10)
		case float64:
			value["doubleValue"] = v
		case bool:
			value["boolValue"] = v
		case []string:
			if len(v) == 0 {
				continue
			}
			values := make([]any, len(v))
			for i, item := range v {
				values[i] = map[string]any{"stringValue": item}
			}
			value["arrayValue"] = map[string]any{"values": values}
		default:
			continue
		}
		if a.Key != "" {
			out = append(out, map[string]any{"key": a.Key, "value": value})
		}
	}
	return out
}
