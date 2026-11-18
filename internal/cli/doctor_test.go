package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/render"
	"github.com/itswl/agent-session-query/internal/sources"
	"github.com/itswl/agent-session-query/internal/sources/claude"
	"github.com/itswl/agent-session-query/internal/transcript"
)

// shelf is a store whose sessions are listed newest first: a conversation where talk[i]
// is true, an empty session otherwise. Every session holds one record of a kind no
// adapter knows, named after what it is. The newest was written at at (now, when it is
// zero), and each of the others every before the one listed above it (a minute, when
// that is zero).
type shelf struct {
	talk  []bool
	read  *int
	at    time.Time
	every time.Duration
}

func (shelf) Location() string { return "test" }
func (shelf) Exists() bool     { return true }
func (s shelf) Scan(context.Context) ([]sources.Ref, error) {
	at, every := s.at, s.every
	if at.IsZero() {
		at = time.Now()
	}
	if every == 0 {
		every = time.Minute
	}
	refs := make([]sources.Ref, len(s.talk))
	for i := range refs {
		refs[i] = sources.Ref{Key: fmt.Sprint(i), Stamp: "1", ModTime: at.Add(-time.Duration(i) * every)}
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

// Doctor says how far back each store goes and, of a CLI that deletes its own old sessions,
// that it does and which setting keeps them longer: what the CLI deletes leaves the index
// with it. In words and in JSON alike, and neither of a store that holds no transcripts.
func TestDoctorSaysHowFarBackAStoreGoes(t *testing.T) {
	const retention = "My CLI deletes the sessions not used for 30 days (keep in ~/.mycli/settings.json)"
	const day = 24 * time.Hour
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	three := []bool{true, true, true}
	for _, tc := range []struct {
		about     string
		store     shelf
		name      string // the store's; "" for the CLI's own, "mycli"
		retention string // the CLI's, as its Spec has it
		oldest    string // the day doctor gives the oldest transcript; "" for none
		said      bool   // whether doctor says what the CLI deletes
	}{
		{"a CLI that deletes its old sessions", shelf{talk: three, at: at, every: day}, "", retention, "2026-10-17", true},
		{"a CLI that keeps them", shelf{talk: three, at: at, every: day}, "", "", "2026-10-17", false},
		{"an empty store", shelf{at: at}, "", retention, "", false},
		// a database row with no time in it is listed at 1970, which is no session's day
		{"a store that cannot tell when", shelf{talk: three, at: time.UnixMilli(0)}, "", retention, "", true},
		// a copy, or another machine's: this CLI does not sweep it, and its settings are not here
		{"a labelled store", shelf{talk: three, at: at, every: day}, "mycli@backup", retention, "2026-10-17", false},
	} {
		tc.store.read = new(int)
		name := tc.name
		if name == "" {
			name = "mycli"
		}
		stores := []index.Store{{Name: name, Spec: sources.Spec{Name: "mycli", Retention: tc.retention}, Source: tc.store}}

		var out bytes.Buffer
		if err := doctor(context.Background(), stores, false, render.Style{}, &out); err != nil {
			t.Errorf("%s: %v\n%s", tc.about, err, &out)
		}
		text := out.String()
		if tc.oldest != "" && !strings.Contains(text, "3 transcripts, oldest "+tc.oldest+"   newest read whole") {
			t.Errorf("%s: the oldest transcript is not said to be of %s:\n%s", tc.about, tc.oldest, text)
		}
		if tc.oldest == "" && strings.Contains(text, "oldest") {
			t.Errorf("%s: an oldest transcript is said to be of a day:\n%s", tc.about, text)
		}
		if strings.Contains(text, "leave asq") != tc.said || tc.said && !strings.Contains(text, "\n  · "+retention+", and they leave asq too\n") {
			t.Errorf("%s: what the CLI deletes said %v; want %v:\n%s", tc.about, !tc.said, tc.said, text)
		}

		out.Reset()
		if err := doctor(context.Background(), stores, true, render.Style{}, &out); err != nil {
			t.Errorf("%s --json: %v\n%s", tc.about, err, &out)
		}
		var report struct {
			Stores []struct {
				Oldest    time.Time `json:"oldest"`
				Retention string    `json:"retention"`
			} `json:"stores"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Stores) != 1 {
			t.Fatalf("%s --json: %v\n%s", tc.about, err, &out)
		}
		got := report.Stores[0]
		if want := at.Add(-2 * day); tc.oldest != "" && !got.Oldest.Equal(want) || tc.oldest == "" && !got.Oldest.IsZero() {
			t.Errorf("%s --json: oldest %v; want %q", tc.about, got.Oldest, tc.oldest)
		}
		if tc.oldest == "" && strings.Contains(out.String(), `"oldest"`) {
			t.Errorf("%s --json: an oldest when there is none:\n%s", tc.about, &out)
		}
		if want := map[bool]string{true: retention}[tc.said]; got.Retention != want {
			t.Errorf("%s --json: retention %q; want %q", tc.about, got.Retention, want)
		}
	}

	// Claude Code's store goes back to the day its oldest session last wrote, and says what
	// Claude Code deletes
	h, indexPath := setup(t)
	oldest := h.Now.Add(-3*day + 40*time.Second) // beta's last record
	out, errs, code := asq(t, h, indexPath, "doctor")
	if code != 0 || !strings.Contains(out, "3 transcripts, oldest "+oldest.Local().Format(time.DateOnly)) || !strings.Contains(out, "  · "+claude.Spec.Retention+", and they leave asq too\n") {
		t.Errorf("doctor: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code = asq(t, h, indexPath, "doctor", "--json")
	var report struct {
		Stores []struct {
			Oldest    time.Time `json:"oldest"`
			Retention string    `json:"retention"`
		} `json:"stores"`
	}
	if err := json.Unmarshal([]byte(out), &report); code != 0 || err != nil || len(report.Stores) != 1 {
		t.Fatalf("doctor --json: exit %d, %v\n%s%s", code, err, out, errs)
	}
	if got := report.Stores[0]; !got.Oldest.Equal(oldest) || got.Retention != claude.Spec.Retention {
		t.Errorf("doctor --json: oldest %v, retention %q; want %v, %q", got.Oldest, got.Retention, oldest, claude.Spec.Retention)
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
