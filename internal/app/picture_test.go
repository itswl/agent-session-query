package app_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// A picture an item carried is read from the transcript when it is asked for — the index
// keeps only what it is — and only a picture the transcript holds, by what its bytes are:
// not a file it names, not an SVG that calls itself an image, not a file it no longer holds
// as it did when it was read. A turn's pictures are read together, so that the rest of
// them are not each a read of the transcript.
func TestAPictureIsReadFromTheTranscript(t *testing.T) {
	a, h := fixture.Open(t)
	path := h.WritePictures(t)
	ctx := t.Context()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	s, _, err := a.Resolve(ctx, "claude/"+fixture.Pictured)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := a.Index.Turns(ctx, &s, index.TurnRange{First: 1, Last: -1})
	if err != nil {
		t.Fatal(err)
	}
	ask, shot, svg, named := turns[0].Items[0], turns[0].Items[1], turns[0].Items[2], turns[1].Items[0]
	if len(ask.Attachments) != 1 || len(shot.Attachments) != 2 || len(svg.Attachments) != 2 || len(named.Attachments) != 1 {
		t.Fatalf("attachments: ask %+v, screenshot %+v, svg %+v, named %+v", ask.Attachments, shot.Attachments, svg.Attachments, named.Attachments)
	}
	png, _ := base64.StdEncoding.DecodeString(fixture.PNG)
	gif, _ := base64.StdEncoding.DecodeString(fixture.GIF)

	pic, err := a.Picture(ctx, &s, ask.Seq, 0)
	if err != nil || pic.Type != "image/png" || !bytes.Equal(pic.Data, png) {
		t.Fatalf("the pasted picture = %q, %d bytes, %v", pic.Type, len(pic.Data), err)
	}
	// The screenshot's first picture reads the turn: its second is read with it, and is
	// there with the transcript gone
	if pic, err := a.Picture(ctx, &s, shot.Seq, 0); err != nil || pic.Type != "image/png" {
		t.Fatalf("the screenshot = %q, %v", pic.Type, err)
	}
	moved := path + ".away"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if pic, err := a.Picture(ctx, &s, shot.Seq, 1); err != nil || pic.Type != "image/gif" || !bytes.Equal(pic.Data, gif) {
		t.Errorf("the screenshot's second picture = %q, %v: it was read with the turn", pic.Type, err)
	}
	if err := os.Rename(moved, path); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct{ seq, n int }{
		"an SVG":                   {svg.Seq, 0},
		"an SVG called a PNG":      {svg.Seq, 1},
		"a picture only named":     {named.Seq, 0},
		"a file the step lacks":    {ask.Seq, 1},
		"a step that is no step":   {9999, 0},
		"a step that carried none": {turns[0].Items[3].Seq, 0},
	} {
		if _, err := a.Picture(ctx, &s, c.seq, c.n); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}

	// A transcript rewritten under the index, with another picture where the first one was,
	// does not have the other picture passed off as the one the index read
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(fixture.PNG), []byte(fixture.GIF), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := fresh.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	s2, _, _ := fresh.Resolve(ctx, "claude/"+fixture.Pictured)
	if err := os.WriteFile(path, b, 0o644); err != nil { // the PNG back, unknown to the index
		t.Fatal(err)
	}
	if _, err := fresh.Picture(ctx, &s2, ask.Seq, 0); !errors.Is(err, index.ErrNotFound) || !strings.Contains(err.Error(), "not in the transcript as it was") {
		t.Errorf("a picture the transcript no longer holds as it was read: %v", err)
	}
}

// Pictures a page asks for together are each sent. A turn's read keeps the pictures of the
// step asked for — the one asked for last, so that what is kept lets it go last — and of
// the others only what fits beside them; a request that waited for another's read, which
// did not keep its picture, reads the turn for itself rather than say the picture is gone.
func TestPicturesAskedForTogetherAreEachSent(t *testing.T) {
	a, h := fixture.Open(t)
	h.WritePictures(t)
	ctx := t.Context()
	if _, err := a.Sync(ctx, index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	s, _, err := a.Resolve(ctx, "claude/"+fixture.Pictured)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := a.Index.Turns(ctx, &s, index.TurnRange{First: 1, Last: 1})
	if err != nil {
		t.Fatal(err)
	}
	shot := turns[0].Items[1] // a PNG of 70 bytes and a GIF of 43
	defer app.SetHeldPictures(100)()

	// The screenshot's PNG with its GIF is more than is kept: the PNG asked for stays
	if pic, err := a.Picture(ctx, &s, shot.Seq, 0); err != nil || pic.Type != "image/png" {
		t.Errorf("the picture asked for, beside more than is kept = %q, %v", pic.Type, err)
	}

	// Another request reads the turn, and keeps nothing this one asks for
	release := app.BusyPictures(a, &s, 1)
	got := make(chan error, 1)
	go func() {
		pic, err := a.Picture(ctx, &s, shot.Seq, 1)
		if err == nil && pic.Type != "image/gif" {
			err = errors.New("sent as " + pic.Type)
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond) // for the request to wait on the other read
	release()
	if err := <-got; err != nil {
		t.Errorf("a picture the read waited for did not keep: %v", err)
	}
}
