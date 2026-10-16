package source

// SessionUsage is one session's token accounting, for the usage report: the totals a
// session's own records add up to, under the shared names (inputTokens, outputTokens,
// cacheReadTokens, cacheWriteTokens, reasoningTokens), with the model that did the work.
// Cost is deliberately not carried: it would fall to whoever maintains a price table.
type SessionUsage struct {
	SessionID string
	Project   string
	Model     string
	UpdatedAt string
	Calls     int            // model calls, when the source can count them
	Usage     map[string]any // the shared token names
}

// UsageReporter answers every session's token accounting in one pass. The SQLite sources
// implement it with a single query; the file-shaped sources need not — their finals
// already sum usage per session, and the report reads those.
type UsageReporter interface {
	Usage() []SessionUsage
}
