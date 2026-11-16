package kit

import (
	"encoding/json"
	"github.com/itswl/agent-session-query/internal/model"
	"math"
	"strconv"
	"strings"
	"time"
)

// Transcripts are other programs' JSON, and their fields change type between versions.
// These read a value as the type wanted and fall back to the zero value, so an adapter
// says what it expects once and is not littered with type assertions.

// Str is v when it is a string, and "" otherwise.
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Text renders a scalar as text: strings as they are, numbers without an exponent.
func Text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// Map is v when it is an object, and an empty one otherwise — never nil, so a lookup on
// the result is always safe.
func Map(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// List is v when it is an array.
func List(v any) []any {
	l, _ := v.([]any)
	return l
}

// Num reads a number, whichever numeric type it arrived as.
func Num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// Int reads a count; anything that is not a number is zero.
func Int(v any) int64 {
	n, _ := Num(v)
	// A float64 outside an int64's range converts to whatever the platform makes of it —
	// the largest int64 on arm64, the smallest on amd64 — so it is not converted at all
	if !(n >= math.MinInt64 && n < math.MaxInt64) { // NaN fails both
		return 0
	}
	return int64(n)
}

// Count is a token count from a JSON value: see model.TokensOf. 0 for anything that is
// not a number, is below zero, or is past what one response can count.
func Count(v any) int64 {
	n, _ := Num(v)
	return model.TokensOf(n)
}

// Truthy: nil, false, 0, "" and empty containers are false.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// First is the first non-empty string.
func First(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ContentText flattens the shapes "content" takes across CLIs — a string, an array of
// blocks, a single block — into plain text, taking each block's text, content or
// thinking.
func ContentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		for _, key := range []string{"text", "content", "thinking"} {
			switch inner := t[key].(type) {
			case string:
				return inner
			case []any:
				return ContentText(inner)
			}
		}
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if text := ContentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// timeLayouts are the shapes seen across CLIs, most common first. One without a zone is
// UTC: every CLI writes UTC.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

// Time reads a timestamp in any of the forms the CLIs write: an RFC 3339 string, a
// zoneless one, or an epoch count in any unit, as a number or as a string of one. The zero time means it could not be read.
func Time(v any) time.Time {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}
		}
		for _, layout := range timeLayouts {
			if parsed, err := time.Parse(layout, s); err == nil {
				return parsed.UTC()
			}
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return Epoch(f)
		}
	case float64, int, int64, json.Number:
		f, _ := Num(v)
		return Epoch(f)
	}
	return time.Time{}
}

// Epoch converts an epoch count to UTC, whatever unit it was written in. The magnitude
// says which: a count of seconds past 1e11 would be the year 5138, so past it the count
// is milliseconds; past 1e14 microseconds; past 1e17 nanoseconds. Each boundary is three
// thousand years from any date a transcript holds, so none can be mistaken.
//
// The instant is counted in whole microseconds rather than carried through a fraction of
// a second: 1791312688214 ms divided by a thousand is 1791312688.2139999866 in a float,
// which lands 14 ns before the instant that was written.
func Epoch(v float64) time.Time {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return time.Time{}
	}
	var micros float64
	switch abs := math.Abs(v); {
	case abs > 1e17:
		micros = v / 1e3
	case abs > 1e14:
		micros = v
	case abs > 1e11:
		micros = v * 1e3
	default:
		micros = v * 1e6
	}
	if sec := micros / 1e6; sec < -62135596800 || sec > 253402300799 {
		return time.Time{}
	}
	return time.UnixMicro(int64(math.Round(micros))).UTC()
}
