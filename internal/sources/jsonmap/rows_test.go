package jsonmap

import (
	"math"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
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
