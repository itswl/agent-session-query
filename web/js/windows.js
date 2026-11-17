// The windows of time every view offers, and how each is said: one list, so that one
// view's "7 days" is every other's, in the same words and the same order. They roll —
// the last 24 hours, not today — as a calendar window reads differently depending on
// when it is asked. The key is what the API takes as since.

export const WINDOWS = [['24h', '24 hours'], ['7d', '7 days'], ['30d', '30 days'], ['all', 'All time']];

// windowOf is the window a query names, or fallback when it names none of these.
export const windowOf = (query, fallback) => (WINDOWS.some(([k]) => k === query.get('since')) ? query.get('since') : fallback);

// label is the window as a choice shows it: "7 days", "All time".
export const label = (key) => WINDOWS.find(([k]) => k === key)?.[1] || key;

// span is the window beside a heading: "last 7 days", "all time".
export const span = (key) => (key === 'all' ? 'all time' : `last ${label(key)}`);

// within ends a sentence about what happened in the window: "in the last 7 days", and for
// all time nothing, as "12 files changed" says it already.
export const within = (key) => (key === 'all' ? '' : ` in the last ${label(key)}`);

// longer is what an empty window suggests: a longer one, while there is one.
export const longer = (key) => (key === 'all' ? '' : 'Try a longer window.');
