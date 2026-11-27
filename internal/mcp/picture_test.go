package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/index"
)

// withPictures is the fixture's history and, beside it, a session with pictures in it
// (fixture.WritePictures).
func withPictures(t *testing.T) *Server {
	t.Helper()
	h := fixture.Write(t)
	h.WritePictures(t)
	a, err := app.Open(h.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if _, err := a.Sync(t.Context(), index.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	return New(a, io.Discard)
}

// pictured is the session with pictures in it, and its steps by number: the ask a PNG was
// pasted into; the screenshot tool, whose result is a PNG and a GIF; the Read, whose result
// is an SVG and an SVG calling itself a PNG; the reply, which carried no file; and in the
// next turn an ask with a picture the transcript only names. It holds six steps in all.
type pictured struct {
	ref                          string
	ask, shot, svg, reply, named int
}

func picturesIn(t *testing.T, s *Server) pictured {
	t.Helper()
	sess, _, err := s.App.Resolve(t.Context(), "claude/"+fixture.Pictured)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := s.App.Index.Turns(t.Context(), &sess, index.TurnRange{First: 1, Last: -1})
	if err != nil || len(turns) != 2 || len(turns[0].Items) != 4 || len(turns[1].Items) != 2 || sess.Stats.Items != 6 {
		t.Fatalf("the pictured session's turns = %d, %v", len(turns), err)
	}
	p := pictured{ref: sess.Ref, ask: turns[0].Items[0].Seq, shot: turns[0].Items[1].Seq, svg: turns[0].Items[2].Seq,
		reply: turns[0].Items[3].Seq, named: turns[1].Items[0].Seq}
	if files := turns[0].Items[1].Attachments; len(files) != 2 || files[0].Mime != "image/png" || files[0].Bytes != 70 || files[1].Mime != "image/gif" || files[1].Bytes != 43 {
		t.Fatalf("the screenshot's files = %+v", files)
	}
	return p
}

// samePicture says whether an image block carries the bytes the fixture wrote, in base64.
func samePicture(b block, data string) bool {
	got, err1 := base64.StdEncoding.DecodeString(b.Data)
	want, err2 := base64.StdEncoding.DecodeString(data)
	return err1 == nil && err2 == nil && bytes.Equal(got, want)
}

// A picture a step carried is sent to the model that asks for it by number, as an image
// block beside the text that says which step's file it is: as the page shows it, a picture
// the transcript holds and by the type its bytes are, and not otherwise. Unasked for, it
// is its mark where it came, as before — a tool call's line says how many pictures its
// result carried, and a step read whole numbers its files, that a model can count to n.
func TestAPictureIsSentWhenAskedForByNumber(t *testing.T) {
	s := withPictures(t)
	p := picturesIn(t, s)

	turn, isErr := call(t, s, "read", map[string]any{"ref": p.ref, "turns": "1"})
	if isErr || !strings.Contains(turn, "✓ · 2 pictures") || strings.Contains(turn, "files:") {
		t.Errorf("the turn step by step does not count the pictures on its screenshot's line alone:\n%s", turn)
	}
	step, isErr := call(t, s, "read", map[string]any{"ref": p.ref, "seq": p.shot})
	if want := fmt.Sprintf("files: [0] image/png 70 bytes · [1] image/gif 43 bytes — picture=N with this seq sends file N"); isErr || !strings.Contains(step, want) {
		t.Errorf("the step read whole: isError=%v, no %q in\n%s", isErr, want, step)
	}

	for _, c := range []struct {
		name string
		turn int
		seq  int
		n    int
		mime string
		size int
		data string
	}{
		{"a PNG pasted into an ask", 1, p.ask, 0, "image/png", 70, fixture.PNG},
		{"a screenshot's first picture", 1, p.shot, 0, "image/png", 70, fixture.PNG},
		{"a screenshot's second picture", 1, p.shot, 1, "image/gif", 43, fixture.GIF},
	} {
		r := answered(t, s, "read", map[string]any{"ref": p.ref, "seq": c.seq, "picture": c.n})
		if r.Result.IsError || len(r.Result.Content) != 2 || r.Result.Content[0].Type != "text" || r.Result.Content[1].Type != "image" {
			t.Fatalf("%s: isError=%v, %d content blocks, want text then an image", c.name, r.Result.IsError, len(r.Result.Content))
		}
		text, image := r.Result.Content[0], r.Result.Content[1]
		if image.MimeType != c.mime || !samePicture(image, c.data) {
			t.Errorf("%s: the image block is %q and %d characters of base64, want the picture the transcript holds", c.name, image.MimeType, len(image.Data))
		}
		if want := fmt.Sprintf("%s#%d step %d, file %d: %s, %d bytes.", p.ref, c.turn, c.seq, c.n, c.mime, c.size); !strings.HasPrefix(text.Text, want) {
			t.Errorf("%s: the text = %q, want it to start %q", c.name, text.Text, want)
		}
		const note = "\n\n[The picture is sent as the transcript holds it: masking covers text, not what a picture shows.]"
		// The bytes are in the image block alone, and nothing else is in either
		if !strings.HasSuffix(text.Text, note) || strings.Contains(text.Text, image.Data) || text.Data != "" || image.Text != "" {
			t.Errorf("%s: the text = %q, the image's text = %q", c.name, text.Text, image.Text)
		}
	}

	// Over /mcp itself, the same two blocks
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"ref":%q,"seq":%d,"picture":1}}}`, p.ref, p.shot)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != http.StatusOK || r.Result.IsError || len(r.Result.Content) != 2 ||
		!samePicture(r.Result.Content[1], fixture.GIF) {
		t.Errorf("POST /mcp read with a picture = %d, %d content blocks", rec.Code, len(r.Result.Content))
	}
}

// Over stdio a picture goes as every reply does: one line of JSON, with the picture's
// bytes in it.
func TestAPictureGoesOverStdioOnOneLine(t *testing.T) {
	s := withPictures(t)
	p := picturesIn(t, s)
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{"ref":%q,"seq":%d,"picture":0}}}`, p.ref, p.ask)
	var out strings.Builder
	if err := s.Serve(t.Context(), strings.NewReader(line+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var r reply
	if lines := strings.Count(strings.TrimSpace(out.String()), "\n"); lines != 0 {
		t.Fatalf("the reply took %d lines", lines+1)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &r); err != nil || r.Result.IsError || len(r.Result.Content) != 2 ||
		!samePicture(r.Result.Content[1], fixture.PNG) || r.Result.Content[1].MimeType != "image/png" {
		t.Errorf("stdio read with a picture = %d content blocks, %v", len(r.Result.Content), err)
	}
}

// What is no picture says why, in words a model can act on: an SVG, an SVG calling itself
// a PNG, a picture the transcript only names, a file the step lacks, a step that carries
// none, a picture asked for without its step, and one asked for by anything but a number.
// None of them sends a second block.
func TestWhatIsNoPictureSaysWhy(t *testing.T) {
	s := withPictures(t)
	p := picturesIn(t, s)
	const shown = ", and the pictures shown are PNG, JPEG, GIF and WebP"
	for _, c := range []struct {
		name string
		seq  any
		n    any
		want string
	}{
		{"an SVG", p.svg, 0, "is image/svg+xml" + shown},
		{"an SVG calling itself a PNG", p.svg, 1, "is not in the transcript as it was when the transcript was read, or its bytes are not the picture it is said to be"},
		{"a picture the transcript only names", p.named, 0, "is image/*" + shown},
		{"a file the step lacks", p.ask, 1, fmt.Sprintf("step %d carried no file 1 (it carried 1, numbered from 0)", p.ask)},
		{"a step that carries none", p.reply, 0, fmt.Sprintf("step %d carried no file 0 (it carried 0, numbered from 0)", p.reply)},
		{"a file numbered below zero", p.shot, -1, "picture: want the number of a file of the step, from 0, got -1"},
		{"a picture asked for as an empty string", p.shot, "", `picture: want the number of a file of the step, from 0, got ""`},
		{"a step that is no step", 9999, 0, p.ref + " has no step 9999 (it has 6, numbered from 0)"},
		{"a picture asked for without its step", nil, 0, "picture goes with seq"},
		{"a picture asked for by a word", p.shot, "big", "picture: want a whole number, got big"},
	} {
		args := map[string]any{"ref": p.ref, "picture": c.n}
		if c.seq != nil {
			args["seq"] = c.seq
		}
		text, isErr := call(t, s, "read", args)
		if !isErr || !strings.Contains(text, c.want) || strings.HasPrefix(text, "no such session") {
			t.Errorf("%s: isError=%v %q, want an error saying %q", c.name, isErr, text, c.want)
		}
	}
}

// A picture larger than a model is sent is refused, saying how big it is, and is not
// shrunk: the person can see it on the page. One exactly at the limit is sent.
func TestAPictureTooLargeForAModelIsRefused(t *testing.T) {
	s := withPictures(t)
	p := picturesIn(t, s)
	old := maxSentPicture
	t.Cleanup(func() { maxSentPicture = old })

	maxSentPicture = 40
	text, isErr := call(t, s, "read", map[string]any{"ref": p.ref, "seq": p.shot, "picture": 1})
	if want := fmt.Sprintf("step %d's file 1 is 43 bytes: a picture is sent to a model only up to 40 bytes. The person can see it on the page", p.shot); !isErr || text != want {
		t.Errorf("a picture past the limit: isError=%v %q", isErr, text)
	}
	// What is no picture is still said so, whatever the limit
	if text, isErr = call(t, s, "read", map[string]any{"ref": p.ref, "seq": p.svg, "picture": 0}); !isErr || !strings.Contains(text, "is image/svg+xml") {
		t.Errorf("an SVG under a small limit: isError=%v %q", isErr, text)
	}

	maxSentPicture = 43
	r := answered(t, s, "read", map[string]any{"ref": p.ref, "seq": p.shot, "picture": 1})
	if r.Result.IsError || len(r.Result.Content) != 2 || !samePicture(r.Result.Content[1], fixture.GIF) ||
		!strings.HasPrefix(r.Result.Content[0].Text, fmt.Sprintf("%s#1 step %d, file 1: image/gif, 43 bytes.", p.ref, p.shot)) {
		t.Errorf("a picture at the limit: isError=%v, %d content blocks", r.Result.IsError, len(r.Result.Content))
	}
}

// A model is told what asking for a picture brings before it asks: that the picture goes
// as the transcript holds it, which masking does not cover, and how large a one may be.
func TestReadSaysWhatAPictureBrings(t *testing.T) {
	rd, _ := find("read")
	for _, want := range []string{"picture=n", "masking covers text, not what a picture shows", "up to 2 MB"} {
		if !strings.Contains(rd.about, want) {
			t.Errorf("read's description does not say %q: %q", want, rd.about)
		}
	}
	// and that a model may leave it out: nothing is sent but text until it asks
	for _, needed := range rd.need {
		if needed == "picture" {
			t.Errorf("read needs %v: a picture is asked for", rd.need)
		}
	}
}
