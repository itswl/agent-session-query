package source

import "time"

// SearchQuery is one content search.
type SearchQuery struct {
	Needle     string    // kept verbatim, echoed back to the caller
	Lowered    []byte    // the needle lowercased (ASCII folding)
	Limit      int       // how many sessions to return at most
	PerSession int       // how many hits per session at most
	Since      time.Time // only search sessions updated after this; zero means no limit
	Until      time.Time // only search sessions updated before this; zero means no limit
	// Pattern limits the search to the one session it names, found the same way
	// get_session finds it (a full id, a fragment, a path fragment). Without it a search
	// spans every session; with it, "where in this session did we discuss X" is one call
	// instead of paging a 16 000-message session fifty at a time.
	Pattern string
	// Role keeps only hits from messages with that role — the human words rather than
	// the answer that repeats them. Applied while collecting, not after, so per_session
	// counts hits that match rather than hits that happen to come first.
	Role string
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
