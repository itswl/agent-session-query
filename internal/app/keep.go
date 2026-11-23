package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/itswl/agent-session-query/internal/catalog"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/keep"
	"github.com/itswl/agent-session-query/internal/sources"
)

// KeptFolder is the folder of copies a process reads as stores of their own (see
// catalog.Config.Kept): the one the settings in a state directory name, once anything is
// kept or was.
func KeptFolder(state string) string {
	st, err := keep.Load(state)
	if err != nil {
		return ""
	}
	return KeptFolderOf(st, state)
}

// stateDir is where what is kept is written down: none, for a process told of none.
func (a *App) stateDir() (string, error) {
	if a.state == "" {
		return "", errors.New("this asq was given no state directory, where what is kept is written down")
	}
	return a.state, nil
}

// Copied is what one round of keeping copied.
type Copied struct {
	Transcripts int   `json:"transcripts"`
	Bytes       int64 `json:"bytes"`
}

// keepUp brings the copies of what is kept up to date, after a sync has brought the index
// up to date with the stores. What goes wrong is kept for KeepStatus to say: the sync
// itself went well.
func (a *App) keepUp(ctx context.Context) {
	if a.state == "" {
		return
	}
	st, err := keep.Load(a.state)
	if err == nil && !st.On() {
		return
	}
	if err == nil {
		_, err = a.copyKept(ctx, st, nil, false)
	}
	a.keepMu.Lock()
	a.keepErr = err
	a.keepMu.Unlock()
}

// copyKept copies what the settings keep and has changed since it was copied — of every
// store, or of one session alone — into the folder they name. A file still being written
// waits for a later round (keep.Settle), unless force says to copy it as it is.
func (a *App) copyKept(ctx context.Context, st keep.Settings, only *index.Session, force bool) (Copied, error) {
	a.copyMu.Lock()
	defer a.copyMu.Unlock()
	folder := st.Folder(a.state)
	a.Set.Keep(folder)
	var done Copied
	var errs error
	now := time.Now()
	for _, store := range a.Set.Present() {
		keeper, ok := store.Source.(sources.Keeper)
		if !ok || store.Name != store.Spec.Name {
			continue // a CLI's own store: one added by name is a copy already, or another machine's
		}
		all, ids := st.All && store.Spec.Retention != "", st.IDs(store.Spec.Name)
		if only != nil {
			if only.Source != store.Name {
				continue
			}
			all, ids = false, []string{only.ID}
		}
		rows, err := a.Index.Unkept(ctx, store.Name, all, ids)
		if err != nil {
			return done, err
		}
		to := filepath.Join(folder, store.Spec.Name)
		for _, row := range rows {
			if ctx.Err() != nil {
				return done, errors.Join(errs, ctx.Err())
			}
			whole, n, err := keep.Copy(keeper.Files(sources.Ref{Key: row.Key, Stamp: row.Stamp}), store.Source.Location(), to, now, force)
			done.Bytes += n
			if n > 0 {
				done.Transcripts++
			}
			errs = errors.Join(errs, err)
			if whole {
				errs = errors.Join(errs, a.Index.Kept(ctx, row.SID, row.Stamp))
			}
		}
	}
	return done, errs
}

// KeepAll keeps every session of the CLIs that delete their own, or stops: what is kept
// stays where it is. Kept, the sessions are copied now, but for those being written.
func (a *App) KeepAll(ctx context.Context, on bool) (Copied, error) {
	state, err := a.stateDir()
	if err != nil {
		return Copied{}, err
	}
	st, err := keep.Load(state)
	if err != nil {
		return Copied{}, err
	}
	st.All = on
	if err := keep.Save(state, st); err != nil {
		return Copied{}, err
	}
	if !on {
		return Copied{}, nil
	}
	return a.copyKept(ctx, st, nil, false)
}

// KeepIn names the folder the copies go in; "" is the one in asq's state directory. The
// copies made so far are not moved: a folder that holds some is not left for another
// until they have been moved there.
func (a *App) KeepIn(dir string) error {
	state, err := a.stateDir()
	if err != nil {
		return err
	}
	st, err := keep.Load(state)
	if err != nil {
		return err
	}
	if dir != "" {
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
	}
	was, next := st.Folder(state), keep.Settings{Dir: dir}.Folder(state)
	if filepath.Clean(was) != filepath.Clean(next) {
		if files, _ := keep.Size(was); files > 0 {
			return fmt.Errorf("the copies kept so far are in %s: move what is in it to %s first, and then name it again", was, next)
		}
	}
	st.Dir = dir
	if next == filepath.Join(state, "kept") {
		st.Dir = ""
	}
	return keep.Save(state, st)
}

// Keeping is what keeping one session, or no longer keeping it, did.
type Keeping struct {
	Copied
	// Stays says why a copy stays when its session is no longer kept one by one: "all",
	// every session of its CLI is kept; "gone", its CLI deleted the session, and the copy
	// is all there is of it. Empty: the copy was deleted.
	Stays string `json:"stays,omitempty"`
}

