package transcript

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
)

// marking adds msgs to a Writer that marks every turn it can, naming each by the message
// it began at, and gives the turns, the stats and the latest mark, as a mark is stored.
func marking(t *testing.T, from *Mark, msgs ...Message) ([]model.Turn, model.Stats, model.Session, *Mark, error) {
	t.Helper()
	c := &Collector{}
	w := New(c)
	start := 0
	if from != nil {
		if err := json.Unmarshal(from.Source, &start); err != nil {
			t.Fatal(err)
		}
		w.Resume(from)
	}
	for i := start; i < len(msgs); i++ {
		began := w.Turns()
		w.Add(msgs[i])
		if w.Turns() != began {
			w.MarkTurn(i)
		}
	}
	stats, err := w.Finish()
	var mark *Mark
	if m := w.Marked(); m != nil {
		b, _ := json.Marshal(m)
		mark = &Mark{}
		if err := json.Unmarshal(b, mark); err != nil {
			t.Fatal(err)
		}
	}
	return c.Turns, stats, w.S, mark, err
}

// A read taken up at the latest turn marked makes that turn and the ones after as a whole
// read does, and the session adds up to the same. A turn whose first message also answers
// a call of the turn before is not marked, and the mark stays at the turn before it.
func TestAMarkIsTakenUpAsAWholeRead(t *testing.T) {
	msgs := []Message{
		user(0, "<environment_context>cwd</environment_context>"),
		user(1, "run the tests"),
		{Role: Assistant, At: at(2), Response: "r1", Model: "m1", Usage: &model.Usage{Input: 10, Output: 2}, Blocks: []Block{CallOf("c1", "Bash", map[string]any{"command": "go test ./..."})}},
		{Role: User, At: at(3), Blocks: []Block{ResultOf("c1", "", "FAIL", Outcome{Status: model.StatusError})}},
		{Role: Assistant, At: at(4), Response: "r2", Model: "m1", Usage: &model.Usage{Input: 12, Output: 3}, Blocks: []Block{TextOf("One fails.")}},
		user(5, "fix it"),
		{Role: Assistant, At: at(6), Response: "r3", Model: "m2", Usage: &model.Usage{Input: 20, Output: 5}, Blocks: []Block{CallOf("c2", "Edit", map[string]any{"file_path": "/w/a.go", "old_string": "a", "new_string": "b"})}},
		// The next ask arrives in the row that answers the call: it is not marked
		{Role: User, At: at(8), Blocks: []Block{ResultOf("c2", "", "ok", Outcome{Status: model.StatusOK}), TextOf("and commit")}},
		{Role: Assistant, At: at(9), Response: "r4", Model: "m2", Usage: &model.Usage{Input: 30, Output: 4}, Blocks: []Block{TextOf("Committed.")}},
	}
	wholeTurns, wholeStats, wholeSession, mark, err := marking(t, nil, msgs...)
	if err != nil {
		t.Fatal(err)
	}
	if mark == nil || mark.Turn != 2 {
		t.Fatalf("the mark = %+v, want turn 2: turn 3 begins in a row that answers turn 2's call", mark)
	}
	if b := mark.Before(); b.Turns != 1 || b.Calls != 2 || b.Usage.Input != 22 {
		t.Errorf("before the mark = %+v, want turn 1's", b)
	}
	turns, stats, session, again, err := marking(t, mark, msgs...)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(Collected{Session: session, Stats: stats, Turns: turns})
	want, _ := json.Marshal(Collected{Session: wholeSession, Stats: wholeStats, Turns: wholeTurns[2:]})
	if string(got) != string(want) {
		t.Errorf("taken up at turn 2:\n%s\nwant:\n%s", got, want)
	}
	if again == nil || again.Turn != 2 {
		t.Errorf("the read taken up marks %+v, want turn 2 again", again)
	}
}

// Taken up at a mark, the first message must begin the turn marked; one that does not, or
// none at all, fails the read, which is then made whole.
func TestAMarkWithNoTurnThereIsNotTakenUp(t *testing.T) {
	msgs := []Message{user(0, "run the tests"), {Role: Assistant, At: at(1), Response: "r1", Blocks: []Block{TextOf("Done.")}}, user(2, "again")}
	_, _, _, mark, _ := marking(t, nil, msgs...)
	if mark == nil || mark.Turn != 2 {
		t.Fatalf("mark = %+v", mark)
	}
	for name, rest := range map[string][]Message{
		"an answer":              {{Role: Assistant, At: at(3), Response: "r2", Blocks: []Block{TextOf("Done again.")}}},
		"injected text":          {{Role: User, At: at(3), Injected: true, Blocks: []Block{TextOf("<system-reminder>x</system-reminder>")}}},
		"nothing":                nil,
		"an answer, then an ask": {{Role: Assistant, At: at(3), Response: "r2", Blocks: []Block{TextOf("Done again.")}}, user(4, "and again")},
	} {
		c := &Collector{}
		w := New(c)
		w.Resume(mark)
		for _, m := range rest {
			w.Add(m)
		}
		if _, err := w.Finish(); !errors.Is(err, ErrNotTakenUp) {
			t.Errorf("%s where turn 2 began: %v, %d turns", name, err, len(c.Turns))
		}
	}
}

// Whether a read taken up at a mark begins there is decided as a whole read decides that a
// turn begins: by the words a message holds, and by the files it carried when it holds
// none. A picture sent alone begins a turn, and is taken up at; CLI-written words do not,
// whatever came with them.
func TestAMarkBeginsWhereATurnDoes(t *testing.T) {
	png := Embedded("image/png", "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	msgs := []Message{
		user(0, "look at this"),
		{Role: Assistant, At: at(1), Response: "r1", Blocks: []Block{TextOf("Looking.")}},
		{Role: User, At: at(2), Blocks: []Block{AttachmentOf(png)}},
		{Role: Assistant, At: at(3), Response: "r2", Blocks: []Block{TextOf("A dot.")}},
	}
	wholeTurns, wholeStats, wholeSession, mark, err := marking(t, nil, msgs...)
	if err != nil {
		t.Fatal(err)
	}
	if mark == nil || mark.Turn != 2 {
		t.Fatalf("the mark = %+v, want turn 2, which a picture sent alone began", mark)
	}
	turns, stats, session, _, err := marking(t, mark, msgs...)
	if err != nil {
		t.Fatalf("taken up at a picture sent alone: %v", err)
	}
	got, _ := json.Marshal(Collected{Session: session, Stats: stats, Turns: turns})
	want, _ := json.Marshal(Collected{Session: wholeSession, Stats: wholeStats, Turns: wholeTurns[1:]})
	if string(got) != string(want) {
		t.Errorf("taken up at turn 2:\n%s\nwant:\n%s", got, want)
	}

	c := &Collector{}
	w := New(c)
	w.Resume(mark)
	w.Add(Message{Role: User, At: at(2), Blocks: []Block{TextOf("<ide_selection>x := 1</ide_selection>"), AttachmentOf(png)}})
	w.Add(user(3, "and this"))
	if _, err := w.Finish(); !errors.Is(err, ErrNotTakenUp) {
		t.Errorf("words the IDE wrapped, with a picture, where turn 2 began: %v, %d turns", err, len(c.Turns))
	}
}
