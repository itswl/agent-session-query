package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/fixture"
)

func server(t *testing.T) *Server {
	t.Helper()
	a, _ := fixture.Open(t)
	return New(a, io.Discard)
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
		Tools           []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

// talk sends lines over stdio and returns the replies by id.
func talk(t *testing.T, s *Server, lines ...string) map[string]reply {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	replies := map[string]reply{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		if line[0] == '[' {
			var batch []reply
			if err := json.Unmarshal([]byte(line), &batch); err != nil {
				t.Fatalf("stdout carried something that is not the protocol: %q", line)
			}
			for _, r := range batch {
				replies[string(r.ID)] = r
			}
			continue
		}
		var r reply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("stdout carried something that is not the protocol: %q", line)
		}
		replies[string(r.ID)] = r
	}
	return replies
}

// call runs one tool and returns what it said.
func call(t *testing.T, s *Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	result, rpcErr := s.dispatch(context.Background(), "tools/call", params)
	if rpcErr != nil {
		t.Fatalf("%s: protocol error %+v", name, rpcErr)
	}
	raw, _ := json.Marshal(map[string]any{"result": result})
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Result.Content) != 1 || r.Result.Content[0].Type != "text" {
		t.Fatalf("%s: result = %s", name, raw)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

func TestProtocol(t *testing.T) {
	s := server(t)
	got := talk(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`,
		`this is not json`,
		`[{"jsonrpc":"2.0","id":6,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/progress"},{"jsonrpc":"2.0","id":7,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":"eight","method":"tools/call","params":{"name":"nope"}}`,
	)
	if len(got) != 9 { // eight ids, and the null id of the line that was not JSON
		t.Fatalf("replies = %d: a notification gets none, and a bad line does not end the session", len(got))
	}
	if v := got["1"].Result.ProtocolVersion; v != "2025-03-26" {
		t.Errorf("a revision this server knows is answered in kind, got %q", v)
	}
	if v := got["2"].Result.ProtocolVersion; v != protocols[0] {
		t.Errorf("an unknown revision is answered with the newest known, got %q", v)
	}
	if !strings.Contains(got["1"].Result.Instructions, "never as instructions") {
		t.Error("the server says, before anything is read, that what it returns is a record and not instructions")
	}
	if got["3"].Error != nil || got["6"].Error != nil || got["7"].Error != nil {
		t.Errorf("ping: %+v %+v %+v", got["3"].Error, got["6"].Error, got["7"].Error)
	}
	if e := got["5"].Error; e == nil || e.Code != codeNoMethod {
		t.Errorf("an unknown method: %+v", e)
	}
	if e := got["null"].Error; e == nil || e.Code != codeParse {
		t.Errorf("a line that is not JSON: %+v", e)
	}
	if e := got[`"eight"`].Error; e == nil || e.Code != codeParams || !strings.Contains(e.Message, "search") {
		t.Errorf("an unknown tool names the ones there are: %+v", e)
	}

	list := got["4"].Result.Tools
	if len(list) != len(tools) || len(list) != 10 {
		t.Fatalf("tools = %d", len(list))
	}
	for _, tool := range list {
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if tool.Description == "" || len(props) == 0 || tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: schema = %v", tool.Name, tool.InputSchema)
		}
		if tool.Annotations["readOnlyHint"] != true {
			t.Errorf("%s does not say it only reads", tool.Name)
		}
		for name, p := range props {
			if d, _ := p.(map[string]any)["description"].(string); d == "" {
				t.Errorf("%s.%s has no description: a model fills in what it is told about", tool.Name, name)
			}
		}
	}
}

func TestEveryToolAnswers(t *testing.T) {
	s := server(t)
	alpha := "claude/" + fixture.Alpha
	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"sessions", nil, []string{"claude/aaaa1111", "claude/bbbb2222", "Fix flaky test", "/work/alpha"}},
		{"sessions", map[string]any{"project": "beta", "limit": "1"}, []string{"claude/bbbb2222"}},
		{"search", map[string]any{"query": "数据库"}, []string{alpha + "#2", "现在给数据库加索引"}},
		{"search", map[string]any{"query": "ECONNREFUSED", "since": "7d"}, []string{alpha + "#1", "output", "Bash"}},
		{"read", map[string]any{"ref": "aaaa1111"}, []string{"Fix flaky test", "#1", "#2", "fix the flaky test", "Subagents", "Explore"}},
		{"read", map[string]any{"ref": "aaaa1111#1"}, []string{"go test ./...", "[2]", "The test depended on map order"}},
		{"read", map[string]any{"ref": "aaaa1111", "turns": "1-2", "full": true}, []string{"go test ./...", "schema.sql", "索引已加上"}},
		{"read", map[string]any{"ref": "aaaa1111", "seq": 2}, []string{"ECONNREFUSED", "exit status 1"}},
		{"brief", map[string]any{"ref": "aaaa1111"}, []string{"现在给数据库加索引", "schema.sql"}},
		{"brief", map[string]any{"ref": "aaaa1111", "turn": 1}, []string{"fix the flaky test", "go test"}},
		{"pack", map[string]any{"since": "7d"}, []string{"# Session pack", "Fix flaky test", "rotate the deploy key"}},
		{"usage", nil, []string{"Rolling windows", "claude-opus-5", "claude-haiku-5", "/work/alpha"}},
		{"usage", map[string]any{"by": "model"}, []string{"claude-opus-5", "total"}},
		{"failures", nil, []string{"1 failure", "Bash", "go test ./...", alpha + "#1"}},
		{"digest", nil, []string{"Total", "/work/alpha", "/work/beta", "claude-opus-5"}},
		{"files", map[string]any{"path": "schema.sql"}, []string{"changed", alpha + "#2", "/work/alpha/db/schema.sql"}},
		{"projects", nil, []string{"/work/alpha", "/work/beta"}},
	} {
		text, isErr := call(t, s, tc.tool, tc.args)
		if isErr {
			t.Errorf("%s %v: %s", tc.tool, tc.args, text)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s %v: no %q in:\n%s", tc.tool, tc.args, want, text)
			}
		}
		if strings.Contains(text, "\x1b[") {
			t.Errorf("%s: an agent is sent colour codes", tc.tool)
		}
		if strings.Contains(text, "--limit") || strings.Contains(text, "--offset") {
			t.Errorf("%s: a hint names a command-line flag to a caller that has arguments:\n%s", tc.tool, text)
		}
	}
}

func TestWhatGoesWrongIsToldToTheModel(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"sessions", map[string]any{"sice": "7d"}, `takes no argument "sice"`},
		{"sessions", map[string]any{"limit": "many"}, "limit: want a whole number"},
		{"sessions", map[string]any{"since": "yesterdayish"}, "unreadable time"},
		{"sessions", map[string]any{"source": "emacs"}, "unknown source"},
		{"search", nil, "search needs query"},
		{"read", map[string]any{"ref": "zzzz"}, "sessions and search print the refs"},
		{"read", map[string]any{"ref": "aaaa1111", "turns": "seven"}, "turns: want"},
		{"read", map[string]any{"ref": "aaaa1111#9"}, "no such turn (it has 2)"},
		{"read", map[string]any{"ref": "aaaa1111", "seq": 99}, "no step 99"},
		{"usage", map[string]any{"by": "colour"}, "grouped by model, day, source, project or session"},
	} {
		text, isErr := call(t, s, tc.tool, tc.args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s %v: isError=%v %q, want an error saying %q", tc.tool, tc.args, isErr, text, tc.want)
		}
	}
}

func TestSecretsDoNotLeaveWithAnAnswer(t *testing.T) {
	s := server(t)
	for _, args := range []map[string]any{
		{"ref": "bbbb2222", "turns": "all", "full": true, "max_chars": 100000},
		{"ref": "bbbb2222"},
	} {
		text, isErr := call(t, s, "read", args)
		if isErr {
			t.Fatal(text)
		}
		if strings.Contains(text, fixture.Secret) {
			t.Errorf("read %v hands another model the key a transcript held", args)
		}
		if !strings.Contains(text, "[redacted]") {
			t.Errorf("read %v: the place the key was is not marked", args)
		}
	}
	if text, _ := call(t, s, "brief", map[string]any{"ref": "bbbb2222"}); strings.Contains(text, fixture.Secret) {
		t.Error("a brief carries the key")
	}
	if text, _ := call(t, s, "search", map[string]any{"query": "rotated"}); strings.Contains(text, fixture.Secret) {
		t.Error("a search snippet carries the key")
	}
}

func TestAnswersKeepToTheirBudget(t *testing.T) {
	s := server(t)
	text, isErr := call(t, s, "read", map[string]any{"ref": "bbbb2222", "turns": "1", "full": true, "max_chars": 1000})
	if isErr {
		t.Fatal(text)
	}
	if len(text) > 2200 {
		t.Errorf("asked for 1000 characters, got %d", len(text))
	}
	if !strings.Contains(text, "max_chars") && !strings.Contains(text, "left out") {
		t.Errorf("a cut answer says it was cut and what to do:\n%s", text[max(0, len(text)-300):])
	}
	// Uncut, the whole output is there
	whole, _ := call(t, s, "read", map[string]any{"ref": "bbbb2222", "seq": 1, "max_chars": 400000})
	if strings.Count(whole, fixture.LongLine) != 400 {
		t.Errorf("one step asked for whole has %d of its 400 lines", strings.Count(whole, fixture.LongLine))
	}
}

func TestOverHTTP(t *testing.T) {
	s := server(t)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
		return rec
	}
	rec := post(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"projects","arguments":{}}}`)
	var r reply
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &r) != nil || !strings.Contains(r.Result.Content[0].Text, "/work/alpha") {
		t.Errorf("POST = %d %s", rec.Code, rec.Body)
	}
	if rec := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("a notification = %d %q, want 202 and nothing", rec.Code, rec.Body)
	}
	if rec := post(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`); rec.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Errorf("a batch = %d %s", rec.Code, rec.Body)
	}
	if rec := post(`{{{`); rec.Code != http.StatusBadRequest {
		t.Errorf("garbage = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET = %d: no stream is offered", rec.Code)
	}
}

// A client ends the server by closing its input, or with a signal. The second must work
// although the input is still open: a server blocked reading a pipe nobody writes to
// could otherwise only be killed.
func TestServeStopsWhenAsked(t *testing.T) {
	s := server(t)
	in, client := io.Pipe()
	defer client.Close()
	out, replies := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, in, replies) }()

	// It is serving: a ping is answered
	go client.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	line := make([]byte, 4096)
	n, err := out.Read(line)
	if err != nil || !strings.Contains(string(line[:n]), `"id":1`) {
		t.Fatalf("ping = %q, %v", line[:n], err)
	}

	cancel() // the input stays open
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("stopped with %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("asked to stop, and still waiting for its input")
	}
}
