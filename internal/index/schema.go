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

// schemaVersion names the index file. A different layout, or an adapter that lists a
// CLI's sessions differently — other keys for them — is a different file: two versions of
// the program running at once each keep their own instead of rewriting one another's.
// Each removes from the index what it does not list, and the other reads it in again.
// The same sessions read differently are not a new file: see sources.Spec.Reads.
//
// Only the default path carries the version. An index at a path of its own (ASQ_INDEX,
// --index) is the same file from one version to the next, and is brought up to this one
// in place when it is opened (see addColumn).
//
// 2: the reads column, and Gemini's subagents and Antigravity's conversations listed apart.
const schemaVersion = 2

// readsTrigger forgets the reading of a session another asq wrote. An index at a path of
// its own can be written by an asq from before readings as well — an asq mcp an agent
// started before the upgrade, a container rolled back — which reads a session its own way,
// writes that over the row, and leaves reads as this one set it: the row would then name a
// reading it does not hold. This asq counts its every write of a row in reads_seq; a write
// of read_at that does not is another's, and the session's reading is none, so that it is
// read again, once. The trigger is in the file, and fires whichever asq writes.
const readsTrigger = `
CREATE TRIGGER IF NOT EXISTS sessions_read_by_another AFTER UPDATE OF read_at ON sessions
WHEN NEW.reads_seq = OLD.reads_seq
BEGIN
  UPDATE sessions SET reads = 0 WHERE sid = NEW.sid;
END`

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
  reads      INTEGER NOT NULL DEFAULT 0,   -- the adapter's reading it was read with (sources.Spec.Reads)
  reads_seq  INTEGER NOT NULL DEFAULT 0,   -- this asq's writes of the row: see readsTrigger
  kept       TEXT NOT NULL DEFAULT '',     -- the stamp its files were last copied at (internal/keep)
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
  live_state TEXT NOT NULL DEFAULT '',     -- where its latest turn stands (model.Live) …
  live       TEXT NOT NULL DEFAULT '',     --   and the rest of it, as JSON
  mark       TEXT NOT NULL DEFAULT '',     -- where a read can take its transcript up (storedMark)

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
-- rewrites only the turns that changed; src names what the rows were made from, so that
-- such a read need not make the rows of those that did not again.
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
  src        BLOB NOT NULL DEFAULT x'',
  PRIMARY KEY (sid, idx)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS turns_started ON turns (started_at);
-- What a search takes of each turn it finds: which it is and when, in the index itself. A
-- common word is found in most turns, and each read from its row is a page or more of text
-- for two numbers. Named by the queries that use it (see wordsFrom): the planner takes the
-- table's own key for a lookup by sid and idx, whatever the row holds. ended_at is here for
-- usage, which adds up the agents' time on every turn of each session when no window bounds
-- it (see usageTurns): a column another report reads of the turns it finds by session goes
-- here too.
CREATE INDEX IF NOT EXISTS turns_at ON turns (sid, idx, started_at, last_at, ended_at);
-- What a report adds up over the turns of a window, in the index itself. A turn's row
-- holds its text, so a month of turns read for four numbers each is most of the file.
CREATE INDEX IF NOT EXISTS turns_report ON turns (started_at, tools, failures, ended_at);

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

-- The tool calls that failed, and where the command each ran next succeeded (see later.go).
-- The columns after output an older asq lacks are added to its table (see failuresColumns).
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
  command_key   TEXT    NOT NULL DEFAULT '',  -- the program and its subcommand: go test (command.Key)
  later_idx     INTEGER NOT NULL DEFAULT 0,   -- where the same command next succeeded: its turn,
  later_seq     INTEGER NOT NULL DEFAULT 0,   --   its step — 0: it did not —
  later_at      INTEGER NOT NULL DEFAULT 0,   --   when, and the command as it ran then
  later_command TEXT    NOT NULL DEFAULT '',
  judged        INTEGER NOT NULL DEFAULT 0,   -- the laterVersion that worked these out; 0: none
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

` + wordsTable + `;

