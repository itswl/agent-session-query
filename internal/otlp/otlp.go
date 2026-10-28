package otlp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OTLP over HTTP with the JSON encoding: no protobuf dependency, one request format that
// every collector accepts (Langfuse included). The exporter is deliberately small — spans
// in, one POST out — because the interesting decisions live in what the caller puts in the
// spans, not in the wire.

// Attribute is one span attribute. Values are string, int64, float64 or bool; OTLP JSON
// spells int64 as a string, which this converts at encode time.
type Attribute struct {
	Key   string
	Value any
}

// Span is one OTLP span. Times are wall clock: the format carries explicit start/end, so
// a span read from a transcript after the fact is a legitimate span.
type Span struct {
	TraceID  string // 32 hex characters
	SpanID   string // 16 hex characters
	ParentID string // empty for a trace root

	Name       string
	Kind       int // 1 internal, 2 server, 3 client
	Start, End time.Time
	Attributes []Attribute
	Error      bool
	Status     string // the error's message, when Error
}

// hash folds the parts into one SHA-256 digest
func hash(parts ...string) [32]byte {
	return sha256.Sum256([]byte(strings.Join(parts, "\x00")))
}

// NewID derives a stable 128-bit hex id from the parts: the same session, round and step
// send the same id on every export, which is what makes a resumed or re-read transcript
// idempotent instead of duplicated.
func NewID(parts ...string) string {
	sum := hash(parts...)
	return hex.EncodeToString(sum[:16])
}

// NewSpanID derives a stable 64-bit hex id from the parts.
func NewSpanID(parts ...string) string {
	sum := hash(parts...)
	return hex.EncodeToString(sum[:8])
}

// Resource attributes for every span this service sends
func Resource() []Attribute {
	return []Attribute{
		{Key: "service.name", Value: "agent-session-query"},
		{Key: "service.version", Value: "dev"},
	}
}

// Send posts spans to endpoint, appending /v1/traces to a base URL. headers are sent as
// given (Langfuse: Authorization + x-langfuse-ingestion-version). It retries once on a
// server error or a transport failure; a collector that is down must never fail the
// service, so the error comes back for the caller to log and drop.
func Send(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	target := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(target, "/v1/traces") {
		target += "/v1/traces"
	}
	body, err := Encode(spans)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("collector answered %s", resp.Status)
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return lastErr // a rejected payload will be rejected again
		}
	}
	return lastErr
}

// Encode renders spans as an OTLP/HTTP JSON request body.
func Encode(spans []Span) ([]byte, error) {
	if len(spans) == 0 {
		return nil, errors.New("no spans")
	}
	encoded := make([]any, 0, len(spans))
	for _, s := range spans {
		span := map[string]any{
			"traceId":           s.TraceID,
			"spanId":            s.SpanID,
			"name":              s.Name,
			"kind":              s.Kind,
			"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(s.End.UnixNano(), 10),
			"status":            map[string]any{"code": 0},
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
		attrs := make([]any, 0, len(s.Attributes))
		for _, a := range s.Attributes {
			if a.Key == "" || a.Value == nil {
				continue
			}
			value := map[string]any{}
			switch v := a.Value.(type) {
			case string:
				if v == "" {
					continue
				}
				value["stringValue"] = v
			case int:
				value["intValue"] = strconv.Itoa(v)
			case int64:
				value["intValue"] = strconv.FormatInt(v, 10)
			case float64:
				value["doubleValue"] = v
			case bool:
				value["boolValue"] = v
			default:
				continue
			}
			attrs = append(attrs, map[string]any{"key": a.Key, "value": value})
		}
		span["attributes"] = attrs
		encoded = append(encoded, span)
	}
	resourceAttrs := make([]any, 0, len(Resource()))
	for _, a := range Resource() {
		resourceAttrs = append(resourceAttrs, map[string]any{"key": a.Key, "value": map[string]any{"stringValue": a.Value}})
	}
	body := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": resourceAttrs},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "github.com/itswl/agent-session-query"},
				"spans": encoded,
			}},
		}},
	}
	return json.Marshal(body)
}

// Metric is one sum metric: a name, a unit and data points for this export pass. The
// exporter sends deltas (aggregationTemporality=1), so a collector adds them without the
// service having to keep process-lifetime totals.
type Metric struct {
	Name      string
	Unit      string
	Monotonic bool
	Points    []MetricPoint
}

// MetricPoint is one labelled value of a metric
type MetricPoint struct {
	Attributes []Attribute
	Value      int64
}

// SendMetrics posts metrics to endpoint, appending /v1/metrics to a base URL. Same retry
// and error contract as Send: a collector that is down never fails the service.
func SendMetrics(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, metrics []Metric, start time.Time) error {
	if len(metrics) == 0 {
		return nil
	}
	target := strings.TrimRight(endpoint, "/")
	target = strings.TrimSuffix(target, "/v1/traces")
	if !strings.HasSuffix(target, "/v1/metrics") {
		target += "/v1/metrics"
	}
	body, err := EncodeMetrics(metrics, start, time.Now())
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("collector answered %s", resp.Status)
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return lastErr
		}
	}
	return lastErr
}

// EncodeMetrics renders the metrics as an OTLP/HTTP JSON request body, delta temporality.
func EncodeMetrics(metrics []Metric, start, now time.Time) ([]byte, error) {
	if len(metrics) == 0 {
		return nil, errors.New("no metrics")
	}
	encoded := make([]any, 0, len(metrics))
	for _, m := range metrics {
		points := make([]any, 0, len(m.Points))
		for _, p := range m.Points {
			point := map[string]any{
				"startTimeUnixNano": strconv.FormatInt(start.UnixNano(), 10),
				"timeUnixNano":      strconv.FormatInt(now.UnixNano(), 10),
				"asInt":             strconv.FormatInt(p.Value, 10),
			}
			attrs := make([]any, 0, len(p.Attributes))
			for _, a := range p.Attributes {
				if text, ok := a.Value.(string); ok && text != "" {
					attrs = append(attrs, map[string]any{"key": a.Key, "value": map[string]any{"stringValue": text}})
				}
			}
			point["attributes"] = attrs
			points = append(points, point)
		}
		metric := map[string]any{
			"name": m.Name,
			"sum": map[string]any{
				"dataPoints":             points,
				"aggregationTemporality": 1, // delta
				"isMonotonic":            m.Monotonic,
			},
		}
		if m.Unit != "" {
			metric["unit"] = m.Unit
		}
		encoded = append(encoded, metric)
	}
	resourceAttrs := make([]any, 0, len(Resource()))
	for _, a := range Resource() {
		resourceAttrs = append(resourceAttrs, map[string]any{"key": a.Key, "value": map[string]any{"stringValue": a.Value}})
	}
	body := map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource": map[string]any{"attributes": resourceAttrs},
			"scopeMetrics": []any{map[string]any{
				"scope":   map[string]any{"name": "github.com/itswl/agent-session-query"},
				"metrics": encoded,
			}},
		}},
	}
	return json.Marshal(body)
}
