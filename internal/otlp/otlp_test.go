package otlp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEncodeAndSend: the wire shape is OTLP/HTTP JSON, with the ids, nanos, attributes and
// status a collector reads. Errors become status 2 with a message.
func TestEncodeAndSend(t *testing.T) {
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	spans := []Span{{
		TraceID: NewID("s", "1", "trace"), SpanID: NewSpanID("s", "1", "root"),
		Name: "round 1", Kind: 1, Start: start, End: start.Add(2 * time.Second),
		Attributes: []Attribute{
			{Key: "langfuse.session.id", Value: "s1"},
			{Key: "round.index", Value: int64(1)},
			{Key: "timing.source", Value: "recorded"},
		},
	}, {
		TraceID: NewID("s", "1", "trace"), SpanID: NewSpanID("s", "1", "tool"),
		ParentID: NewSpanID("s", "1", "root"), Name: "Bash", Kind: 1,
		Start: start, End: start.Add(time.Second), Error: true, Status: "boom",
	}}

	var gotPath, gotAuth, gotVersion string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotVersion = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-langfuse-ingestion-version")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := Send(context.Background(), server.Client(), server.URL, map[string]string{
		"Authorization":                "Basic abc",
		"x-langfuse-ingestion-version": "4",
	}, spans)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/traces" || gotAuth != "Basic abc" || gotVersion != "4" {
		t.Fatalf("request = %s auth=%q version=%q", gotPath, gotAuth, gotVersion)
	}
	root := body["resourceSpans"].([]any)[0].(map[string]any)
	if root["resource"] == nil {
		t.Fatalf("no resource: %v", body)
	}
	encoded := root["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(encoded) != 2 {
		t.Fatalf("spans = %d", len(encoded))
	}
	first := encoded[0].(map[string]any)
	if first["traceId"] != spans[0].TraceID || first["spanId"] != spans[0].SpanID {
		t.Fatalf("ids = %v / %v", first["traceId"], first["spanId"])
	}
	if first["startTimeUnixNano"] != strconv.FormatInt(start.UnixNano(), 10) {
		t.Fatalf("start = %v", first["startTimeUnixNano"])
	}
	second := encoded[1].(map[string]any)
	if second["parentSpanId"] != spans[1].ParentID {
		t.Fatalf("parent = %v", second["parentSpanId"])
	}
	if status := second["status"].(map[string]any); status["code"] != float64(2) || status["message"] != "boom" {
		t.Fatalf("status = %v", status)
	}
	attrs := first["attributes"].([]any)
	found := map[string]map[string]any{}
	for _, raw := range attrs {
		attr := raw.(map[string]any)
		found[attr["key"].(string)] = attr["value"].(map[string]any)
	}
	if found["langfuse.session.id"]["stringValue"] != "s1" {
		t.Fatalf("session attr = %v", found)
	}
	if found["round.index"]["intValue"] != "1" {
		t.Fatalf("int attr = %v", found["round.index"])
	}
}

// TestSendEndpointAndFailure: a base URL gains /v1/traces, a URL that already names the
// path is left alone, and a failing collector surfaces its error
func TestSendEndpointAndFailure(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/bad/v1/traces") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	span := Span{TraceID: NewID("t"), SpanID: NewSpanID("s"), Name: "x", Start: time.Now(), End: time.Now()}

	if err := Send(context.Background(), server.Client(), server.URL, nil, []Span{span}); err != nil {
		t.Fatal(err)
	}
	if err := Send(context.Background(), server.Client(), server.URL+"/v1/traces", nil, []Span{span}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/v1/traces" || paths[1] != "/v1/traces" {
		t.Fatalf("paths = %v", paths)
	}
	err := Send(context.Background(), server.Client(), server.URL+"/bad", nil, []Span{span})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %v", err)
	}
}
