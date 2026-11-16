// Package jsonmap reads the layout Hermes and OpenClaw shared before each moved into
// SQLite, and the rows that outlived it.
//
//	<dir>/sessions.json         the index: key → what the CLI records about a session
//	<dir>/<session id>.jsonl    the session's transcript, one JSON object per line
//
// The index is a map from a routing key ("agent:main:…") to a record that names the
// session id. The transcript's rows come in two shapes, read by the same Reader: pi's
// typed rows, which OpenClaw writes and still keeps — unchanged, one per row — in the
// database that replaced its files, and Hermes's flat messages.
package jsonmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/kit"
)

// Index is one sessions.json and the directory its transcripts are in.
type Index struct {
	Path string // sessions.json
	Dir  string // where <session id>.jsonl is

	mu      sync.Mutex
	stamp   string
	records []Entry
	loaded  bool
}

// At is the index of a sessions directory.
func At(dir string) *Index {
	return &Index{Path: filepath.Join(dir, "sessions.json"), Dir: dir}
}

// Entry is one record of the index.
type Entry struct {
	Key  string         // the index's own key for the session
	ID   string         // the session it names
	Info map[string]any // what the index records about it
	// File is the session's transcript, empty when there is none: a session that has not
	// written its first row yet, or one whose rows are kept somewhere else.
	File string
	// Stamp moves whenever the record does.
	Stamp string
}

// Exists reports whether the index is there.
func (x *Index) Exists() bool { return kit.Exists(x.Path) }

// Entries is every record of the index, in the order the file holds them, each with its
// transcript when it has one. A missing index is empty; one that is there and cannot be
// read — truncated mid-write by the process that owns it, corrupt, not ours to open — is
// an error, because a store that failed is not a store with nothing in it.
//
// The parsed file is kept until it changes. Which transcripts exist is asked again every
// time, in one listing of the directory: a store that keeps no transcript beside its
// index (Hermes with everything in its database: 518 records here and not one file)
// would otherwise cost a stat per record on every scan.
func (x *Index) Entries() ([]Entry, error) {
	records, err := x.load()
	if err != nil || len(records) == 0 {
		return nil, err
	}
	// A directory that cannot be listed is an error, like an index that cannot be read:
	// every record would otherwise look as if it had lost its transcript
	names := map[string]bool{}
	dir, err := os.ReadDir(x.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range dir {
		if !e.IsDir() {
			names[e.Name()] = true
		}
	}
	out := make([]Entry, len(records))
	for i, e := range records {
		e.File = x.fileOf(e, func(name string) bool { return names[name] })
		out[i] = e
	}
	return out, nil
}

// Entry finds the record of a session by its id.
func (x *Index) Entry(id string) (Entry, bool) {
	records, _ := x.load()
	for _, e := range records {
		if e.ID == id {
			e.File = x.fileOf(e, x.isFile)
			return e, true
		}
	}
	return Entry{}, false
}

// ByFile finds the record whose transcript is file.
func (x *Index) ByFile(file string) (Entry, bool) {
	records, _ := x.load()
	name := filepath.Base(file)
	for _, e := range records {
		for _, candidate := range candidates(e) {
			if candidate != name {
				continue
			}
			if found := x.fileOf(e, x.isFile); found == file {
				e.File = found
				return e, true
			}
		}
	}
	return Entry{}, false
}

func (x *Index) isFile(name string) bool {
	info, err := os.Stat(filepath.Join(x.Dir, name))
	return err == nil && info.Mode().IsRegular()
}

// fileOf is the transcript an entry names, or "" when it is not there. The record may
// name its file (OpenClaw's sessionFile: a transcript is not always called after its
// session); failing that it is <session id>.jsonl.
//
// Only the name of a recorded file is used, and it is looked for beside the index. The
// recorded path is whatever the index says: for a directory copied from another machine
// it names a path there, and a doctored index could name any file on this one.
func (x *Index) fileOf(e Entry, has func(name string) bool) string {
	for _, name := range candidates(e) {
		if has(name) {
			return filepath.Join(x.Dir, name)
		}
	}
	return ""
}

// candidates are the file names an entry's transcript may have, the likeliest first.
func candidates(e Entry) []string {
	recorded := strings.ReplaceAll(kit.Str(e.Info["sessionFile"]), "\\", "/")
	out := make([]string, 0, 2)
	for _, name := range []string{path.Base(recorded), e.ID + ".jsonl"} {
		if name == "." || name == ".." || name == "/" || strings.ContainsAny(name, `/\`) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func (x *Index) load() ([]Entry, error) {
	stamp, _ := kit.Stamp(x.Path)
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.loaded && x.stamp == stamp {
		return x.records, nil
	}
	records, err := readIndex(x.Path)
	if err != nil {
		// Only a read that worked is kept — and what the last one that worked said is
		// still handed back with the error: a file caught mid-rewrite is a reason to say
		// so, not to forget what it held a moment ago
		return x.records, err
	}
	x.stamp, x.records, x.loaded = stamp, records, true
	return records, nil
}

// readIndex decodes the index in the file's own order. A Go map would lose it, and the
// order decides which of two keys naming one session is the session's record: the first.
func readIndex(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s is not valid JSON (truncated or corrupt)", path)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%s is not a JSON object", path)
	}
	var out []Entry
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			break
		}
		// A value that is not an object is a note among the records: Hermes keeps a
		// _README string in the file, saying what the file is
		var info map[string]any
		if json.Unmarshal(value, &info) != nil || info == nil {
			continue
		}
		id := kit.First(kit.Str(info["session_id"]), kit.Str(info["sessionId"]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Entry{Key: key, ID: id, Info: info, Stamp: StampOf(string(value))})
	}
	return out, nil
}

// Updated is when the index says the session was last written: Hermes's updated_at, an
// ISO string, or OpenClaw's updatedAt, epoch milliseconds.
func (e Entry) Updated() time.Time {
	return kit.Time(firstSet(e.Info["updated_at"], e.Info["updatedAt"]))
}

// Created is when the index says the session began.
func (e Entry) Created() time.Time {
	return kit.Time(firstSet(e.Info["created_at"], e.Info["createdAt"]))
}

func firstSet(values ...any) any {
	for _, v := range values {
		if kit.Truthy(v) {
			return v
		}
	}
	return nil
}

// Ref names an entry's session for a scan. One with a transcript is its file: it changes
// when the file does, or when what the index says of it does. One without is known to the
// index alone, and is named after it.
func (x *Index) Ref(e Entry) sources.Ref {
	if e.File != "" {
		stamp, mod := kit.Stamp(e.File)
		return sources.Ref{Key: e.File, Stamp: stamp + ";" + e.Stamp, ModTime: mod}
	}
	mod := e.Updated()
	if mod.IsZero() {
		_, mod = kit.Stamp(x.Path)
	}
	return sources.Ref{Key: x.Path + "#" + e.ID, Stamp: e.Stamp, ModTime: mod}
}

// Named reports whether key is one Ref gave a session with no transcript, and which
// session.
func (x *Index) Named(key string) (id string, ok bool) {
	return strings.CutPrefix(key, x.Path+"#")
}

// StampOf folds what a record says about itself into a short stamp: a title or a whole
// index record is not something to carry around in one.
func StampOf(parts ...string) string {
	h := fnv.New64a()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 36)
}
