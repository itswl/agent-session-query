package transcript

import (
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/model"
)

// A 1×1 PNG and GIF, as a transcript holds them: base64.
const (
	pngData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	gifData = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAICRAEAOw=="
)

// A file reads among the words as what it is and how big, or, when the transcript only
// names it, as what it is and its name. What a transcript says it is comes first; where
// it says nothing, or only "an image", the bytes say which.
func TestAnAttachmentIsMarkedAsWhatItIs(t *testing.T) {
	cases := []struct {
		name string
		a    Attachment
		want string
		mime string
		size int
	}{
		{"a picture held", Embedded("image/png", pngData), "[image · png · 70 bytes]", "image/png", 70},
		{"of a type not said", Embedded("", gifData), "[image · gif · 43 bytes]", "image/gif", 43},
		{"only said to be an image", Embedded("image/*", pngData), "[image · png · 70 bytes]", "image/png", 70},
		{"a data URL", Embedded("", "data:image/png;base64,"+pngData), "[image · png · 70 bytes]", "image/png", 70},
		{"its parameters", Embedded("Image/JPEG; q=1", strings.Repeat("A", 65536)), "[image · jpeg · 48 KB]", "image/jpeg", 49152},
		{"another name for it", Embedded("image/jpg", "AAAA"), "[image · jpeg · 3 bytes]", "image/jpeg", 3},
		{"a large one", Embedded("application/pdf", strings.Repeat("A", 1677722)), "[document · pdf · 1.2 MB]", "application/pdf", 1258291},
		{"its bytes left out", Embedded("image/png", ""), "[image · png]", "image/png", 0},
		{"named", Referenced("image/png", "/w/proj/plot.png"), "[image · png · plot.png]", "image/png", 0},
		{"named on Windows", Referenced("", `C:\w\shot.webp`), "[file · shot.webp]", "", 0},
		{"plain text", Embedded("text/plain", ""), "[document · text]", "text/plain", 0},
		{"a sound", Embedded("audio/x-wav", "AAAA"), "[audio · wav · 3 bytes]", "audio/x-wav", 3},
		{"not a media type", Embedded("a]b", "AAAA"), "[file · 3 bytes]", "", 3},
		{"not its bytes", Embedded("image/png", "…"), "[image · png]", "image/png", 0},
	}
	for _, c := range cases {
		if got := c.a.Marker(); got != c.want || c.a.Mime != c.mime || c.a.Size != c.size {
			t.Errorf("%s: %q (%q, %d bytes), want %q (%q, %d)", c.name, got, c.a.Mime, c.a.Size, c.want, c.mime, c.size)
		}
	}
	// A data URL that is not base64 holds text, which is no picture: not kept to be shown
	if a := Embedded("", "data:text/plain,hello"); a.Data != "" || a.Marker() != "[document · text · 5 bytes]" {
		t.Errorf("a text data URL = %+v, %q", a, a.Marker())
	}
	if Sniff([]byte("<svg xmlns=")) != "" || Sniff([]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")) != "image/webp" || Sniff([]byte("\xff\xd8\xff\xe0")) != "image/jpeg" {
		t.Error("only PNG, JPEG, GIF and WebP are pictures by their bytes")
	}
}

// A picture pasted into a prompt is marked after the words, whatever the order the CLI
// wrote it in: the ask says it was there, and a title is still the words. A picture sent
// alone is an ask. A result carries what came back beside its output. The record keeps
// what each file is, not its bytes — unless they were asked for.
func TestAFileAMessageCarriedIsMarkedAndKeptApart(t *testing.T) {
	shot := Embedded("image/png", pngData)
	c := &Collector{}
	w := New(c)
	w.Keep(0, 1, 5)
	for _, m := range []Message{
		{Role: User, At: at(0), Blocks: []Block{AttachmentOf(shot), TextOf("why is this button blue<runtime_info>T3</runtime_info>")}},
		{Role: Assistant, At: at(1), Model: "m1", Blocks: []Block{CallOf("c1", "screenshot", nil)}},
		{Role: User, At: at(2), Blocks: []Block{ResultOf("c1", "", "Took a screenshot", Outcome{Status: model.StatusOK}).With(Embedded("", gifData))}},
		{Role: User, At: at(3), Blocks: []Block{AttachmentOf(Referenced("image/png", "/w/plot.png"))}},
		{Role: Assistant, At: at(3), Model: "m1", Blocks: []Block{CallOf("c2", "screenshot", nil)}},
		{Role: User, At: at(3), Blocks: []Block{ResultOf("c2", "", "again", Outcome{}).With(shot), TextOf("<system-reminder>a note</system-reminder>")}},
		{Role: Assistant, At: at(4), Model: "m1", Blocks: []Block{TextOf("A plot.")}},
	} {
		w.Add(m)
	}
	stats, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	one, two := c.Turns[0], c.Turns[1]
	if one.Ask != "why is this button blue\n[image · png · 70 bytes]" || w.S.Title != "why is this button blue" {
		t.Errorf("ask %q, title %q: the words, without the harness block, then the picture", one.Ask, w.S.Title)
	}
	prompt, tool := one.Items[0], one.Items[1]
	want := []model.Attachment{{Mime: "image/png", Bytes: 70, Inline: true}}
	if !strings.HasSuffix(prompt.Text, "\n[image · png · 70 bytes]") || !equal(prompt.Attachments, want) {
		t.Errorf("prompt = %q, %+v", prompt.Text, prompt.Attachments)
	}
	if tool.Tool.Output != "Took a screenshot\n[image · gif · 43 bytes]" || !equal(tool.Attachments, []model.Attachment{{Mime: "image/gif", Bytes: 43, Inline: true}}) {
		t.Errorf("a screenshot tool's result = %q, %+v", tool.Tool.Output, tool.Attachments)
	}
	if stats.Turns != 2 || two.Ask != "[image · png · plot.png]" || !equal(two.Items[0].Attachments, []model.Attachment{{Mime: "image/png", Name: "/w/plot.png"}}) {
		t.Errorf("%d turns, a picture sent alone: ask %q, %+v", stats.Turns, two.Ask, two.Items[0].Attachments)
	}
	// A result's picture is the result's, and not the words beside it in the same message
	if again, note := two.Items[1], two.Items[2]; len(again.Attachments) != 1 || note.Kind != model.Context || len(note.Attachments) != 0 {
		t.Errorf("the result carries %+v, the words beside it %+v", again.Attachments, note.Attachments)
	}
	kept := w.Kept()
	if len(kept) != 2 || len(kept[0]) != 1 || kept[0][0].Data != pngData || len(kept[1]) != 1 || kept[1][0].Data != gifData {
		t.Errorf("kept = %+v: the bytes of what the items asked for, and nothing else", kept)
	}
	if New(nil).Kept() != nil {
		t.Error("nothing is kept unless asked for")
	}
}

func equal(a, b []model.Attachment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
