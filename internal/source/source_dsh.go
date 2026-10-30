package source

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// DeepSeek Harness (dsh): $DSH_HOME/sessions/<project>/<session>/session.v<N>.jsonl[.zstd].
//
// The file is JSONL: a header row (`session` / `header` with the id and cwd), then events —
// `user/message`, `assistant/message`, `tool/result` — each carrying `time` in epoch
// milliseconds. Two things are not obvious:
//
//   - The CLI appends compacted zstd frames: the file is a chain of independently decodable
//     frames, and the last one is often half-written. Decompress frame by frame and keep
//     what completed; the truncated tail cannot be read and is dropped with the incomplete
//     JSON line it would have contained.
//   - A `surfaceOp.op == "replace"` row is the model-visible replacement of earlier rows
//     (compaction), not something the human transcript shows. It is skipped.
//
// Usage hangs on `assistant/message`: inputTokens excludes cache, and totalTokens is
// input + output + cacheRead + cacheWrite. Session directories hold several rotating
// generations; only the highest version is the current transcript.
type DshSource struct {
	root   string
	cache  *fileRecordCache
	finals finalCache
}

func NewDshSource(root string) *DshSource {
	return &DshSource{root: root, cache: newFileRecordCache(dshCountMessages)}
}

// dshHome follows the harness's own resolution: DSH_HOME moves the whole ~/.dsh
func dshHome(home string) string {
	if d := os.Getenv("DSH_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".dsh")
}

func (s *DshSource) Mode() string     { return "dsh" }
func (s *DshSource) Location() string { return s.root }
func (s *DshSource) Exists() bool     { return fileExists(s.root) }

// dshGeneration reads the version out of session.v<N>.jsonl[.zstd]
func dshGeneration(name string) (int, bool) {
	name = strings.TrimSuffix(name, ".zstd")
	name = strings.TrimSuffix(name, ".jsonl")
	rest, ok := strings.CutPrefix(name, "session.v")
	if !ok {
		return 0, false
	}
	version, err := strconv.Atoi(rest)
	return version, err == nil
}

// files lists the current transcript of every session: the highest generation in each
// session directory.
func (s *DshSource) files() []string {
	projects, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(s.root, project.Name()))
		if err != nil {
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			dir := filepath.Join(s.root, project.Name(), session.Name())
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			best, bestVersion := "", -1
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				version, ok := dshGeneration(entry.Name())
				if !ok {
					continue
				}
				if version > bestVersion || (version == bestVersion && entry.Name() > filepath.Base(best)) {
					bestVersion = version
					best = filepath.Join(dir, entry.Name())
				}
			}
			if best != "" {
				out = append(out, best)
			}
		}
	}
	return out
}

// dshRead returns a session's bytes, decompressing the concatenated frames. A truncated
// final frame is normal for a session still being written: the complete frames before it
// are kept, which is everything the CLI has finished appending.
func dshRead(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".zstd") {
		return raw, nil
	}
	decoder, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(decoder)
	if readErr != nil && len(out) == 0 {
		return nil, readErr
	}
	return out, nil
}

// dshLines splits a transcript into complete lines. The last line is only kept when it is
// newline-terminated: a partial append is not a row yet.
func dshLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > 0 {
		lines = lines[:len(lines)-1] // either the trailing empty element or a partial line
	}
	return lines
}

// dshRecord reads one session: the header row, and the messages the reader draws.
func dshRecord(path string) (map[string]any, []map[string]any) {
	data, err := dshRead(path)
	if err != nil {
		return nil, nil
	}
	header := map[string]any{}
	messages := []map[string]any{}
	seen := map[string]bool{}
	for index, line := range dshLines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		kind := StrOr(row["type"], "")
		if index == 0 && (kind == "session" || kind == "header") {
			header = row
			continue
		}
		if kind == "session" || kind == "header" {
			continue
		}
		if op := getMap(row, "surfaceOp"); StrOr(op["op"], "") == "replace" {
			continue // a model-visible rewrite, not a row of the human transcript
		}
		message := dshMessage(kind, row)
		if message == nil {
			continue
		}
		if id := StrOr(message["id"], ""); id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		messages = append(messages, message)
	}
	return header, messages
}

