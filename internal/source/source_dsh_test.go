package source

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// dshFixture lays down one DeepSeek Harness session: the header, two turns with a tool
// call, one duplicate message id, a compaction replacement, and (optionally) zstd frames
// with a half-written last frame.
func dshFixture(t *testing.T, compress bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "project", "session-abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	ms := func(offset time.Duration) int64 { return at.Add(offset).UnixMilli() }
	lines := []string{
		`{"type":"session","version":4,"id":"dsh-1","cwd":"/w/proj","createdAt":` + strconv.FormatInt(at.Unix(), 10) + `}`,
		`{"type":"user/message","seq":1,"surfaceOp":{"op":"append"},"time":` + strconv.FormatInt(ms(0), 10) + `,"data":{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"fix the uploader"}]}}`,
		`{"type":"assistant/message","seq":2,"time":` + strconv.FormatInt(ms(time.Second), 10) + `,"data":{"message":{"id":"a1","source":{"kind":"model","provider":"deepseek-account","model":"deepseek-flash"},"content":[{"type":"text","text":"looking at it"},{"type":"tool-call","id":"c1","name":"read","arguments":"{\"path\":\"a.go\"}"}]},"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":300,"cacheWriteTokens":5,"totalTokens":425}}}`,
		`{"type":"tool/result","seq":3,"time":` + strconv.FormatInt(ms(2*time.Second), 10) + `,"data":{"message":{"id":"r1","role":"tool","toolCallId":"c1","isError":false,"content":[{"type":"text","text":"package x"}]}}}`,
		`{"type":"assistant/message","seq":4,"time":` + strconv.FormatInt(ms(3*time.Second), 10) + `,"data":{"message":{"id":"a2","source":{"kind":"model","model":"deepseek-flash"},"content":[{"type":"text","text":"done"}]},"usage":{"inputTokens":10,"outputTokens":20}}}`,
		// A repeated id is the same message written again
		`{"type":"assistant/message","seq":5,"time":` + strconv.FormatInt(ms(4*time.Second), 10) + `,"data":{"message":{"id":"a1","content":[{"type":"text","text":"dup"}]}}}`,
		// A compaction replacement is model-visible only, never a transcript row
		`{"type":"user/message","seq":6,"surfaceOp":{"op":"replace","startSeq":1,"endSeq":3},"time":` + strconv.FormatInt(ms(5*time.Second), 10) + `,"data":{"id":"summary","content":[{"type":"text","text":"compacted summary"}]}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	if !compress {
		path := filepath.Join(dir, "session.v4.jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return root, path
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	var frames bytes.Buffer
	for start := 0; start < len(data); {
		end := start + 97 // small frames, the way event batches arrive
		if end > len(data) {
			end = len(data)
		}
		frames.Write(encoder.EncodeAll(data[start:end], nil))
		start = end
	}
	// A frame still being written: its bytes are incomplete
	tail := encoder.EncodeAll([]byte(`{"type":"tool/result","time":1,"data":{"message":{"id":"partial",`), nil)
	frames.Write(tail[:len(tail)/2])
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	if err := os.WriteFile(path, frames.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

// dshText is the text of a message built by this source: its first text block
func dshText(msg map[string]any) string {
	blocks, _ := msg["content"].([]map[string]any)
	for _, block := range blocks {
		if block["type"] == "text" {
			return ToStr(block["content"])
		}
	}
	return ""
}

func TestDshSource(t *testing.T) {
	root, _ := dshFixture(t, false)
	src := NewDshSource(root)
	if !src.Exists() {
		t.Fatal("sessions dir exists")
	}
	records := src.List()
	if len(records) != 1 {
		t.Fatalf("list = %v", records)
	}
	rec := records[0]
	if rec.SessionID != "dsh-1" || rec.Cwd != "/w/proj" || rec.ShortKey != "fix the uploader" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.UpdatedAt != "2026-10-29T10:00:03" {
		t.Fatalf("updatedAt = %q", rec.UpdatedAt)
	}

	msgs := src.Messages(rec, MessageQuery{Limit: 50})
	if len(msgs) != 4 { // u1, a1, r1, a2 — the duplicate and the replacement are not rows
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "user" || dshText(msgs[0]) != "fix the uploader" {
		t.Fatalf("first message = %v", msgs[0])
	}
	calls := 0
	for _, block := range msgs[1]["content"].([]map[string]any) {
		if block["type"] == "toolCall" && block["name"] == "read" {
			calls++
			if block["arguments"].(map[string]any)["path"] != "a.go" {
				t.Fatalf("arguments = %v", block["arguments"])
			}
		}
	}
	if calls != 1 {
		t.Fatalf("tool calls = %d", calls)
	}
	result := msgs[2]["content"].([]map[string]any)[0]
	if result["type"] != "toolResult" || result["callId"] != "c1" || result["status"] != StatusOK {
		t.Fatalf("tool result = %v", result)
	}
	usageOf := func(v any) float64 { n, _ := ToFloat(v); return n }
	if usage := msgs[1]["usage"].(map[string]any); usageOf(usage["inputTokens"]) != 100 || usageOf(usage["cacheReadTokens"]) != 300 {
		t.Fatalf("message usage = %v", usage)
	}

	final := src.Final(rec)
	if final["text"] != "done" || final["messageCount"] != 3 || final["model"] != "deepseek-flash" {
		t.Fatalf("final = %v", final)
	}
	usage := final["usage"].(map[string]any)
	if usage["inputTokens"] != int64(110) || usage["outputTokens"] != int64(40) || usage["cacheReadTokens"] != int64(300) {
		t.Fatalf("final usage = %v", usage)
	}
}

// TestDshZstdFrames: a compressed transcript is a chain of independent frames with a
// half-written tail; the complete frames read, the tail is dropped with its partial line.
func TestDshZstdFrames(t *testing.T) {
	root, path := dshFixture(t, true)
	if !strings.HasSuffix(path, ".zstd") {
		t.Fatal(path)
	}
	src := NewDshSource(root)
	records := src.List()
	if len(records) != 1 || records[0].ShortKey != "fix the uploader" {
		t.Fatalf("list = %v", records)
	}
	msgs := src.Messages(records[0], MessageQuery{Limit: 50})
	if len(msgs) != 4 {
		t.Fatalf("compressed messages = %d", len(msgs))
	}
}

// TestDshHighestGeneration: a session directory keeps several rotating generations; the
// highest is the current transcript
func TestDshHighestGeneration(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"type":"session","id":"old","cwd":"/w"}` + "\n" +
		`{"type":"user/message","time":1,"data":{"id":"u","role":"user","content":[{"type":"text","text":"old title"}]}}` + "\n"
	cur := `{"type":"session","id":"cur","cwd":"/w"}` + "\n" +
		`{"type":"user/message","time":2,"data":{"id":"u2","role":"user","content":[{"type":"text","text":"current title"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session.v3.jsonl"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl"), []byte(cur), 0o600); err != nil {
		t.Fatal(err)
	}
	records := NewDshSource(root).List()
	if len(records) != 1 || records[0].SessionID != "cur" || records[0].ShortKey != "current title" {
		t.Fatalf("records = %+v", records)
	}
}

// TestDshSearch: search reads the decompressed transcript and names the speaker from the
// row type; a compaction replacement is not searched
func TestDshSearch(t *testing.T) {
	root, _ := dshFixture(t, true)
	src := NewDshSource(root)
	rec := src.List()[0]
	hits := src.Search(context.Background(), rec, SearchQuery{
		Needle: "uploader", Lowered: AppendLowerASCII(nil, []byte("uploader")), Limit: 10,
	})
	if len(hits) != 1 || hits[0]["role"] != "user" {
		t.Fatalf("hits = %v", hits)
	}
	if got := src.Search(context.Background(), rec, SearchQuery{
		Needle: "compacted", Lowered: AppendLowerASCII(nil, []byte("compacted")), Limit: 10,
	}); len(got) != 0 {
		t.Fatalf("a replacement row was searched: %v", got)
	}
}
