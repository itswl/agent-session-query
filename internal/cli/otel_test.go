package cli

import (
	"io"
	"strings"
	"testing"
)

// A dry run at a terminal says what it would send and how to read the requests, rather than
// pouring out lines of JSON hundreds of kilobytes long; asked for JSON, or piped, it prints
// the requests.
func TestADryRunAtATerminalSaysWhatItWouldSend(t *testing.T) {
	h, indexPath := setup(t)
	defer func(was func(io.Writer) bool) { toTerminal = was }(toTerminal)
	toTerminal = func(io.Writer) bool { return true }

	out, errs, code := asq(t, h, indexPath, "otel", "--dry-run", "--since", "all")
	if code != 0 || out != "" || !strings.Contains(errs, "would send ") || !strings.Contains(errs, "pipe them to jq or to a file") {
		t.Errorf("a dry run at a terminal: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code = asq(t, h, indexPath, "otel", "--dry-run", "--since", "all", "--json")
	if code != 0 || !strings.Contains(out, `"resourceSpans"`) || strings.Contains(errs, "pipe them") {
		t.Errorf("a dry run at a terminal, asked for JSON: exit %d\n%.300s%s", code, out, errs)
	}
	toTerminal = func(io.Writer) bool { return false }
	out, errs, code = asq(t, h, indexPath, "otel", "--dry-run", "--since", "all")
	if code != 0 || !strings.Contains(out, `"resourceSpans"`) || strings.Contains(errs, "pipe them") {
		t.Errorf("a dry run into a pipe: exit %d\n%.300s%s", code, out, errs)
	}
}