// dshMessage maps one event onto the shared message shape; nil when it is not a row the
// reader draws or has no content
func dshMessage(kind string, row map[string]any) map[string]any {
	var raw map[string]any
	role := ""
	switch kind {
	case "user/message":
		raw, role = getMap(row, "data"), "user"
	case "assistant/message":
		raw, role = getMap(getMap(row, "data"), "message"), "assistant"
	case "tool/result":
		raw, role = getMap(getMap(row, "data"), "message"), "tool"
	default:
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	parts := []map[string]any{}
	if role == "tool" {
		text, _ := blockArrayText(raw["content"])
		block := ToolResultBlock(StrOr(raw["toolCallId"], ""), "tool", text, false)
		block = ToolOutcome{Status: statusFromError(Truthy(raw["isError"]))}.Apply(block)
		parts = append(parts, block)
	} else {
		for _, item := range getSlice(raw, "content") {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch StrOr(part["type"], "") {
			case "text":
				if text := strField(part, "text"); text != "" {
					parts = append(parts, TextBlock(text))
				}
			case "thinking", "reasoning":
				text := strField(part, "text")
				if text == "" {
					text = strField(part, "thinking")
				}
				if text != "" {
					parts = append(parts, ThinkingBlock(text, false))
				}
			case "tool-call":
				arguments := part["arguments"]
				if encoded, ok := arguments.(string); ok {
					var decoded any
					if json.Unmarshal([]byte(encoded), &decoded) == nil {
						arguments = decoded
					}
				}
				parts = append(parts, ToolCallBlock(strField(part, "id"), strField(part, "name"), arguments))
			}
		}
	}
	if len(parts) == 0 {
		return nil
	}
	message := map[string]any{
		"id":        StrOr(raw["id"], ""),
		"role":      role,
		"timestamp": dshTimeISO(GetOr(row, "time", nil)),
		"content":   parts,
	}
	if role == "user" {
		// A user row the harness wrote itself (context, a summary) is not something the
		// person typed, so a round does not start at it
		if kind := StrOr(getMap(raw, "source")["kind"], ""); kind != "" && kind != "user" {
			message["injected"] = true
		}
	}
	if role == "assistant" {
		if model := StrOr(getMap(raw, "source")["model"], ""); model != "" {
			message["model"] = model
		}
		if usage := UsageOf(getMap(getMap(row, "data"), "usage")); usage != nil {
			message["usage"] = usage
		}
	}
	return message
}

// dshTimeISO reads a harness timestamp: epoch milliseconds (>1e12) or seconds
func dshTimeISO(v any) string {
	n, ok := ToFloat(v)
	if !ok || n <= 0 {
		return ""
	}
	ms := int64(n)
	if ms <= 1_000_000_000_000 {
		ms *= 1000
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05")
}

// dshMessageText is the readable text of a message this source built: its text blocks,
// joined
func dshMessageText(content any) string {
	switch blocks := content.(type) {
	case string:
		return blocks
	case []map[string]any:
		texts := []string{}
		for _, block := range blocks {
			if StrOr(block["type"], "") == "text" {
				texts = append(texts, ToStr(block["content"]))
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// buildRecord is the list row for one transcript
func (s *DshSource) buildRecord(path, modISO string) Record {
	header, messages := dshRecord(path)
	id := StrOr(header["id"], "")
	if id == "" {
		id = filepath.Base(filepath.Dir(path))
	}
	cwd := StrOr(header["cwd"], "")
	title := ""
	for _, message := range messages {
		if StrOr(message["role"], "") != "user" || Truthy(message["injected"]) {
			continue
		}
		if text := dshMessageText(message["content"]); strings.TrimSpace(text) != "" {
			title = TitleFromUserText(text)
			break
		}
	}
	created := dshTimeISO(header["createdAt"])
	updated := ""
	for _, message := range messages {
		if at := StrOr(message["timestamp"], ""); at > updated {
			updated = at
		}
	}
	if updated == "" {
		updated = FirstNonEmpty(created, modISO)
	}
	return NewRecord(Record{
		Source:    "dsh",
		Key:       path,
		ShortKey:  FirstNonEmpty(title, id),
		SessionID: id,
		File:      path,
		HasFile:   true,
		Status:    "done",
		Cwd:       cwd,
		CreatedAt: created,
		UpdatedAt: updated,
	}, updated)
}

func (s *DshSource) List() []Record {
	return s.cache.records(s.files(), s.buildRecord)
}

func (s *DshSource) Messages(r Record, q MessageQuery) []map[string]any {
	sink := newMessageSink(q)
	if r.File == "" {
		return sink.result()
	}
	_, messages := dshRecord(r.File)
	for _, message := range messages {
		if !sink.add(message) {
			break
		}
	}
	return sink.result()
}

// Search scans the decompressed transcript with the same line rules the other files use
func (s *DshSource) Search(ctx context.Context, r Record, q SearchQuery) []map[string]any {
	data, err := dshRead(r.File)
	if err != nil {
		return nil
	}
	return searchLines(ctx, q, lineRules{
		stamp: func(obj map[string]any) any { return obj["time"] },
		keep:  dshSearchRow,
	}, func(yield func([]byte) bool) {
		for _, line := range dshLines(data) {
			if !yield(bytes.TrimSpace(line)) {
				return
			}
		}
	})
}

// dshSearchRow keeps the rows the reader draws; a compaction replacement is skipped just
// as Messages skips it
func dshSearchRow(obj map[string]any) bool {
	switch StrOr(obj["type"], "") {
	case "user/message", "assistant/message", "tool/result":
	default:
		return false
	}
	return StrOr(getMap(obj, "surfaceOp")["op"], "") != "replace"
}

// DshHitRole names the speaker of a DSH row (its role lives in the row type)
func DshHitRole(obj map[string]any) string {
	switch StrOr(obj["type"], "") {
	case "user/message":
		return "user"
	case "assistant/message":
		return "assistant"
	case "tool/result":
		return "tool"
	}
	return ""
}

// dshCountMessages counts the conversation rows (user and assistant), deduplicated by id
func dshCountMessages(path string, from int64) (int, bool) {
	_ = from
	// Compressed frames cannot be resumed from a byte offset: count the whole file and
	// say so, as the grouping counters do
	_, messages := dshRecord(path)
	count := 0
	for _, message := range messages {
		if role := StrOr(message["role"], ""); role == "user" || role == "assistant" {
			count++
		}
	}
	return count, false
}

func (s *DshSource) Final(r Record) map[string]any {
	if r.File == "" {
		return nil
	}
	return s.finals.load([]string{r.File}, func() map[string]any { return s.readFinal(r) })
}

// readFinal is Final's one pass over the transcript: the last assistant text, the token
// totals (input excludes cache), and the number of conversation rows
func (s *DshSource) readFinal(r Record) map[string]any {
	_, messages := dshRecord(r.File)
	count := 0
	var totals usageTotals
	var last map[string]any
	for _, message := range messages {
		role := StrOr(message["role"], "")
		if role != "user" && role != "assistant" {
			continue
		}
		count++
		if role != "assistant" {
			continue
		}
		last = message
		if usage, ok := message["usage"].(map[string]any); ok {
			totals.add(usage)
		}
	}
	out := map[string]any{
		"status":       "done",
		"isFinal":      last != nil,
		"isProcessing": false,
		"messageCount": count,
		"source":       "dsh",
		"text":         "",
		"thinking":     "",
	}
	if last != nil {
		out["text"] = dshMessageText(last["content"])
		out["timestamp"] = StrOr(last["timestamp"], "")
		if model := StrOr(last["model"], ""); model != "" {
			out["model"] = model
		}
	}
	if usage := totals.result(); usage != nil {
		out["usage"] = usage
	}
	return out
}
