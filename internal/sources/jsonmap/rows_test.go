package jsonmap

import (
	"math"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Two keys that fold to one counter — input beside input_tokens — give the larger, and so
// do two costs: the map is walked in a different order each time, and the same row must
// read the same every time it is read.
func TestUsageOfKeysThatFoldTogether(t *testing.T) {
	raw := map[string]any{"input": 5.0, "input_tokens": 7.0, "outputTokens": 3.0, "output": 2.0,
		"cost": 0.5, "Cost": map[string]any{"total": 0.7}}
	for range 100 {
		if u, cost := Usage(raw); u != (model.Usage{Input: 7, Output: 3}) || cost != 0.7 {
			t.Fatalf("usage = %+v, cost %v; want the larger of each", u, cost)
		}
	}
}

// A counter is a count by model.TokensOf's rule: one below zero or past model.MaxTokens
// is none, not the bound, and the bound itself is a count.
func TestUsageCountsByTheSharedRule(t *testing.T) {
	u, _ := Usage(map[string]any{"input": -5.0, "output": float64(model.MaxTokens),
		"cacheRead": float64(model.MaxTokens + 1), "cacheWrite": 9e18, "reasoning": 12.7})
	if want := (model.Usage{Output: model.MaxTokens, Reasoning: 12}); u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

// A price is one above zero, finite and short of maxCost; anything else, added to a
// session's total, would take it below zero or to infinity, and is none.
func TestCost(t *testing.T) {
	for in, want := range map[any]float64{0.25: 0.25, 1: 1, maxCost: maxCost, int64(3): 3,
		0.0: 0, -0.5: 0, maxCost * 2: 0, 1e300: 0, math.Inf(1): 0, math.Inf(-1): 0, "0.5": 0, nil: 0} {
		if got := Cost(in); got != want {
			t.Errorf("Cost(%v) = %v, want %v", in, got, want)
		}
	}
	if got := Cost(math.NaN()); got != 0 {
		t.Errorf("Cost(NaN) = %v", got)
	}
}

// The silent reply is the token alone, as OpenClaw takes it: in any case, repeated, with
// punctuation at its edges. A reply that says it among other words said something.
func TestIsSilent(t *testing.T) {
	for text, want := range map[string]bool{"NO_REPLY": true, " no_reply.\n": true, "NO_REPLY NO_REPLY": true, "**NO_REPLY**": true,
		"": false, "  ": false, "NO_REPLY, the build passed": false, "NO_REPLYING": false, "DONE": false} {
		if got := isSilent(text); got != want {
			t.Errorf("isSilent(%q) = %v, want %v", text, got, want)
		}
	}
}

// A picture in a typed message is marked where it came: after the words of the person's
// message, after the output of the tool that returned it — in a result message or a result
// block — as what its bytes say it is where the row does not.
func TestPicturesAreMarked(t *testing.T) {
	const png = "iVBORw0KGgo="
	var r Reader
	c := &transcript.Collector{}
	w := transcript.New(c)
	for i, line := range []string{
		`{"type":"message","message":{"role":"user","content":[{"type":"image","data":"` + png + `"},{"type":"text","text":"look"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"browser"},{"type":"toolCall","id":"c2","name":"read"}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"c1","toolName":"browser","content":[{"type":"text","text":"shot"},{"type":"image","data":"` + png + `","mimeType":"image/png"}]}}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"toolResult","toolCallId":"c2","toolName":"read","content":[{"type":"image","data":"` + png + `"}]}]}}`,
	} {
		m, ok := r.Row([]byte(line))
		if !ok {
			t.Fatalf("row %d not read", i)
		}
		w.Add(m)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	turn := c.Turns[0]
	if turn.Ask != "look\n[image · png · 8 bytes]" {
		t.Errorf("ask = %q", turn.Ask)
	}
	for i, want := range []string{"shot\n[image · png · 8 bytes]", "[image · png · 8 bytes]"} {
		it := turn.Items[1+i]
		if it.Tool == nil || it.Tool.Output != want || len(it.Attachments) != 1 {
			t.Errorf("result %d = %+v, want %q", i, it, want)
		}
	}
}
