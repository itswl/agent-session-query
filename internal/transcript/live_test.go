package transcript

import (
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
)

// liveOf is where the latest turn stands after the messages, for a CLI that tells it so.
func liveOf(t *testing.T, tells Tells, msgs ...Message) *model.Live {
	t.Helper()
	w := New(&Collector{})
	w.Tells = tells
	for _, m := range msgs {
		w.Add(m)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	return w.S.Live
}

func reply(sec int, response, text string) Message {
	return Message{Role: Assistant, At: at(sec), Response: response, Model: "m", Blocks: []Block{TextOf(text)}}
}

func call(sec int, response, id, command string) Message {
	return Message{Role: Assistant, At: at(sec), Response: response, Model: "m", Blocks: []Block{CallOf(id, "Bash", map[string]any{"command": command})}}
}

func result(sec int, id string, status string) Message {
	return Message{Role: User, At: at(sec), Blocks: []Block{ResultOf(id, "", "output", Outcome{Status: status})}}
}

func ends(sec int) Message { return Message{Role: System, At: at(sec), Ends: true} }

// What the end of a transcript says of the latest turn: at work, waiting on a call, or
// done and waiting for the person.
func TestWhereTheLatestTurnStands(t *testing.T) {
	ask := user(0, "run the tests")
	for _, c := range []struct {
		name  string
		tells Tells
		msgs  []Message
		want  model.Live
	}{
		{"asked, nothing back yet", TellsByRows, []Message{ask},
			model.Live{State: model.LiveWorking, Since: at(0)}},
		{"thinking", TellsByRows, []Message{ask, {Role: Assistant, At: at(2), Response: "r1", Blocks: []Block{ThinkingOf("which tests")}}},
			model.Live{State: model.LiveWorking, Since: at(0)}},
		{"a call with no result", TellsByRows, []Message{ask, call(3, "r1", "c1", "go test ./...")},
			model.Live{State: model.LiveWaiting, Since: at(3), Tool: "Bash", Command: "go test ./...", Seq: 1}},
		{"its result on its way back", TellsByRows, []Message{ask, call(3, "r1", "c1", "go test ./..."), result(9, "c1", model.StatusOK)},
			model.Live{State: model.LiveWorking, Since: at(0)}},
		{"answered", TellsByRows, []Message{ask, call(3, "r1", "c1", "go test ./..."), result(9, "c1", model.StatusOK), reply(12, "r2", "All pass.")},
			model.Live{State: model.LiveIdle, Since: at(12)}},
		{"words, then a call of the same response", TellsByRows, []Message{ask, reply(2, "r1", "Running them."), call(3, "r1", "c1", "go test ./...")},
			model.Live{State: model.LiveWaiting, Since: at(3), Tool: "Bash", Command: "go test ./...", Seq: 2}},
		{"words the response says calls follow", TellsByRows, []Message{ask, {Role: Assistant, At: at(2), Response: "r1", Stop: "tool_use", Blocks: []Block{TextOf("Running them.")}}},
			model.Live{State: model.LiveWorking, Since: at(0)}},
		{"words of a response still being written", TellsByRows, []Message{ask, {Role: Assistant, At: at(2), Unfinished: true, Blocks: []Block{TextOf("Running")}}},
			model.Live{State: model.LiveWorking, Since: at(0)}},
		{"answered, by marks: not over until marked", TellsByMarks, []Message{ask, reply(12, "r1", "All pass.")},
			model.Live{State: model.LiveWorking, Since: at(0)}},
		{"marked over", TellsByMarks, []Message{ask, reply(12, "r1", "All pass."), ends(13)},
			model.Live{State: model.LiveIdle, Since: at(13)}},
		{"a call the person declined ends it", TellsByMarks, []Message{ask, call(3, "r1", "c1", "rm -rf build"), result(20, "c1", model.StatusInterrupted)},
			model.Live{State: model.LiveIdle, Since: at(20)}},
		{"stopped", TellsByRows, []Message{ask, call(3, "r1", "c1", "sleep 100"),
			{Role: User, At: at(30), Blocks: []Block{EventOf(model.EventInterrupted, "[Request interrupted by user]")}}},
			model.Live{State: model.LiveIdle, Since: at(30)}},
		{"the CLI gave up in the model's place", TellsByRows, []Message{ask, {Role: Assistant, At: at(40), Model: SyntheticModel, Blocks: []Block{TextOf("API Error: overloaded")}}},
			model.Live{State: model.LiveIdle, Since: at(40)}},
		{"taken up again after it was over", TellsByRows, []Message{ask, reply(12, "r1", "All pass."), ends(13),
			{Role: User, At: at(50), Blocks: []Block{TextOf("<task-notification>done</task-notification>")}, Injected: true},
			call(52, "r2", "c2", "git log")},
			model.Live{State: model.LiveWaiting, Since: at(52), Tool: "Bash", Command: "git log", Seq: 3}},
		{"a call of an earlier response never answered", TellsByRows, []Message{ask, call(3, "r1", "c1", "make"), reply(9, "r2", "Done.")},
			model.Live{State: model.LiveIdle, Since: at(9)}},
		{"words after a call never answered, in one message", TellsByRows, []Message{ask, {Role: Assistant, At: at(5),
			Blocks: []Block{CallOf("c1", "Bash", map[string]any{"command": "sleep 9"}), TextOf("Here is how.")}}},
			model.Live{State: model.LiveIdle, Since: at(5)}},
		{"a command the person ran beside it", TellsByRows, []Message{ask, reply(12, "r1", "All pass."),
			{Role: System, At: at(60), Blocks: []Block{CallOf("s1", "shell", map[string]any{"command": "git status"})}}},
			model.Live{State: model.LiveIdle, Since: at(12)}},
		{"an error after the answer", TellsByRows, []Message{ask, reply(12, "r1", "All pass."),
			{Role: System, At: at(90), Blocks: []Block{{Type: EventBlock, Name: "error", Text: "quota", After: true}}}},
			model.Live{State: model.LiveIdle, Since: at(12)}},
		{"a file the call names", TellsByRows, []Message{ask, {Role: Assistant, At: at(5), Response: "r1",
			Blocks: []Block{CallOf("e1", "Edit", map[string]any{"file_path": "/work/a.go", "old_string": "a", "new_string": "b"})}}},
			model.Live{State: model.LiveWaiting, Since: at(5), Tool: "Edit", File: "/work/a.go", Seq: 1}},
	} {
		got := liveOf(t, c.tells, c.msgs...)
		if got == nil || *got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A CLI that does not tell is not said to be anywhere; nor is a session with no ask yet.
// The mark alone is no row of the turn: it moves neither the turn's end nor its last row.
func TestWhatTellsNothing(t *testing.T) {
	if got := liveOf(t, TellsNothing, user(0, "hi"), reply(1, "r1", "hello")); got != nil {
		t.Errorf("a CLI that tells nothing: %+v", got)
	}
	if got := liveOf(t, TellsByRows, Message{Role: User, At: at(0), Injected: true, Blocks: []Block{TextOf("<environment_context/>")}}); got != nil {
		t.Errorf("before any ask: %+v", got)
	}
	marked := collect(t, user(0, "hi"), reply(1, "r1", "hello"), ends(3600))
	plain := collect(t, user(0, "hi"), reply(1, "r1", "hello"))
	if len(marked.Turns) != 1 || !marked.Turns[0].LastAt.Equal(plain.Turns[0].LastAt) || !marked.Turns[0].EndedAt.Equal(at(1)) ||
		len(marked.Turns[0].Items) != len(plain.Turns[0].Items) || !marked.Session.UpdatedAt.Equal(plain.Session.UpdatedAt) {
		t.Errorf("a mark changed the turn: %+v against %+v", marked.Turns[0], plain.Turns[0])
	}
}
