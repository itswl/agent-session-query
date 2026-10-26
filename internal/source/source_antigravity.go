package source

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Antigravity: Google's agent IDE writes the same session layout as its `agy` CLI, one
// directory per conversation:
//
//	~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript_full.jsonl   (agy)
//	~/.gemini/antigravity/brain/<id>/.system_generated/logs/transcript_full.jsonl       (IDE)
//
// The transcript is a JSONL of steps that is rewritten while the session runs — a step's
// completed line can be corrected later, so the latest line for a step_index is the truth
// and the file is read as a map, not in order. Sibling to the brains,
// conversation_summaries.db carries each conversation's title, workspace and last-active
// time (table conversation_summaries), which lets a list answer without opening a
// transcript at all.
//
// An API-key provider can run agy with a private --gemini_dir (cc-switch's profiles are
// the common case); that directory has the same shape, which is what --path
// antigravity=<dir> points at.
type AntigravitySource struct {
	geminiDir string
	roots     []string
	cache     *fileRecordCache
	summaries dbSnapshotCache[map[string]agySummary]
	finals    finalCache
	mu        sync.Mutex
	listErr   error
}

// agySummary is one conversation_summaries row, as the file system needs it
type agySummary struct {
	title   string
	cwd     string
	updated string
}

func NewAntigravitySource(geminiDir string) *AntigravitySource {
	return &AntigravitySource{
		geminiDir: geminiDir,
		roots: []string{
			filepath.Join(geminiDir, "antigravity-cli", "brain"),
			filepath.Join(geminiDir, "antigravity", "brain"),
		},
		cache: newFileRecordCache(agyCountMessages),
	}
}

func (s *AntigravitySource) Mode() string { return "antigravity" }

func (s *AntigravitySource) Location() string { return s.roots[0] }

// Exists: the IDE alone or the CLI alone is enough; the other root is simply empty
func (s *AntigravitySource) Exists() bool {
	return fileExists(s.roots[0]) || fileExists(s.roots[1])
}

// setListError records how the last list went (see ListErrorReporter)
func (s *AntigravitySource) setListError(err error) {
	s.mu.Lock()
	s.listErr = err
	s.mu.Unlock()
}

// ListError implements ListErrorReporter
func (s *AntigravitySource) ListError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listErr
}

// transcriptPath is where a session's steps live under one brain root
func agyTranscriptPath(root, id string) string {
	return filepath.Join(root, id, ".system_generated", "logs", "transcript_full.jsonl")
}

