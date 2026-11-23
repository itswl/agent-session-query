// Package sources is the contract between one agent CLI's session store and the rest of
// the program.
//
// An adapter answers two questions and nothing else: which sessions are there (Scan),
// and what does this one say (Read). It does not list, search, count, cache or page —
// those are done once, over the canonical record, for every CLI alike. Adding a CLI is
// one package that implements Source and one Spec in the catalog.
package sources

import (
	"context"
	"time"

	"github.com/itswl/agent-session-query/internal/transcript"
)

// Ref names one session inside a store and says whether it has changed.
type Ref struct {
	// Key names the session within its source and stays the same from scan to scan: a
	// file path, a directory, a database row's id.
	Key string
	// Stamp changes whenever the session's content may have: sizes and modification
	// times for files, a row's own counters for a database. A session whose stamp has
	// not moved is not read again.
	Stamp string
	// ModTime is when the store last wrote the session, as far as a scan can tell. It
	// only orders the first read of a history — newest first — and is the session's time
	// of last resort when its transcript carries none.
	ModTime time.Time
}

// Source is one store of sessions.
type Source interface {
	// Location is where the store is, for a person to read.
	Location() string
	// Exists reports whether the store is there at all.
	Exists() bool
	// Scan lists the sessions the store holds now, without reading a transcript. An
	// error means the store could not be read — which is not the same as an empty one,
	// though both may list nothing.
	//
	// A store in several parts (a directory per project, a database per agent) may
	// return both: the sessions of the parts it could list, and an error for the part it
	// could not. Those sessions are read as usual; and because the list is known to be
	// short, nothing missing from it is taken for deleted.
	Scan(ctx context.Context) ([]Ref, error)
	// Read parses one session: it sets what the CLI recorded about the session on w.S and
	// adds every message, in order, to w. The caller finishes w.
	Read(ctx context.Context, ref Ref, w *transcript.Writer) error
}

// Keeper is a store whose sessions are files beneath its location, which asq can copy into
// a folder of its own and read there with the same adapter (see internal/keep). It is for
// the CLIs that delete their own sessions (Spec.Retention).
type Keeper interface {
	// Files lists the files a session is made of — its transcript and what is read beside
	// it — each beneath Location. A file that is not there is passed over.
	Files(ref Ref) []string
}

// Env is where an adapter looks for its CLI's store: the home directory and the
// environment, so a test can stand in for both.
type Env struct {
	Home   string
	Getenv func(string) string
}

// Spec describes one kind of agent CLI.
type Spec struct {
	Name  string // "claude": the name sessions carry and --source takes
	Title string // "Claude Code"
	// Locate is where the CLI keeps its sessions by default, following the CLI's own
	// rules for relocating them (CLAUDE_CONFIG_DIR, CODEX_HOME …).
	Locate func(Env) string
	// Open builds the adapter for a store at location.
	Open func(location string) Source
	// Movable says a single path can relocate the store (--path name=…). The stores that
	// span a whole data directory cannot be pointed at that way.
	Movable bool
	// Resume is the command that reopens a session by id in its own CLI, the id appended;
	// empty when the CLI has no such command.
	Resume string
	// ResumeHere says the command finds the session only from the directory it was worked
	// in, which a CLI that keeps sessions per project does: where that directory is not
	// known, there is no command to give.
	ResumeHere bool
	// Resumable says whether the CLI knows a session by the id its adapter gave it; nil
	// when it knows every one. An adapter that names a session after its file, for want
	// of the id the CLI keeps in it, gives an id the command cannot take.
	Resumable func(id string) bool
	// Store is the layout in one line, for the documentation the program prints.
	Store string
	// Retention is what the CLI deletes of its own history without being asked, in one
	// clause: what it deletes, after how long — saying "by default", as a setting changes
	// it — and the setting and the file that change it. Empty when the CLI keeps every
	// session until someone deletes it. Said only of the CLI's own store, not of a labelled
	// one: that is a copy, or another machine's, which this CLI does not sweep.
	// A session the CLI deletes is gone from its store, and the next sync takes it out of
	// the index too, so this is how far back the CLI's history in asq goes. Doctor prints
	// it followed by ", and they leave asq too": it names what is deleted in the plural.
	Retention string
	// Reads is the version of how the adapter reads its CLI's files. Raised whenever a
	// change would read a session already in an index differently — a record now read,
	// a figure now bounded — it has every session the index holds of the CLI, read with
	// an older one, read again at the next sync, so that an index does not keep what an
	// older reading made of them. The index keeps each session's reading beside its stamp.
	Reads int
}