-- The turns whose row in the words table is to be written again from what the index
-- holds: see wordsStale.
CREATE TABLE IF NOT EXISTS words_stale (
  sid INTEGER NOT NULL,
  idx INTEGER NOT NULL,
  PRIMARY KEY (sid, idx)
) WITHOUT ROWID;
`

// wordsTable is the words of each turn, by who said them, and the title of its session,
// in the row of its first ask (see titleTurn). The rowid is the turn's: sid * 2^20 + idx.
// Contentless: the words themselves are in the turn, and are not stored twice. Each column
// holds a part's words and, after them, their stems (see stemsOf). The title is last: an
// older asq names the five columns it writes, and weighs five in the order it knows them.
// Its searches find a session's first turn by the title too, and weigh a word of it as
// they weigh one of a tool's output.
const wordsTable = `CREATE VIRTUAL TABLE IF NOT EXISTS words USING fts5 (
  ask, reply, thinking, tool, output, title,
  content = '', contentless_delete = 1,
  tokenize = "unicode61 remove_diacritics 2"
)`

// wordsVersion numbers what the words table holds, in the meta table under "words": a
// change to it raises the number. An index whose table an older asq made — a lower number,
// or none — has it made anew when it is opened, empty, and every turn named in words_stale:
// the next sync writes them all again from what the index holds, without reading a
// transcript (see Index.refill). One a newer asq made is left to it (see wordsAhead), so
// that two releases sharing an index do not make it anew, each its own way, by turns.
//
// 2: the session's title, and the stems of English words.
const wordsVersion = 2

// wordsStale names, in words_stale, the turns whose words another asq wrote: a turn it
// inserted, or the first ask of a session whose title it changed (see titleTurn). One that
// does not know of titles and stems in the words table writes the row without them; this
// one writes such a row again at its next sync (see Index.refill), and clears its own turns
// from the table as it writes them (see Index.write). The triggers are in the file, and fire
// whichever asq writes.
const wordsStale = `
CREATE TRIGGER IF NOT EXISTS words_stale_turn AFTER INSERT ON turns
BEGIN
  INSERT OR IGNORE INTO words_stale (sid, idx) VALUES (NEW.sid, NEW.idx);
END;
CREATE TRIGGER IF NOT EXISTS words_stale_title AFTER UPDATE OF title ON sessions
WHEN NEW.title IS NOT OLD.title
BEGIN
  INSERT OR IGNORE INTO words_stale (sid, idx) SELECT sid, idx FROM turns WHERE sid = NEW.sid AND idx = 1;
END`

// failuresColumns are the columns of the failures table an asq before laterVersion did not
// make, with their declarations (see addColumn).
var failuresColumns = [][2]string{
	{"command_key", "TEXT NOT NULL DEFAULT ''"},
	{"later_idx", "INTEGER NOT NULL DEFAULT 0"},
	{"later_seq", "INTEGER NOT NULL DEFAULT 0"},
	{"later_at", "INTEGER NOT NULL DEFAULT 0"},
	{"later_command", "TEXT NOT NULL DEFAULT ''"},
	{"judged", "INTEGER NOT NULL DEFAULT 0"},
}

// laterVersion numbers how a failure is judged to have passed later — the rule in later.go,
// and command.Key and command.Passed that it rests on: a change to any of them raises it. A
// failure the failures table says an older one judged — a lower number in judged, or none —
// is judged again at the next sync, from what the index holds, without reading a transcript
// (see Index.judge). One a newer asq judged is left to it.
//
// 1: the first.
const laterVersion = 1

// failuresJudged makes the failures whose judgement a turn written may change judged by
// none: those of the turns before it, as far back as laterTurns, where the same command may
// have passed in it, or no longer does. A failure another asq writes has none already, and
// one that does not judge failures writes turns without judging those before them: this one
// judges them all at its next sync (see Index.judge). It writes them again itself, from the
// reading, as it writes a session (see judged). The triggers are in the file, and fire
// whichever asq writes. A change of laterTurns would have them made anew. The indexes here,
// on what is judged and on each turn's failures, need the columns an older table gains.
const failuresJudged = `
CREATE INDEX IF NOT EXISTS failures_judged ON failures (judged);
CREATE INDEX IF NOT EXISTS failures_turn ON failures (sid, idx, later_seq);
CREATE TRIGGER IF NOT EXISTS failures_judged_turn AFTER INSERT ON turns
BEGIN
  UPDATE failures SET judged = 0 WHERE sid = NEW.sid AND idx >= NEW.idx - 3 AND idx <= NEW.idx AND judged != 0;
END;
CREATE TRIGGER IF NOT EXISTS failures_judged_gone AFTER DELETE ON turns
BEGIN
  UPDATE failures SET judged = 0 WHERE sid = OLD.sid AND idx >= OLD.idx - 3 AND idx < OLD.idx AND judged != 0;
END`

// turnRowID is the words table's rowid for a turn.
func turnRowID(sid int64, idx int) int64 { return sid<<20 | int64(idx) }

// maxTurnIndex is the largest turn index a rowid can hold.
const maxTurnIndex = 1<<20 - 1
