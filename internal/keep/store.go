package keep

import (
	"context"
	"os"
	"path/filepath"

	"github.com/itswl/agent-session-query/internal/sources"
)

// Store is a folder of one CLI's kept copies, read as a store: kept is the CLI's adapter
// opened on the folder, live its own store. A copy of a session the live store still holds
// is not listed — that store counts the session, and the copy would only be read twice —
// and a copy of one the CLI has deleted is: from then on, it is the session.
func Store(kept, live sources.Source) sources.Source { return &store{Source: kept, live: live} }

type store struct {
	sources.Source
	live sources.Source
}

// Scan lists the copies of the sessions the live store no longer holds.
func (s *store) Scan(ctx context.Context) ([]sources.Ref, error) {
	refs, err := s.Source.Scan(ctx)
	out := refs[:0]
	for _, ref := range refs {
		if !s.held(ref) {
			out = append(out, ref)
		}
	}
	return out, err
}

// held reports whether the live store still holds the session a copy is of: the file the
// copy's ref names, at the same path beneath it.
func (s *store) held(ref sources.Ref) bool {
	rel, err := filepath.Rel(s.Source.Location(), ref.Key)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	_, err = os.Lstat(filepath.Join(s.live.Location(), rel))
	return err == nil
}

// Held is what the folder holds: how many transcripts it keeps a copy of, and of those, how
// many the CLI no longer does.
func Held(ctx context.Context, src sources.Source) (kept, gone int, err error) {
	s, ok := src.(*store)
	if !ok {
		return 0, 0, nil
	}
	refs, err := s.Source.Scan(ctx)
	for _, ref := range refs {
		if !s.held(ref) {
			gone++
		}
	}
	return len(refs), gone, err
}

// IsKept reports whether a store is a folder of kept copies.
func IsKept(src sources.Source) bool {
	_, ok := src.(*store)
	return ok
}
