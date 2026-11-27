package cli

import (
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/fixture"
)

// A tool call's line says how many pictures its result carried, for a person as for the
// page: a screenshot is otherwise a call that seems to have returned nothing. The numbers
// of a step's files are an agent's to ask by, and are not printed to a person.
func TestShowSaysHowManyPicturesAStepCarried(t *testing.T) {
	h, index := setup(t)
	h.WritePictures(t)
	ref := "claude/" + fixture.Pictured
	out, errs, got := asq(t, h, index, "show", ref, "--turns", "1")
	if got != 0 {
		t.Fatalf("asq show = %d\n%s%s", got, out, errs)
	}
	// The screenshot's two, and the Read's two — an SVG and an SVG calling itself a PNG,
	// which count as the transcript names them; the reply carries none
	if !strings.Contains(out, "✓ · 2 pictures  [1]") || !strings.Contains(out, "✓ · 2 pictures  [2]") || strings.Count(out, "picture") != 2 || strings.Contains(out, "files:") {
		t.Errorf("asq show --turns 1:\n%s", out)
	}
	out, errs, got = asq(t, h, index, "show", ref, "--seq", "1")
	if got != 0 {
		t.Fatalf("asq show --seq = %d\n%s%s", got, out, errs)
	}
	if !strings.Contains(out, "✓ · 2 pictures  [1]") || !strings.Contains(out, "[image · gif · 43 bytes]") || strings.Contains(out, "files:") || strings.Contains(out, "picture=") {
		t.Errorf("asq show --seq 1:\n%s", out)
	}
}
