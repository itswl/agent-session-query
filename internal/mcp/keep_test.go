package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// withState is the MCP server over the fixture's history, given a state directory of its
// own — as asq mcp and asq serve are — where what keep keeps goes.
func withState(t *testing.T) (*Server, fixture.History) {
	t.Helper()
	h := fixture.Write(t)
	cfg := h.Config(t)
	cfg.State = t.TempDir()
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return New(a, io.Discard), h
}

// keep is asq keep for an agent: a copy of one session and its subagents, made now, of
// which the answer says where and how much; or no longer keeping it, the copy deleted
// while its CLI holds the session, and kept where the CLI has deleted it. What cannot be
// kept says why. ref "this" says which session it took before what it did. A server on
// stdio and one answering HTTP, /mcp itself, both write; the outline of read says whether
// there is something to keep, before and after.
func TestKeep(t *testing.T) {
	s, h := withState(t)
	alpha, agent, beta := "claude/"+fixture.Alpha, "claude/"+fixture.Agent, "claude/"+fixture.Beta
	folder := func() string {
		ks, err := s.App.KeepStatus(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return ks.Folder
	}
	const copies = " copied now. It is copied again a few minutes after it changes."

	// Nothing kept yet: the outline says so, and how to keep it, in the tool's terms
	text, isErr := call(t, s, "read", map[string]any{"ref": alpha})
	if isErr || !strings.Contains(text, "not kept — its CLI deletes old sessions of its own accord; keep(ref, keep=true) keeps a copy") {
		t.Errorf("read before anything is kept: isError=%v\n%s", isErr, text)
	}

	// On stdio, as Claude Code starts it: "this" is the session it named
	s.Caller = &Caller{Session: fixture.Alpha, name: "claude-code"}
	text, isErr = call(t, s, "keep", map[string]any{"ref": "this", "keep": true})
	if want := `[ref "this" is ` + alpha + `: the session Claude Code named when it started this server (CLAUDE_CODE_SESSION_ID).]` + "\n\nkept " + alpha +
		", and its subagents with it, in " + folder() + ": "; isErr || !strings.HasPrefix(text, want) || !strings.Contains(text, copies) {
		t.Errorf("keep this: isError=%v\n%s\nwant it to open with %q", isErr, text, want)
	}
	// "this" is found as the agent's own session, not resolved by its ref, and says how it
	// is kept all the same: a read of it is not to call a kept session "not kept"
	if text, isErr = call(t, s, "read", map[string]any{"ref": "this"}); isErr || !strings.Contains(text, "kept one by one: asq has a copy") || strings.Contains(text, "not kept") {
		t.Errorf("read this, kept: isError=%v\n%s", isErr, text)
	}

	// Asked again, it says the session was kept already: what is copied now is only what
	// changed since, and is not to be taken for the session's size
	if text, isErr = call(t, s, "keep", map[string]any{"ref": alpha, "keep": true}); isErr || !strings.HasPrefix(text, "already kept one by one: "+alpha+", and its subagents with it, in "+folder()+" — ") {
		t.Errorf("keep again: isError=%v %q", isErr, text)
	}
	if text, isErr = call(t, s, "keep", map[string]any{"ref": "this", "keep": true}); isErr || !strings.Contains(text, "\n\nalready kept one by one: "+alpha+", and its subagents with it, in ") {
		t.Errorf("keep this again: isError=%v %q", isErr, text)
	}
	in := filepath.Join(folder(), "claude", "-work-alpha")
	live, err := os.Stat(filepath.Join(h.Dir, "-work-alpha", fixture.Alpha+".jsonl"))
	if copy, err2 := os.Stat(filepath.Join(in, fixture.Alpha+".jsonl")); err != nil || err2 != nil || copy.Size() != live.Size() {
		t.Errorf("the copy = %v, %v; the transcript = %v, %v", copy, err2, live, err)
	}
	if _, err := os.Stat(filepath.Join(in, fixture.Alpha, "subagents", "agent-"+fixture.Agent+".meta.json")); err != nil {
		t.Errorf("the subagent's transcript was not kept with its session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(folder(), "claude", "-work-beta")); !os.IsNotExist(err) {
		t.Errorf("a session not named was kept too: %v", err)
	}
	if text, _ := call(t, s, "read", map[string]any{"ref": alpha}); !strings.Contains(text, "kept one by one: asq has a copy") {
		t.Errorf("read after keeping does not say so:\n%s", text)
	}

	// No longer kept, while its CLI holds it: the copy is deleted
	s.Caller = nil
	if text, isErr = call(t, s, "keep", map[string]any{"ref": alpha, "keep": false}); isErr || text != alpha+" is no longer kept, and its copy is deleted: its CLI still holds it." {
		t.Errorf("keep false: isError=%v %q", isErr, text)
	}
	if _, err := os.Stat(filepath.Join(in, fixture.Alpha+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("the copy of a session its CLI still holds stays: %v", err)
	}
	if text, _ := call(t, s, "read", map[string]any{"ref": alpha}); !strings.Contains(text, "not kept — ") {
		t.Errorf("read after no longer keeping does not say so:\n%s", text)
	}

	// Kept with every session of its CLI, it stays kept when it is no longer kept one by one
	if _, err := s.App.KeepAll(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if text, _ := call(t, s, "read", map[string]any{"ref": alpha}); !strings.Contains(text, "kept: asq keeps a copy of every session of its CLI") {
		t.Errorf("read of a session kept with all does not say so:\n%s", text)
	}
	if text, isErr = call(t, s, "keep", map[string]any{"ref": alpha, "keep": true}); isErr || !strings.HasPrefix(text, "kept one by one now, as well as with every session of Claude Code, which keeps a copy of it already: "+alpha+", and its subagents with it, in "+folder()+" — ") {
		t.Errorf("keep one that is kept with all: isError=%v %q", isErr, text)
	}
	if text, isErr = call(t, s, "keep", map[string]any{"ref": alpha, "keep": false}); isErr || text != alpha+" is no longer kept one by one, and is still kept: asq keeps every session of Claude Code, until the person runs asq keep --off." {
		t.Errorf("keep false of one kept with all: isError=%v %q", isErr, text)
	}
	if _, err := s.App.KeepAll(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	// Stopping what was never kept says that nothing changed
	if text, isErr = call(t, s, "keep", map[string]any{"ref": alpha, "keep": false}); isErr || text != alpha+" was not kept, and asq holds no copy of it: its CLI still holds it." {
		t.Errorf("keep false of what was never kept: isError=%v %q", isErr, text)
	}

	// Kept over /mcp itself, then its CLI deletes it: the copy is all there is of it, is
	// read in the session's place, and stays when it is no longer kept
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"keep","arguments":{"ref":"` + beta + `","keep":true}}}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != http.StatusOK || r.Result.IsError || len(r.Result.Content) != 1 ||
		!strings.HasPrefix(r.Result.Content[0].Text, "kept "+beta+", in "+folder()+": ") {
		t.Errorf("POST /mcp keep = %d %s", rec.Code, rec.Body)
	}
	if err := os.Remove(filepath.Join(h.Dir, "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.App.Sync(t.Context(), index.SyncOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	gone := "claude@kept/" + fixture.Beta
	if text, _ := call(t, s, "read", map[string]any{"ref": beta}); !strings.Contains(text, "kept: its CLI has deleted the session, and this is asq's copy") {
		t.Errorf("read of a session only asq holds does not say so:\n%s", text)
	}
	if text, isErr := call(t, s, "keep", map[string]any{"ref": beta, "keep": true}); !isErr || !strings.Contains(text, "it is a copy asq keeps already") {
		t.Errorf("keep what is a copy already: isError=%v %q", isErr, text)
	}
	if text, isErr = call(t, s, "keep", map[string]any{"ref": beta, "keep": false}); isErr || text != gone+" is no longer kept one by one. Its CLI has deleted it, so its copy is all there is of it, and stays in "+folder()+
		": deleting it there is the person's to do." {
		t.Errorf("keep false of what only asq holds: isError=%v %q", isErr, text)
	}
	if _, err := os.Stat(filepath.Join(folder(), "claude", "-work-beta", fixture.Beta+".jsonl")); err != nil {
		t.Errorf("the only copy of a session went: %v", err)
	}

	// What is not a session asq keeps a copy of says so, by its ref
	if text, isErr = call(t, s, "keep", map[string]any{"ref": agent, "keep": true}); !isErr || !strings.HasPrefix(text, agent+": it is a subagent's transcript: keep the session it ran in, "+alpha+", and its subagents are kept with it") {
		t.Errorf("keep a subagent's transcript: isError=%v %q", isErr, text)
	}
	// The argument is read leniently, as every boolean is
	if text, isErr = call(t, s, "keep", map[string]any{"ref": beta, "keep": "no"}); isErr || !strings.HasPrefix(text, gone+" is no longer kept one by one. Its CLI has deleted it") {
		t.Errorf(`keep "no": isError=%v %q`, isErr, text)
	}
}

// A model that has not loaded the tools is told of the one that writes before it calls
// anything, and the tool says so where a client shows descriptions: that keep writes in
// asq's own folder, and that the person is asked first.
func TestKeepSaysItWritesAndAsksFirst(t *testing.T) {
	for _, want := range []string{"keep is the one tool that writes", "Ask the person before you call it"} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("the instructions do not say %q", want)
		}
	}
	for _, tool := range describe() {
		if tool["name"] != "keep" {
			continue
		}
		required, _ := tool["inputSchema"].(map[string]any)["required"].([]string)
		if len(required) != 2 || required[0] != "ref" || required[1] != "keep" {
			t.Errorf("keep's required arguments = %v: a call is to say whether to keep, not leave it to a default", required)
		}
		for _, want := range []string{"The one tool that writes", "Ask the person before you call it", "which takes disk"} {
			if !strings.Contains(tool["description"].(string), want) {
				t.Errorf("keep's description does not say %q", want)
			}
		}
	}
}
