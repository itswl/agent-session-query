package app

import (
	"os/exec"
	"testing"
)

// TestUIScriptsParse: app.js is hand-written with no build step, and nothing else in the
// suite reads it as code — the asset tests only grep it for forbidden sinks, so a syntax
// error used to ship with every Go test green (verified by appending one). node --check
// is the cheapest real parse of the file; where node is not installed the check skips,
// and CI (.github/workflows/test.yml) runs it on runners that have it.
func TestUIScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; CI runs this check on runners that have it")
	}
	for _, name := range []string{"ui/app.js", "ui/theme.js"} {
		out, err := exec.Command(node, "--check", name).CombinedOutput()
		if err != nil {
			t.Fatalf("node --check %s: %v\n%s", name, err, out)
		}
	}
}
