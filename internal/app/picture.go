package app

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// Picture is a picture an item carried, as a page may show it.
type Picture struct {
	Type string // image/png, image/jpeg, image/gif or image/webp: what its bytes are
	Data []byte
}

// The pictures read are bounded: one past MaxPicture is marked and not shown, and what is
// kept of the pictures read lately — so that the rest of a turn's do not each read its
// transcript again — is at most heldPictures.
const (
	MaxPicture   = 8 << 20
	readPictures = 30 * time.Second // the longest a transcript is read for them
)

var heldPictures = 32 << 20 // a variable for the tests (see export_test.go)

// Picture reads the nth file the item at seq carried, from the session's transcript: the
// index keeps what each file is, not its bytes. Only a picture the transcript holds is
// read — PNG, JPEG, GIF or WebP, as the transcript says and as its bytes are — never a
// file it names: that could be any file on this machine.
//
// The transcript is read to the end of the item's turn, once for all its pictures, which
// a page asks for together; and only while the item still carries in it what the index
// says it does. Anything else is index.ErrNotFound, saying why.
func (a *App) Picture(ctx context.Context, s *index.Session, seq, n int) (Picture, error) {
	it, turn, err := a.Index.Item(ctx, s, seq)
	if errors.Is(err, index.ErrNoItem) {
		return Picture{}, fmt.Errorf("%w: %s has no step %d", index.ErrNotFound, s.Ref, seq)
	}
	if err != nil {
		return Picture{}, err
	}
	if n < 0 || n >= len(it.Attachments) {
		return Picture{}, fmt.Errorf("%w: step %d of %s carried no file %d", index.ErrNotFound, seq, s.Ref, n)
	}
	att := it.Attachments[n]
	switch {
	case !Shown[att.Mime]:
		return Picture{}, fmt.Errorf("%w: step %d's file %d is %s, and the pictures shown are PNG, JPEG, GIF and WebP", index.ErrNotFound, seq, n, cmp.Or(att.Mime, "of no type the transcript says"))
	case !att.Inline:
		return Picture{}, fmt.Errorf("%w: step %d's file %d is named in the transcript, not held in it", index.ErrNotFound, seq, n)
	case att.Bytes > MaxPicture:
		return Picture{}, fmt.Errorf("%w: step %d's file %d is %d bytes, more than a picture shown (%d)", index.ErrNotFound, seq, n, att.Bytes, MaxPicture)
	}
	key := pictureKey{sid: s.SID(), seq: seq, n: n, att: att}
	if p, ok := a.pictures.get(key); ok {
		return p, nil
	}
	for {
		ran, err := a.pictures.once(ctx, pictureTurn(s, turn), func(ctx context.Context) error { return a.readPictures(ctx, s, turn, seq, n) })
		if err != nil {
			return Picture{}, err
		}
		if p, ok := a.pictures.get(key); ok {
			return p, nil
		}
		if ran {
			break
		}
		// The read this waited for was another request's, and kept the pictures it could
		// hold, which may not have been this one: this one is read for itself
	}
	return Picture{}, fmt.Errorf("%w: step %d's file %d is not in the transcript as it was when the transcript was read, or its bytes are not the picture it is said to be", index.ErrNotFound, seq, n)
}

// pictureTurn names a session's turn among the reads of the pictures under way.
func pictureTurn(s *index.Session, turn int) string {
	return strconv.FormatInt(s.SID(), 10) + "#" + strconv.Itoa(turn)
}

