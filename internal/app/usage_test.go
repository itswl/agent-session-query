package app

import "testing"

// TestNormalizeUsage: each source spells the same quantities differently. Every shape
// below was copied from a real session record, not invented.
func TestNormalizeUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want map[string]any
	}{
		{
			name: "claude",
			raw: map[string]any{
				"input_tokens": 577, "output_tokens": 819,
				"cache_read_input_tokens": 564736, "cache_creation_input_tokens": 0,
				"service_tier": "standard", "speed": "standard",
				"iterations": []any{}, "inference_geo": "",
				"output_tokens_details": map[string]any{"thinking_tokens": 12},
			},
			want: map[string]any{
				"inputTokens": 577, "outputTokens": 819,
				"cacheReadTokens": 564736, "cacheWriteTokens": 0,
			},
		},
		{
			name: "codex",
			raw: map[string]any{
				"input_tokens": 15109, "cached_input_tokens": 11965,
				"cache_write_input_tokens": 3141, "output_tokens": 367,
				"reasoning_output_tokens": 108, "total_tokens": 15476,
			},
			want: map[string]any{
				"inputTokens": 15109, "cacheReadTokens": 11965, "cacheWriteTokens": 3141,
				"outputTokens": 367, "reasoningTokens": 108, "totalTokens": 15476,
			},
		},
		{
			name: "pi",
			raw: map[string]any{
				"input": 6151, "output": 83, "cacheRead": 128, "cacheWrite": 0,
				"reasoning": 52, "totalTokens": 6362,
				"cost": map[string]any{"total": 0.0123, "input": 0.0086},
			},
			want: map[string]any{
				"inputTokens": 6151, "outputTokens": 83, "cacheReadTokens": 128,
				"cacheWriteTokens": 0, "reasoningTokens": 52, "totalTokens": 6362,
				"estimatedCostUsd": 0.0123,
			},
		},
		{
			name: "gemini",
			raw:  map[string]any{"input": 15597, "output": 133, "cached": 0, "thoughts": 396, "tool": 0, "total": 16126},
			want: map[string]any{
				"inputTokens": 15597, "outputTokens": 133, "cacheReadTokens": 0,
				"reasoningTokens": 396, "totalTokens": 16126,
			},
		},
		{
			name: "already normalised passes through",
			raw:  map[string]any{"inputTokens": 5, "estimatedCostUsd": 0.5},
			want: map[string]any{"inputTokens": 5, "estimatedCostUsd": 0.5},
		},
		{
			name: "nothing recognisable",
			raw:  map[string]any{"service_tier": "standard", "nested": map[string]any{"a": 1}},
			want: map[string]any{},
		},
	}
	for _, c := range cases {
		got := normalizeUsage(c.raw)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, got[k], v)
			}
		}
	}
}

func TestUsageTotalsSum(t *testing.T) {
	var totals usageTotals
	totals.add(map[string]any{"input_tokens": 10, "output_tokens": 1})
	totals.add(map[string]any{"input_tokens": 5, "cache_read_input_tokens": 100})
	totals.add(nil)                                 // a message without usage contributes nothing
	totals.add(map[string]any{"service_tier": "x"}) // nor does one with nothing recognisable

	got := totals.result()
	if got["inputTokens"] != int64(15) || got["outputTokens"] != int64(1) || got["cacheReadTokens"] != int64(100) {
		t.Fatalf("totals = %v", got)
	}
	// A session where nothing carried usage reports none, rather than a card of zeroes
	var empty usageTotals
	if empty.result() != nil {
		t.Fatalf("no usage should be nil, got %v", empty.result())
	}
}
