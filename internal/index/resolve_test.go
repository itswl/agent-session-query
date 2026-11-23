package index

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources"
)

// A reference that fits more sessions than are listed says how many it fits.
func TestAnAmbiguousReferenceSaysHowManyItFits(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	if _, err := ix.Sync(ctx, []Store{{Name: "claude", Spec: sources.Spec{Name: "claude", Reads: 1}, Source: talk{n: 17, turns: 1}}}, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = ix.Resolve(ctx, "s", nil)
	var amb *AmbiguousError
	if !errors.As(err, &amb) || len(amb.Matches) != 12 || amb.Total != 17 ||
		!strings.HasPrefix(err.Error(), `"s" fits 17 sessions: claude/s`) || !strings.HasSuffix(err.Error(), ", and 5 more") {
		t.Errorf("a reference that fits 17 sessions: %v", err)
	}
}
