package source

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SearchQuery is one content search.
type SearchQuery struct {
	Needle     string    // kept verbatim, echoed back to the caller
	Lowered    []byte    // the needle folded for matching (FoldNeedle)
	Limit      int       // how many sessions to return at most
	PerSession int       // how many hits per session at most
	Since      time.Time // only search sessions updated after this; zero means no limit
	Until      time.Time // only search sessions updated before this; zero means no limit
	// Source, Project and Branch narrow which sessions are scanned, with the meanings
	// list_sessions gives them: a mode or one labeled instance, and substrings of the
	// project and of the branch the session opened on. They narrow before the scan rather
	// than after it, so limit counts sessions inside the selection — a page cut first and
	// filtered second could hold none of the sessions asked about, the newer ones elsewhere
	// having filled it.
	Source  string
	Project string
	Branch  string
	// Pattern limits the search to the one session it names, found the same way
	// get_session finds it (a full id, a fragment, a path fragment). Without it a search
	// spans every session; with it, "where in this session did we discuss X" is one call
	// instead of paging a 16 000-message session fifty at a time.
	Pattern string
	// Role keeps only hits from messages with that role — the human words rather than
	// the answer that repeats them. Applied while collecting, not after, so per_session
	// counts hits that match rather than hits that happen to come first.
	Role string
	// NoSubagents leaves subagents' transcripts out. They are searched by default — the work
	// a session handed to its agents happened there, commands and output included — and a
	// hit in one comes back in the result of the session it ran in, naming the agent.
	NoSubagents bool
	// IncludeArchived reads the sessions their CLI has archived too; left out by default,
	// as each CLI leaves them out of its own list
	IncludeArchived bool
}

// ProbeLimit is what a source actually fetches per session: one hit more than the caller
// asked for.
//
// That extra hit is how "there were more" is known. Without it a source that returns
// exactly per_session hits is indistinguishable from one that ran out of file at exactly
// that point, and the alternative — every source reporting a flag of its own — would mean
// widening searchableSource and touching all five places that cap. The length says it
// instead, and search() trims before anything leaves.
func (q SearchQuery) ProbeLimit() int {
	if q.PerSession <= 0 {
		return 0
	}
	return q.PerSession + 1
}

// LikePatterns are the LIKE patterns a SQLite prefilter tries for this query, before the
// fold confirms what they let through (IndexFold). SQLite's LIKE ignores case for ASCII
// alone, so an ASCII needle needs one pattern, as it always did; one beyond ASCII is looked
// for in the spellings text is written in — as typed, lowercase, uppercase, a capital first
// and a capital per word. A text spelled some other way ("ÉcOLE") slips the prefilter, the
// line t3code's fix for the same SQLite limit draws too.
func (q SearchQuery) LikePatterns() []string {
	folded := string(q.Lowered)
	if isASCII(folded) {
		return []string{"%" + EscapeLike(folded) + "%"}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, spelling := range []string{q.Needle, folded, strings.ToUpper(folded), capitalFirst(folded), capitalWords(folded)} {
		if spelling != "" && !seen[spelling] {
			seen[spelling] = true
			out = append(out, "%"+EscapeLike(spelling)+"%")
		}
	}
	return out
}

// LikeAny is the WHERE clause matching column against n LIKE patterns, any of them —
// placeholders for LikePatterns' patterns, in their order
func LikeAny(column string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = column + " LIKE ? ESCAPE '\\'"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// likeArgs are the arguments of a session query whose placeholders run: the session, the
// LIKE patterns, the limit
func likeArgs(sessionID string, likes []string, limit int) []any {
	args := make([]any, 0, len(likes)+2)
	args = append(args, sessionID)
	for _, like := range likes {
		args = append(args, like)
	}
	return append(args, limit)
}

// capitalFirst uppercases the first character
func capitalFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// capitalWords uppercases the first character of every word
func capitalWords(s string) string {
	words := strings.Split(s, " ")
	for i, w := range words {
		words[i] = capitalFirst(w)
	}
	return strings.Join(words, " ")
}