// validAgyID: session directories are ids, not arbitrary names
func validAgyID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (s *AntigravitySource) List() []Record {
	paths := []string{}
	sums := map[string]agySummary{}
	seen := map[string]bool{}
	for _, root := range s.roots {
		// The index first: a session it knows costs one directory entry, not a transcript
		for id, sum := range s.summariesOf(root) {
			if _, have := sums[id]; !have {
				sums[id] = sum
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			id := entry.Name()
			if !entry.IsDir() || !validAgyID(id) || seen[id] {
				continue
			}
			path := agyTranscriptPath(root, id)
			if !fileExists(path) {
				continue // a conversation with only the binary store: nothing to read
			}
			seen[id] = true
			paths = append(paths, path)
		}
	}

	records := s.cache.records(paths, s.buildRecord)
	for i := range records {
		sum, ok := sums[records[i].SessionID]
		if !ok {
			continue
		}
		// The index is the CLI's own answer to "what is this", and its time moves when
		// the conversation does even if the transcript write is still in flight
		if sum.title != "" {
			records[i].ShortKey = sum.title
		}
		if sum.cwd != "" {
			records[i].Cwd = sum.cwd
			records[i].Project = sum.cwd
		}
		if sum.updated != "" {
			records[i].UpdatedAt = sum.updated
			records[i] = NewRecord(records[i], sum.updated)
		}
	}
	return records
}

// buildRecord is the fallback for a session conversation_summaries.db does not know:
// one pass over the transcript for the opening ask and the span of the steps
func (s *AntigravitySource) buildRecord(path, modISO string) Record {
	id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	title, first, last := "", "", ""
	EachJSONLLine(path, func(line []byte) bool {
		var step struct {
			Type      string `json:"type"`
			Source    string `json:"source"`
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
		}
		if json.Unmarshal(line, &step) != nil {
			return true
		}
		if step.CreatedAt != "" {
			if first == "" || step.CreatedAt < first {
				first = step.CreatedAt
			}
			if step.CreatedAt > last {
				last = step.CreatedAt
			}
		}
		if title == "" && step.Type == "USER_INPUT" {
			title = TitleFromUserText(agyUserRequest(step.Content))
		}
		return true
	})
	updated := FirstNonEmpty(last, first, modISO)
	return NewRecord(Record{
		Source:    "antigravity",
		Key:       path,
		ShortKey:  FirstNonEmpty(title, id),
		SessionID: id,
		File:      path,
		HasFile:   true,
		Status:    "done",
		CreatedAt: first,
		UpdatedAt: updated,
	}, updated)
}

// summariesOf reads the index beside one brain root (…/antigravity-cli/conversation_summaries.db),
// by fingerprint: a poll that finds it unchanged costs three stats.
func (s *AntigravitySource) summariesOf(root string) map[string]agySummary {
	dbPath := filepath.Join(filepath.Dir(root), "conversation_summaries.db")
	if !fileExists(dbPath) {
		return nil
	}
	sums, err := s.summaries.load(dbPath, func() (map[string]agySummary, error) {
		return readAgySummaries(dbPath)
	})
	if err != nil {
		return nil
	}
	return sums
}

// readAgySummaries is the one query behind summariesOf
func readAgySummaries(dbPath string) (map[string]agySummary, error) {
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(dbPath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT conversation_id, title, workspace_uris, last_modified_time
		FROM conversation_summaries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]agySummary{}
	for rows.Next() {
		var id, title, uris, modified sql.NullString
		if err := rows.Scan(&id, &title, &uris, &modified); err != nil {
			return out, err
		}
		if !id.Valid || !validAgyID(id.String) {
			continue
		}
		out[id.String] = agySummary{
			title:   StripTerminalControls(title.String),
			cwd:     agyWorkspace(uris.String),
			updated: agyTime(modified.String),
		}
	}
	return out, rows.Err()
}

// agyWorkspace turns workspace_uris (a JSON array of file:// URIs) into the first path
func agyWorkspace(uris string) string {
	var list []string
	if json.Unmarshal([]byte(uris), &list) != nil || len(list) == 0 {
		return ""
	}
	u := list[0]
	if !strings.HasPrefix(u, "file://") {
		return ""
	}
	p, err := url.PathUnescape(strings.TrimPrefix(u, "file://"))
	if err != nil {
		return ""
	}
	// file:///C:/work → /C:/work on Windows, where the drive letter does not take a slash
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// agyTime normalizes the index's last_modified_time: ISO in some builds, a naive
// "2006-01-02 15:04:05.999" in others
func agyTime(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, ok := ParseTimestamp(raw); ok {
		return raw // already a time the reader takes as it is
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05")
		}
	}
	return ""
}

// agyUserRequest is what the user typed: agy wraps it in <USER_REQUEST> and appends its
// own context blocks (metadata, settings changes) after it
func agyUserRequest(content string) string {
	_, rest, found := strings.Cut(content, "<USER_REQUEST>")
	if !found {
		return content
	}
	inner, _, found := strings.Cut(rest, "</USER_REQUEST>")
	if !found || strings.TrimSpace(inner) == "" {
		return content
	}
	return strings.TrimSpace(inner)
}

// agySteps reads a transcript as steps: the latest line for a step_index wins (the CLI
// corrects finished steps in place), returned in step order
func agySteps(path string) []map[string]any {
	byIndex := map[int64]map[string]any{}
	EachJSONLLine(path, func(line []byte) bool {
		var step map[string]any
		if json.Unmarshal(line, &step) != nil {
			return true
		}
		index, ok := agyStepIndex(step)
		if !ok {
			return true
		}
		byIndex[index] = step // the file order is write order; the last write wins
		return true
	})
	indexes := make([]int64, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })

	out := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, byIndex[index])
	}
	return out
}

func agyStepIndex(step map[string]any) (int64, bool) {
	if n, ok := ToFloat(step["step_index"]); ok {
		return int64(n), true
	}
	return 0, false
}

// agyRole maps a step onto a message role: the user's own prompts and the model's
// answers are the conversation; everything else (tool steps, settings changes) is kept
// visible as an event rather than dropped or passed off as the model's reply
func agyRole(step map[string]any) string {
	switch ToStr(step["type"]) {
	case "USER_INPUT":
		return "user"
	case "PLANNER_RESPONSE":
		return "assistant"
	}
	return "system"
}

