package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/fixture"
)

// asq keep says what it keeps and where; --all keeps every session of the CLIs that delete
// theirs, and doctor says so under the store; a ref keeps one; a label of asq's own is not
// a store's while it keeps.
func TestKeep(t *testing.T) {
	h, index := setup(t)
	state := os.Getenv("ASQ_STATE")
	kept := filepath.Join(state, "kept")
	check := func(args []string, code int, want ...string) {
		t.Helper()
		out, errs, got := asq(t, h, index, args...)
		if got != code {
			t.Errorf("asq %s = %d, want %d\n%s%s", strings.Join(args, " "), got, code, out, errs)
		}
		for _, w := range want {
			if !strings.Contains(out+errs, w) {
				t.Errorf("asq %s: no %q in\n%s%s", strings.Join(args, " "), w, out, errs)
			}
		}
	}
	check([]string{"keep"}, 0, "asq keeps no copies. Claude Code and Gemini CLI delete the sessions not used for 30 days by default")
	check([]string{"keep", "--all"}, 0, "asq keeps a copy of every session of Claude Code and Gemini CLI from now on", "3 copied now", "in "+kept+": 4 files")
	check([]string{"keep"}, 0, "claude@kept", "3 transcripts")
	check([]string{"doctor"}, 0, "; asq keeps a copy of each (asq keep)", "claude@kept     3 kept, and the CLI still holds every one")
	check([]string{"keep", "--drop", "claude/" + fixture.Alpha[:8]}, 0, "still kept: asq keeps every session of Claude Code")
	check([]string{"keep", "claude/x1"}, 1, "subagent")
	check([]string{"ls", "--path", "claude@kept=" + t.TempDir()}, 2, "claude@kept is what asq calls the sessions it keeps")
	check([]string{"keep", "--off"}, 0, "asq keeps nothing more: what it kept stays")
	check([]string{"keep", "--all", "--off"}, 2, "--all or --off")
}

// An outline says whether its session is kept, and how to keep it — as an agent's read
// says, in its own terms — before anything is, and after.
func TestShowSaysWhetherASessionIsKept(t *testing.T) {
	h, index := setup(t)
	ref := "claude/" + fixture.Alpha
	show := func(want string) {
		t.Helper()
		out, errs, got := asq(t, h, index, "show", ref, "--max-chars", "2000")
		if got != 0 {
			t.Fatalf("asq show = %d\n%s%s", got, out, errs)
		}
		if !strings.Contains(out, want) {
			t.Errorf("asq show: no %q in\n%s", want, out)
		}
	}
	show("not kept — its CLI deletes old sessions of its own accord; asq keep " + ref + " keeps a copy")
	if out, errs, got := asq(t, h, index, "keep", ref); got != 0 {
		t.Fatalf("asq keep = %d\n%s%s", got, out, errs)
	}
	show("kept one by one: asq has a copy")
}