// Shown are the pictures Picture sends, and a page may show: by what the transcript says
// they are, and then by what their bytes are.
var Shown = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// readPictures reads the session's transcript to the end of a turn and keeps the pictures
// its items carry: the ones of the item asked for, then the others as far as what is kept
// holds beside them — so that none of them pushes out the picture asked for.
func (a *App) readPictures(ctx context.Context, s *index.Session, turn, first, n int) error {
	turns, err := a.Index.Turns(ctx, s, index.TurnRange{First: turn, Last: turn})
	if err != nil || len(turns) == 0 {
		return err
	}
	want := map[int][]model.Attachment{} // what the index says each item carries
	sizes := map[int]int{}               // what of it would be kept, by item
	for _, it := range turns[0].Items {
		if len(it.Attachments) == 0 {
			continue
		}
		want[it.Seq] = it.Attachments
		for _, att := range it.Attachments {
			if att.Inline && Shown[att.Mime] && att.Bytes <= MaxPicture {
				sizes[it.Seq] += att.Bytes
			}
		}
	}
	others, budget := []int{}, heldPictures-sizes[first]
	for _, it := range turns[0].Items {
		if size := sizes[it.Seq]; it.Seq != first && size > 0 && size <= budget {
			others, budget = append(others, it.Seq), budget-size
		}
	}

	var store *index.Store
	for _, st := range a.Set.Present() {
		if st.Name == s.Source {
			store = &st
			break
		}
	}
	if store == nil {
		return fmt.Errorf("%w: %s is in no store this asq reads now", index.ErrNotFound, s.Ref)
	}
	key, err := a.Index.Key(ctx, s)
	if err != nil {
		return err
	}
	// The read stops once the turn is over: its items are complete then, and what follows
	// is no part of them
	reading, stop := context.WithCancel(ctx)
	defer stop()
	done := errors.New("the turn is read")
	w := transcript.New(transcript.SinkFunc(func(t *model.Turn) error {
		if t.Index >= turn {
			stop()
			return done
		}
		return nil
	}))
	w.Keep(append(others, first)...)
	err = store.Source.Read(reading, sources.Ref{Key: key}, w)
	if _, ferr := w.Finish(); err == nil && !errors.Is(ferr, done) {
		err = ferr
	}
	if err != nil && reading.Err() == nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Kept last, the picture asked for is the last that what is kept lets go of
	kept := w.Kept()
	for _, seq := range append(others, first) {
		files, recorded := kept[seq], want[seq]
		if len(files) != len(recorded) {
			continue // the transcript has moved on from what the index read: nothing of it is the item's
		}
		order := make([]int, 0, len(files))
		for i := range files {
			if seq != first || i != n {
				order = append(order, i)
			}
		}
		if seq == first && n < len(files) {
			order = append(order, n)
		}
		for _, i := range order {
			f, was := files[i], recorded[i]
			if f.Mime != was.Mime || f.Size != was.Bytes || !was.Inline || f.Data == "" || f.Size > MaxPicture {
				continue
			}
			data, ok := decode(f.Data)
			if !ok {
				continue
			}
			if kind := transcript.Sniff(data); kind != "" {
				a.pictures.put(pictureKey{sid: s.SID(), seq: seq, n: i, att: was}, Picture{Type: kind, Data: data})
			}
		}
	}
	return nil
}

// decode reads base64 in either alphabet, padded or not.
func decode(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// pictureKey names a picture an item carried, and what the index said it was when it was
// read: a picture kept for a file the index now says something else of is not the file.
type pictureKey struct {
	sid    int64
	seq, n int
	att    model.Attachment
}

// pictures holds the pictures read lately, oldest first, up to heldPictures; and which
// turns are being read for theirs, so that a page asking for several at once has the
// transcript read once.
type pictures struct {
	mu   sync.Mutex
	held []heldPicture
	size int
	busy map[string]chan struct{}
}

type heldPicture struct {
	key pictureKey
	pic Picture
}

func (c *pictures) get(key pictureKey) (Picture, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range c.held {
		if h.key == key {
			return h.pic, true
		}
	}
	return Picture{}, false
}

func (c *pictures) put(key pictureKey, pic Picture) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range c.held {
		if h.key == key {
			return
		}
	}
	c.held = append(c.held, heldPicture{key, pic})
	c.size += len(pic.Data)
	for c.size > heldPictures && len(c.held) > 1 {
		c.size -= len(c.held[0].pic.Data)
		c.held = c.held[1:]
	}
}

// once runs read for a turn, or waits for the read of it already under way, and says
// which: ran is false when the read was another's. The read is not the caller's to cancel
// — another may be waiting for it — and is bounded instead.
func (c *pictures) once(ctx context.Context, turn string, read func(context.Context) error) (ran bool, err error) {
	c.mu.Lock()
	if wait, ok := c.busy[turn]; ok {
		c.mu.Unlock()
		select {
		case <-wait:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if c.busy == nil {
		c.busy = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	c.busy[turn] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.busy, turn)
		c.mu.Unlock()
		close(done)
	}()
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), readPictures)
	defer cancel()
	if err := read(bounded); err != nil {
		return true, fmt.Errorf("the transcript could not be read for its pictures: %w", err)
	}
	return true, nil
}