// KeepSession keeps one session, copied now whatever is being written, or stops keeping it
// one by one. A copy whose session the CLI's store still holds is then deleted, unless
// every session of the CLI is kept; one the CLI deleted is the session itself, and stays.
func (a *App) KeepSession(ctx context.Context, s *index.Session, on bool) (Keeping, error) {
	gone := keptCopy(s)
	if on || !gone {
		if why := Unkeepable(s); why != "" {
			return Keeping{}, errors.New(why)
		}
	}
	state, err := a.stateDir()
	if err != nil {
		return Keeping{}, err
	}
	st, err := keep.Load(state)
	if err != nil {
		return Keeping{}, err
	}
	st = st.Star(s.Agent, s.ID, on)
	if err := keep.Save(state, st); err != nil {
		return Keeping{}, err
	}
	spec, _ := catalog.Spec(s.Source)
	switch {
	case on:
		copied, err := a.copyKept(ctx, st, s, true)
		return Keeping{Copied: copied}, err
	case gone:
		return Keeping{Stays: "gone"}, nil
	case st.All && spec.Retention != "":
		return Keeping{Stays: "all"}, nil
	}
	return Keeping{}, a.dropCopy(ctx, st, s)
}

// dropCopy deletes the copy of a session the CLI's store still holds.
func (a *App) dropCopy(ctx context.Context, st keep.Settings, s *index.Session) error {
	a.copyMu.Lock()
	defer a.copyMu.Unlock()
	for _, store := range a.Set.Present() {
		keeper, ok := store.Source.(sources.Keeper)
		if !ok || store.Name != s.Source {
			continue
		}
		rows, err := a.Index.Transcripts(ctx, store.Name, []string{s.ID})
		if err != nil {
			return err
		}
		to := filepath.Join(st.Folder(a.state), store.Spec.Name)
		var errs error
		for _, row := range rows {
			for _, path := range keeper.Files(sources.Ref{Key: row.Key, Stamp: row.Stamp}) {
				rel, err := filepath.Rel(store.Source.Location(), path)
				if err != nil || !filepath.IsLocal(rel) {
					continue
				}
				if err := os.Remove(filepath.Join(to, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = errors.Join(errs, err)
				}
				// and the folders it leaves empty, up to the CLI's own
				for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
					if os.Remove(filepath.Join(to, dir)) != nil {
						break
					}
				}
			}
			errs = errors.Join(errs, a.Index.Kept(ctx, row.SID, ""))
		}
		return errs
	}
	return nil
}

// Unkeepable says why a session is not one asq keeps a copy of, or "" when it is.
func Unkeepable(s *index.Session) string {
	spec, ok := catalog.Spec(s.Source)
	switch {
	case !ok:
		return s.Source + " is not a CLI this asq reads"
	case s.Parent != nil:
		return "it is a subagent's transcript: keep the session it ran in, " + s.Root + ", and its subagents are kept with it"
	case s.Source != s.Agent:
		if keptCopy(s) {
			return "it is a copy asq keeps already: " + spec.Title + " no longer holds it"
		}
		return "it was read from " + s.Source + ", a store added by name: asq keeps copies of what a CLI's own store holds"
	}
	if _, ok := spec.Open("").(sources.Keeper); !ok {
		return spec.Title + " keeps its sessions until they are deleted, and asq keeps copies only of the CLIs that delete their own"
	}
	return ""
}

// KeptStore is one CLI's copies.
type KeptStore struct {
	Source string `json:"source"` // claude@kept
	Title  string `json:"title"`
	// Transcripts is how many the folder holds a copy of; Gone how many of those the CLI's
	// own store no longer holds, which are read from the copy
	Transcripts int `json:"transcripts"`
	Gone        int `json:"gone"`
}

// KeepStatus is what asq keeps, where, and what that takes.
type KeepStatus struct {
	All bool `json:"all"` // every session of the CLIs that delete theirs
	// Of is those CLIs
	Of       []string    `json:"of"`
	Sessions []string    `json:"sessions"` // kept one by one, as <cli>/<id>
	Folder   string      `json:"folder"`
	Stores   []KeptStore `json:"stores"`
	Files    int         `json:"files"`
	Bytes    int64       `json:"bytes"` // what the folder takes on disk
	// Error is what went wrong the last time this process copied, if anything did
	Error string `json:"error,omitempty"`
}

// KeepStatus reports what is kept and what the copies take.
func (a *App) KeepStatus(ctx context.Context) (KeepStatus, error) {
	state, err := a.stateDir()
	if err != nil {
		return KeepStatus{}, err
	}
	st, err := keep.Load(state)
	if err != nil {
		return KeepStatus{}, err
	}
	ks := KeepStatus{All: st.All, Sessions: append([]string{}, st.Sessions...), Folder: st.Folder(state), Stores: []KeptStore{}, Of: []string{}}
	for _, spec := range catalog.All {
		if spec.Retention != "" {
			if _, ok := spec.Open("").(sources.Keeper); ok {
				ks.Of = append(ks.Of, spec.Title)
			}
		}
	}
	ks.Files, ks.Bytes = keep.Size(ks.Folder)
	a.Set.Keep(KeptFolderOf(st, state))
	for _, store := range a.Set.All() {
		if !keep.IsKept(store.Source) || !store.Source.Exists() {
			continue
		}
		n, gone, err := keep.Held(ctx, store.Source)
		if err != nil {
			return ks, err
		}
		ks.Stores = append(ks.Stores, KeptStore{Source: store.Name, Title: store.Spec.Title, Transcripts: n, Gone: gone})
	}
	a.keepMu.Lock()
	if a.keepErr != nil {
		ks.Error = a.keepErr.Error()
	}
	a.keepMu.Unlock()
	return ks, nil
}

// KeptFolderOf is the folder to read copies from under these settings: theirs, once
// anything is kept or was; "" otherwise.
func KeptFolderOf(st keep.Settings, state string) string {
	folder := st.Folder(state)
	if !st.On() {
		if _, err := os.Stat(folder); err != nil {
			return ""
		}
	}
	return folder
}
