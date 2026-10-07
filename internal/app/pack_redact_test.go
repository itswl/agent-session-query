package app

import (
	"strings"
	"testing"
)

// packSecret is secret-shaped: a known key prefix followed by a long mixed-case,
// digit-bearing run — what redactSecrets is built to catch.
var packSecret = "sk-" + strings.Repeat("aB3dE", 8)

// secretSource is a minimal SessionSource whose final result quotes a key, so the pack's
// redaction is pinned without depending on one CLI's file format.
type secretSource struct{}

func (secretSource) Mode() string                                   { return "pi" }
func (secretSource) Location() string                               { return "test" }
func (secretSource) Exists() bool                                   { return true }
func (secretSource) List() []record                                 { return nil }
func (secretSource) Messages(record, messageQuery) []map[string]any { return nil }
func (secretSource) Final(record) map[string]any {
	return map[string]any{"text": "done — token " + packSecret, "isFinal": true, "stopReason": "stop"}
}

// TestPackAssembledTextIsRedacted: a pack is assembled text meant to be handed to
// another agent, and the docs promise redaction over what this service assembles. The
// asked/concluded lines travel through packQuote/packSnippet, and the JSONL pack's
// concluded field is the same assembled line — all of them must be cleaned. Full
// transcripts (mode=full, /export) stay as stored, which is a separate path.
func TestPackAssembledTextIsRedacted(t *testing.T) {
	rec := newRecord(record{
		Source: "pi", Key: "k1", ShortKey: "fix the deploy",
	}, "")

	if got := packQuote("conclusion: " + packSecret); strings.Contains(got, packSecret) {
		t.Fatalf("packQuote handed out a secret: %s", got)
	}
	if got := packSnippet("opening: " + packSecret); strings.Contains(got, packSecret) {
		t.Fatalf("packSnippet handed out a secret: %s", got)
	}

	out := renderPackJSONL([]packEntry{{source: secretSource{}, rec: rec}}, packSummary{})
	if strings.Contains(out, packSecret) {
		t.Fatalf("the JSONL pack must not carry a key through its concluded line:\n%s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("the concluded line should carry [redacted]:\n%s", out)
	}
}
