// Package index is the program's memory: the canonical record of every session, kept in
// one SQLite file and brought up to date from the CLIs' own stores before it is asked
// anything.
//
// It is a cache and nothing more. The CLIs' files are the truth; the index holds what
// was read from them and the fingerprint each was read at, and a session whose
// fingerprint has moved is read again. Deleting the file loses nothing — it is rebuilt
// from the stores on the next start.
//
// Why keep one at all, when the transcripts can be scanned: because every question
// worth asking is asked of turns, not of files. Which turn mentions this, what failed
// last week, what a project's sessions cost, what changed this file — each is a scan of
// every transcript when there is no index, and one query when there is. And a process
// that starts cold (an MCP server is started per agent session) inherits everything the
// last one learned.
package index

// schemaVersion names the index file. A different layout, or a different reading of the
// transcripts, is a different file: two versions of the program running at once each
// keep their own instead of rewriting one another's.
const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

-- One row per transcript a store holds.
CREATE TABLE IF NOT EXISTS sessions (
  sid        INTEGER PRIMARY KEY,
  source     TEXT NOT NULL,      -- the store: claude, claude@box2
  agent      TEXT NOT NULL,      -- the CLI: claude
  key        TEXT NOT NULL,      -- the transcript within the store
  stamp      TEXT NOT NULL,      -- the fingerprint it was read at
  id         TEXT NOT NULL,      -- the CLI's own session id
  path       TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  cwd        TEXT NOT NULL DEFAULT '',
  branch     TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL DEFAULT '',
  version    TEXT NOT NULL DEFAULT '',
  platform   TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL DEFAULT 0,   -- unix milliseconds; 0 is unknown
  updated_at INTEGER NOT NULL DEFAULT 0,
  archived   INTEGER NOT NULL DEFAULT 0,
  running    INTEGER NOT NULL DEFAULT 0,

  -- what a subagent's transcript says about where it came from …
  parent_session TEXT NOT NULL DEFAULT '',
  parent_agent   TEXT NOT NULL DEFAULT '',
  parent_call    TEXT NOT NULL DEFAULT '',
  parent_run     TEXT NOT NULL DEFAULT '',
  agent_type     TEXT NOT NULL DEFAULT '',
  fork           INTEGER NOT NULL DEFAULT 0,
  -- … and what linking resolved it to
  parent_sid INTEGER NOT NULL DEFAULT 0,   -- the transcript that spawned it
  root_sid   INTEGER NOT NULL DEFAULT 0,   -- the session it ran in, however deep
  depth      INTEGER NOT NULL DEFAULT 0,
  agents     INTEGER NOT NULL DEFAULT 0,   -- transcripts beneath this one
  copy_of    INTEGER NOT NULL DEFAULT 0,   -- a copy of another transcript, folded into it

  turns     INTEGER NOT NULL DEFAULT 0,
  items     INTEGER NOT NULL DEFAULT 0,
  tools     INTEGER NOT NULL DEFAULT 0,
  failures  INTEGER NOT NULL DEFAULT 0,
  calls     INTEGER NOT NULL DEFAULT 0,
  tok_in    INTEGER NOT NULL DEFAULT 0,
  tok_out   INTEGER NOT NULL DEFAULT 0,
  tok_cr    INTEGER NOT NULL DEFAULT 0,
  tok_cw    INTEGER NOT NULL DEFAULT 0,
  tok_think INTEGER NOT NULL DEFAULT 0,
  cost      REAL    NOT NULL DEFAULT 0,

  last_ask  TEXT NOT NULL DEFAULT '',
  answer    TEXT NOT NULL DEFAULT '',      -- the last thing the agent said
  first_id  TEXT NOT NULL DEFAULT '',      -- the first and last message ids, which is
  last_id   TEXT NOT NULL DEFAULT '',      --   how a copy of a transcript is recognised
  error     TEXT NOT NULL DEFAULT '',      -- why it could not be read whole
  read_ms   INTEGER NOT NULL DEFAULT 0,    -- what the last read cost
  read_at   INTEGER NOT NULL DEFAULT 0,
  UNIQUE (source, key)
);
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions (updated_at DESC);
CREATE INDEX IF NOT EXISTS sessions_id      ON sessions (id);
CREATE INDEX IF NOT EXISTS sessions_parent  ON sessions (source, parent_session);
CREATE INDEX IF NOT EXISTS sessions_root    ON sessions (root_sid);