// agyConversationStep: user and model steps are the messages a session is made of
func agyConversationStep(step map[string]any) bool {
	role := agyRole(step)
	return role == "user" || role == "assistant"
}

// agyStepText is a step's readable content. A step type this reader does not know (agy
// keeps adding tool/event kinds) keeps whatever content it carries; the page labels the
// row with the step's own type name.
func agyStepText(step map[string]any, role string, full bool) string {
	content := ToStr(step["content"])
	if role == "user" {
		return agyUserRequest(content)
	}
	cut, _ := clip(content, toolResultLimit, full)
	return cut
}

// agyCountMessages counts the conversation steps (see countFile)
func agyCountMessages(path string, from int64) (int, bool) {
	_ = from
	// Steps are rewritten in place, so a line count is not a message count and a resumed
	// tail could count a corrected step twice: count the whole file and say so.
	seen := map[int64]bool{}
	EachJSONLLine(path, func(line []byte) bool {
		var step map[string]any
		if json.Unmarshal(line, &step) != nil || !agyConversationStep(step) {
			return true
		}
		if index, ok := agyStepIndex(step); ok {
			seen[index] = true
		}
		return true
	})
	return len(seen), false
}

func (s *AntigravitySource) Messages(r Record, q MessageQuery) []map[string]any {
	sink := newMessageSink(q)
	if r.File == "" {
		return sink.result()
	}
	for _, step := range agySteps(r.File) {
		role := agyRole(step)
		index, _ := agyStepIndex(step)
		text := agyStepText(step, role, q.Full)
		var blocks []map[string]any
		if role == "system" {
			// The step kind is the only name this reader can give it; the page shows it
			blocks = []map[string]any{EventBlock(ToStr(step["type"]), text)}
		} else {
			blocks = []map[string]any{TextBlock(text)}
		}
		message := map[string]any{
			"id":        strconv.FormatInt(index, 10),
			"role":      role,
			"timestamp": ToStr(step["created_at"]),
			"content":   blocks,
		}
		if role == "assistant" {
			if usage := UsageOf(map[string]any{
				"input_tokens":       step["input_tokens"],
				"output_tokens":      step["output_tokens"],
				"cache_read_tokens":  step["cache_read_tokens"],
				"cache_write_tokens": step["cache_write_tokens"],
			}); usage != nil {
				message["usage"] = usage
			}
		}
		if !sink.add(message) {
			break
		}
	}
	return sink.result()
}

// Final is the session's final result, read once per version of its transcript (see
// finalCache)
func (s *AntigravitySource) Final(r Record) map[string]any {
	if r.File == "" {
		return nil
	}
	return s.finals.load([]string{r.File}, func() map[string]any { return s.readFinal(r) })
}

// readFinal is Final's one pass over the transcript
func (s *AntigravitySource) readFinal(r Record) map[string]any {
	steps := agySteps(r.File)
	count := 0
	var totals usageTotals
	var last map[string]any
	for _, step := range steps {
		if !agyConversationStep(step) {
			continue
		}
		count++
		if agyRole(step) != "assistant" {
			continue
		}
		last = step
		if ToStr(step["status"]) == "DONE" {
			// agy's counters: input excludes cache, output includes thinking
			totals.add(map[string]any{
				"input_tokens":       step["input_tokens"],
				"output_tokens":      step["output_tokens"],
				"cache_read_tokens":  step["cache_read_tokens"],
				"cache_write_tokens": step["cache_write_tokens"],
			})
		}
	}
	if last == nil {
		return map[string]any{
			"status":       "done",
			"isFinal":      false,
			"isProcessing": false,
			"messageCount": count,
			"source":       "antigravity",
			"text":         "",
			"thinking":     "",
		}
	}
	out := map[string]any{
		"status":       "done",
		"isFinal":      true,
		"isProcessing": false,
		"messageCount": count,
		"source":       "antigravity",
		"text":         agyStepText(last, "assistant", true),
		"thinking":     "",
		"timestamp":    ToStr(last["created_at"]),
	}
	if model := ToStr(last["model"]); model != "" {
		out["model"] = model
	}
	if usage := totals.result(); usage != nil {
		out["usage"] = usage
	}
	return out
}
