package app

import "strings"

// Usage, folded into one shape.
//
// The eight sources report the same quantities under different names — claude's
// cache_read_input_tokens, codex's cached_input_tokens, pi's cacheRead, gemini's cached,
// grok's cachedReadTokens — and some bundle fields the card has no use for (service_tier,
// speed, iterations). The UI used to render whatever came through, which is why one
// session showed "in / out" beside raw "cache_read_input_tokens" while another showed
// "cache read".
//
// Normalising here also makes the numbers addable, which is what lets a session report a
// total rather than only its last turn: claude's final message says in 312 / out 1201
// while the session as a whole used 8.1M in / 2.5M out over 2396 turns. Both are true;
// only one of them is what "usage" means to a reader.
//
// Keys are folded to lowercase without underscores before lookup, so snake_case and
// camelCase spellings of the same field land on one entry.
var usageFieldAliases = map[string]string{
	"inputtokens": "inputTokens", "input": "inputTokens", "prompttokens": "inputTokens",
	"outputtokens": "outputTokens", "output": "outputTokens", "completiontokens": "outputTokens",
	"cachereadinputtokens": "cacheReadTokens", "cachedinputtokens": "cacheReadTokens",
	"cacheread": "cacheReadTokens", "cached": "cacheReadTokens", "cachedtokens": "cacheReadTokens",
	"cachedreadtokens":         "cacheReadTokens",
	"cachecreationinputtokens": "cacheWriteTokens", "cachewriteinputtokens": "cacheWriteTokens",
	"cachewrite": "cacheWriteTokens", "cachecreationtokens": "cacheWriteTokens",
	"reasoningtokens": "reasoningTokens", "reasoningoutputtokens": "reasoningTokens",
	"reasoning": "reasoningTokens", "thoughts": "reasoningTokens",
	"totaltokens": "totalTokens", "total": "totalTokens",
	"estimatedcostusd": "estimatedCostUsd", "costusd": "estimatedCostUsd",
}

// usageOrder keeps the emitted map's keys in a sensible reading order; the UI sorts by
// its own label table anyway, but a stable shape is easier to test against.
var usageOrder = []string{
	"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens",
	"reasoningTokens", "totalTokens", "estimatedCostUsd",
}

// normalizeUsage maps one provider's usage object onto the shared names, dropping
// everything else. Nested objects are not recursed into except for a numeric cost.total,
// which is where pi puts the figure.
func normalizeUsage(raw map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range raw {
		folded := strings.ReplaceAll(strings.ToLower(key), "_", "")
		name, known := usageFieldAliases[folded]

		if !known && (folded == "cost" || folded == "costs") {
			// pi: {"cost": {"total": 0.0123, ...}}; some writers put the number directly
			if nested, ok := value.(map[string]any); ok {
				if total, ok := toFloat(nested["total"]); ok {
					out["estimatedCostUsd"] = total
				}
				continue
			}
			name = "estimatedCostUsd"
		}
		if !known && name == "" {
			continue // service_tier, speed, iterations, nested detail objects, ...
		}
		if _, isNumber := toFloat(value); !isNumber {
			continue
		}
		out[name] = value
	}
	return out
}

// usageTotals adds up a session's per-message usage.
type usageTotals struct {
	sums map[string]float64
	seen bool
}

// add folds one message's usage in. A nil or unrecognised object contributes nothing.
func (t *usageTotals) add(raw map[string]any) {
	if len(raw) == 0 {
		return
	}
	normalized := normalizeUsage(raw)
	if len(normalized) == 0 {
		return
	}
	if t.sums == nil {
		t.sums = map[string]float64{}
	}
	for name, value := range normalized {
		if n, ok := toFloat(value); ok {
			t.sums[name] += n
		}
	}
	t.seen = true
}

// result is the session total, or nil when no message carried usage at all.
func (t *usageTotals) result() map[string]any {
	if !t.seen || len(t.sums) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, name := range usageOrder {
		sum, ok := t.sums[name]
		if !ok {
			continue
		}
		if name == "estimatedCostUsd" {
			out[name] = sum
			continue
		}
		out[name] = int64(sum)
	}
	return out
}
