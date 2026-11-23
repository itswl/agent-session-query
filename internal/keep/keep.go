// Package keep keeps copies of sessions in a folder of asq's own, for the CLIs that delete
// their own: Claude Code and Gemini CLI delete the sessions not used for 30 days by
// default, and what a CLI deletes leaves the index too.
//
// A copy is a session's files as its CLI wrote them, each at the same path beneath the
// folder as beneath the store, so that the CLI's own adapter reads them: the folder is a
// store, "claude@kept". While the CLI's store holds a session, that store counts it and
// the copy is not read (see Store); once the CLI deleted it, the copy is the session.
//
// Nothing is kept unless a person asks: every session of those CLIs (Settings.All), or
// sessions one by one (Settings.Sessions). The copies go into asq's state directory, or a
// folder the person names, and never into a CLI's store.
package keep

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Label is what the kept copies of a CLI's store go by, after its name: claude@kept.
const Label = "kept"

// Settings is what a person asked to keep. It is kept in asq's state directory, so that
// every asq on the machine — a page server, an MCP server an agent started, a command in a
// terminal — keeps the same, and each looks again before it copies.
type Settings struct {
	// All keeps every session of each CLI that deletes its own (sources.Spec.Retention).
	All bool `json:"all,omitempty"`
	// Sessions are kept one by one, whatever All says: "claude/<id>".
	Sessions []string `json:"sessions,omitempty"`
	// Dir is where the copies go; empty is the folder kept in the state directory.
	Dir string `json:"dir,omitempty"`
}

// settingsFile is where Settings are, in the state directory.
const settingsFile = "keep.json"

// Load reads the settings kept in a state directory: none, when nothing was asked.
func Load(state string) (Settings, error) {
	var s Settings
	raw, err := os.ReadFile(filepath.Join(state, settingsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, errors.New(filepath.Join(state, settingsFile) + ": " + err.Error())
	}
	return s, nil
}

// Save writes the settings into a state directory, for its owner alone, whole or not at
// all: a reader never finds half of them.
func Save(state string, s Settings) error {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(state, settingsFile+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o600)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(state, settingsFile)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Folder is where the copies are: the folder named, or kept in the state directory.
func (s Settings) Folder(state string) string {
	if s.Dir != "" {
		return s.Dir
	}
	return filepath.Join(state, "kept")
}

// On reports whether anything is to be kept.
func (s Settings) On() bool { return s.All || len(s.Sessions) > 0 }

// Starred reports whether a session is kept one by one.
func (s Settings) Starred(agent, id string) bool {
	return slices.Contains(s.Sessions, agent+"/"+id)
}

// Star keeps a session one by one, or stops keeping it so.
func (s Settings) Star(agent, id string, keep bool) Settings {
	name := agent + "/" + id
	s.Sessions = slices.DeleteFunc(slices.Clone(s.Sessions), func(n string) bool { return n == name })
	if keep {
		s.Sessions = append(s.Sessions, name)
	}
	return s
}

// IDs are the ids of the sessions of one CLI kept one by one.
func (s Settings) IDs(agent string) []string {
	var ids []string
	for _, name := range s.Sessions {
		if a, id, ok := strings.Cut(name, "/"); ok && a == agent && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
