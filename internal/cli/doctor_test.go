package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// shelf is a store whose sessions are listed newest first: a conversation where talk[i]
// is true, an empty session otherwise. Every session holds one record of a kind no
// adapter knows, named after what it is.
type shelf struct {
	talk []bool
	read *int
}

func (shelf) Location() string { return "test" }
func (shelf) Exists() bool     { return true }
func (s shelf) Scan(context.Context) ([]sources.Ref, error) {
	now := time.Now()
	refs := make([]sources.Ref, len(s.talk))
	for i := range refs {
		refs[i] = sources.Ref{Key: fmt.Sprint(i), Stamp: "1", ModTime: now.Add(-time.Duration(i) * time.Minute)}
	}
	return refs, nil
}
func (s shelf) Read(_ context.Context, ref sources.Ref, w *transcript.Writer) error {
	*s.read++
	var i int
	fmt.Sscan(ref.Key, &i)
	w.S.ID = "s" + ref.Key
	if s.talk[i] {
		w.Unread("row:in-a-conversation")
		w.Add(transcript.Message{Role: transcript.User, At: time.Now(), Blocks: []transcript.Block{{Type: transcript.Text, Text: "hello"}}})
	} else {
		w.Unread("row:in-an-empty-one")
	}
	return nil
}

// What doctor reports as not understood is what conversations held, and it does not read
// a whole store of empty sessions looking for them.
func TestDoctorReadsConversations(t *testing.T) {
	talk := make([]bool, 3000)
	talk[0] = true // one conversation, then nothing but empty sessions
	read := 0
	lk, err := deepRead(context.Background(), index.Store{Name: "test", Source: shelf{talk: talk, read: &read}})
	if err != nil || lk.sessions != 3000 || lk.newest != "s0" {
		t.Fatalf("deepRead = %d, %q, %v", lk.sessions, lk.newest, err)
	}
	if lk.unread["row:in-an-empty-one"] != 0 || lk.unread["row:in-a-conversation"] != 1 {
		t.Errorf("unread = %v: what the conversation held, and nothing the empty sessions did", lk.unread)
	}
	if read > 1+doctorLook {
		t.Errorf("%d sessions read past the conversation found; doctorLook is %d", read-1, doctorLook)
	}

	// The newest sessions read as empty because nothing in them was understood — the
	// format drifted — and an older conversation, in the old format, reads whole: what the
	// empty ones held is what doctor is for
	talk = make([]bool, 20)
	talk[3] = true
	read = 0
	lk, err = deepRead(context.Background(), index.Store{Name: "test", Source: shelf{talk: talk, read: &read}})
	if err != nil || lk.newest != "s3" || lk.unread["row:in-an-empty-one"] != 3 {
		t.Errorf("drifted newest sessions: %q, %v, unread %v", lk.newest, err, lk.unread)
	}

	// Nothing but empty sessions: what they held is told, since it may be why
	read = 0
	lk, err = deepRead(context.Background(), index.Store{Name: "test", Source: shelf{talk: make([]bool, 8), read: &read}})
	if err == nil || lk.unread["row:in-an-empty-one"] == 0 {
		t.Errorf("a store of empty sessions: %v, unread %v", err, lk.unread)
	}
}

// A kind is printed, and is made from what a transcript names a record: it reaches the
// terminal as one line with nothing in it a terminal acts on.
func TestAnUnreadKindIsText(t *testing.T) {
	w := transcript.New(&transcript.Collector{})
	w.Unread("row:\x1b]52;c;cHduZWQ=\x07\x1b[2J\rfake\nline")
	w.Unread("")
	for kind := range w.Unreads() {
		if strings.ContainsAny(kind, "\x1b\x07\r\n") {
			t.Errorf("unread kind %q", kind)
		}
	}
	if w.Unreads()["(blank)"] != 1 {
		t.Errorf("an empty kind: %v", w.Unreads())
	}
}
