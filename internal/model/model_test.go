package model

import (
	"math"
	"testing"
)

// What a count is, whatever a transcript says.
func TestTokens(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 1: 1, 4096: 4096, MaxTokens: MaxTokens, MaxTokens + 1: 0, -1: 0, math.MaxInt64: 0, math.MinInt64: 0} {
		if got := Tokens(in); got != want {
			t.Errorf("Tokens(%d) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[float64]int64{0: 0, 12.7: 12, MaxTokens: MaxTokens, MaxTokens + 1: 0, -0.5: 0, 1e19: 0, -1e300: 0, math.Inf(1): 0} {
		if got := TokensOf(in); got != want {
			t.Errorf("TokensOf(%g) = %d, want %d", in, got, want)
		}
	}
	if got := TokensOf(math.NaN()); got != 0 {
		t.Errorf("TokensOf(NaN) = %d", got)
	}
}

// A total holds at the most an int64 can, rather than turning negative.
func TestUsageAddDoesNotWrap(t *testing.T) {
	u := Usage{Input: math.MaxInt64 - 1, Output: 5}
	u.Add(Usage{Input: 10, Output: 7, Reasoning: 2})
	if u.Input != math.MaxInt64 || u.Output != 12 || u.Reasoning != 2 {
		t.Errorf("sum = %+v", u)
	}
}

// What a duration is, whatever a transcript says.
func TestDurMs(t *testing.T) {
	for in, want := range map[int64]int64{1: 1, 1500: 1500, MaxDurMs: MaxDurMs, MaxDurMs + 1: 0, 0: 0, -1: 0, math.MaxInt64: 0, math.MinInt64: 0} {
		if got := DurMs(in); got != want {
			t.Errorf("DurMs(%d) = %d, want %d", in, got, want)
		}
	}
}