-- One row per turn. head is the turn without its items, as JSON; body is the items and
-- the model calls, compressed. hash names the content, so a transcript read again
-- rewrites only the turns that changed.
CREATE TABLE IF NOT EXISTS turns (
  sid        INTEGER NOT NULL,
  idx        INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  ended_at   INTEGER NOT NULL DEFAULT 0,
  last_at    INTEGER NOT NULL DEFAULT 0,
  first_seq  INTEGER NOT NULL DEFAULT 0,
  items      INTEGER NOT NULL DEFAULT 0,
  tools      INTEGER NOT NULL DEFAULT 0,
  failures   INTEGER NOT NULL DEFAULT 0,
  interrupted INTEGER NOT NULL DEFAULT 0,
  hash       BLOB NOT NULL,
  head       TEXT NOT NULL,
  body       BLOB NOT NULL,
  PRIMARY KEY (sid, idx)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS turns_started ON turns (started_at);

-- The text a turn's items hold in full, where the turn keeps only a preview.
CREATE TABLE IF NOT EXISTS bodies (
  sid  INTEGER NOT NULL,
  seq  INTEGER NOT NULL,
  part TEXT    NOT NULL,          -- text | output | input
  idx  INTEGER NOT NULL,
  data BLOB    NOT NULL,
  PRIMARY KEY (sid, seq, part)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS bodies_turn ON bodies (sid, idx);

-- What each turn's model calls consumed, by model: the rows every usage figure sums.
-- Tokens only. A cost is a fact about the session whose CLI worked one out, and stays
-- with it: summed across sessions most of which have none, it would read as the cost of
-- usage it is not the cost of.
CREATE TABLE IF NOT EXISTS usage (
  sid       INTEGER NOT NULL,
  idx       INTEGER NOT NULL,
  model     TEXT    NOT NULL,
  at        INTEGER NOT NULL DEFAULT 0,
  calls     INTEGER NOT NULL DEFAULT 0,
  tok_in    INTEGER NOT NULL DEFAULT 0,
  tok_out   INTEGER NOT NULL DEFAULT 0,
  tok_cr    INTEGER NOT NULL DEFAULT 0,
  tok_cw    INTEGER NOT NULL DEFAULT 0,
  tok_think INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (sid, idx, model)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS usage_at ON usage (at);

-- The files each turn's tools named.
CREATE TABLE IF NOT EXISTS files (
  sid     INTEGER NOT NULL,
  idx     INTEGER NOT NULL,
  path    TEXT    NOT NULL,
  changed INTEGER NOT NULL DEFAULT 0,
  at      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (sid, idx, path)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS files_path ON files (path);

-- The tool calls that failed.
CREATE TABLE IF NOT EXISTS failures (
  sid     INTEGER NOT NULL,
  seq     INTEGER NOT NULL,
  idx     INTEGER NOT NULL,
  at      INTEGER NOT NULL DEFAULT 0,
  tool    TEXT    NOT NULL DEFAULT '',
  kind    TEXT    NOT NULL DEFAULT '',
  status  TEXT    NOT NULL DEFAULT '',
  exit_code INTEGER,
  command TEXT    NOT NULL DEFAULT '',
  output  TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (sid, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS failures_at ON failures (at DESC);

-- What a store said when it could not be read.
CREATE TABLE IF NOT EXISTS source_state (
  source   TEXT PRIMARY KEY,
  location TEXT NOT NULL DEFAULT '',
  error    TEXT NOT NULL DEFAULT '',
  scanned_at INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- The words of each turn, by who said them. The rowid is the turn's: sid * 2^20 + idx.
-- Contentless: the words themselves are in the turn, and are not stored twice.
CREATE VIRTUAL TABLE IF NOT EXISTS words USING fts5 (
  ask, reply, thinking, tool, output,
  content = '', contentless_delete = 1,
  tokenize = "unicode61 remove_diacritics 2"
);
`

// turnRowID is the words table's rowid for a turn.
func turnRowID(sid int64, idx int) int64 { return sid<<20 | int64(idx) }

// maxTurnIndex is the largest turn index a rowid can hold.
const maxTurnIndex = 1<<20 - 1
