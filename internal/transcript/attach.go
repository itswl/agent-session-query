package transcript

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Attachment is a file a message carried rather than said: a picture pasted into a prompt,
// a screenshot a tool returned, a PDF. A transcript holds it as bytes (base64) or names it
// (a path, a URI); there are no words in it to read.
//
// It is marked in the text where it was — "[image · png · 48 KB]" — so that the record
// does not read as if nothing was there: a prompt as if the person pasted nothing, a
// screenshot tool as if it returned nothing. Its bytes are not kept in the record; Data
// is what the adapter found, and the Writer lets go of it unless asked to keep it (Keep).
type Attachment struct {
	Mime string // what it is, as the transcript says or, failing that, as its bytes begin
	Size int    // its size in bytes; 0 when not known
	Name string // the file it names, when it names one instead of holding it
	Data string // its bytes, base64, when the transcript holds them
}

// Embedded is a file the transcript holds: data is base64, or a data: URL. mime is what
// the transcript says it is; empty, or only a kind ("image/*"), it is read from how the
// bytes begin where they say. Data that is empty is a file whose bytes the transcript
// left out: it is marked, as what it is, all the same.
func Embedded(mime, data string) Attachment {
	a := Attachment{Mime: cleanMime(mime)}
	if rest, ok := strings.CutPrefix(data, "data:"); ok {
		meta, payload, _ := strings.Cut(rest, ",")
		kind, params, _ := strings.Cut(meta, ";")
		if a.Mime == "" || vague(a.Mime) {
			a.Mime = firstOf(cleanMime(kind), a.Mime)
		}
		if !strings.Contains(";"+params+";", ";base64;") {
			// Text written into the URL as it is: what it holds is not a picture to show
			a.Size = len(payload)
			return a
		}
		data = payload
	}
	data = strings.TrimSpace(data)
	if data == "" {
		return a // named as a file of its kind, with its bytes left out of the transcript
	}
	a.Data = data
	a.Size = len(strings.TrimRight(data, "=")) * 6 / 8 // what the base64 decodes to
	if a.Mime == "" || vague(a.Mime) {
		a.Mime = firstOf(Sniff(head(data)), a.Mime)
	}
	return a
}

// vague reports whether a media type names only a kind of file: image/*.
func vague(mime string) bool { return strings.HasSuffix(mime, "/*") }

// firstOf is the first of two media types that says something.
func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Referenced is a file the transcript names and does not hold. Its bytes are not read:
// what a transcript names could be any file on this machine.
func Referenced(mime, name string) Attachment {
	return Attachment{Mime: cleanMime(mime), Name: name}
}

// head decodes the first bytes of base64 text, enough to tell what the file is.
func head(data string) []byte {
	if len(data) > 64 {
		data = data[:64]
	}
	data = data[:len(data)/4*4]
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		if b, err := enc.DecodeString(data); err == nil {
			return b
		}
	}
	return nil
}

// Sniff is the type of an image by how its bytes begin: PNG, JPEG, GIF or WebP — the
// pictures a page may show — and "" for anything else.
func Sniff(b []byte) string {
	switch {
	case strings.HasPrefix(string(b), "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case strings.HasPrefix(string(b), "\xff\xd8\xff"):
		return "image/jpeg"
	case strings.HasPrefix(string(b), "GIF87a"), strings.HasPrefix(string(b), "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// cleanMime is a media type without its parameters, in lower case, or "" for what is not
// one.
func cleanMime(s string) string {
	s, _, _ = strings.Cut(s, ";")
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.Contains(s, "/") || len(s) > 100 || strings.ContainsAny(s, " \t\r\n[]") {
		return ""
	}
	return s
}

// Marker is how the attachment reads among the words: "[image · png · 48 KB]",
// "[document · pdf · 1.2 MB]", "[image · png · plot.png]" for one only named.
func (a Attachment) Marker() string {
	kind, sub, _ := strings.Cut(a.Mime, "/")
	switch {
	case kind == "image", kind == "audio", kind == "video":
	case a.Mime == "application/pdf", kind == "text":
		kind = "document"
	default:
		kind = "file"
	}
	sub = strings.TrimPrefix(strings.TrimPrefix(sub, "x-"), "vnd.")
	sub, _, _ = strings.Cut(sub, "+")
	switch sub {
	case "plain":
		sub = "text"
	case "*":
		sub = ""
	}
	parts := []string{kind}
	if sub != "" {
		parts = append(parts, sub)
	}
	switch {
	case a.Size > 0:
		parts = append(parts, sizeText(a.Size))
	case a.Name != "":
		name := a.Name
		if i := strings.LastIndexAny(name, `/\`); i >= 0 && i+1 < len(name) {
			name = name[i+1:]
		}
		if name = txt.OneLine(name, 60); name != "" {
			parts = append(parts, name)
		}
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

// sizeText is a size the way a marker gives it: 512 bytes, 48 KB, 1.2 MB.
func sizeText(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.Itoa((n+1<<9)>>10) + " KB"
	}
	return strconv.Itoa(n) + " bytes"
}

// AttachmentOf builds a block that marks a file a message carried. Among a message's
// words it reads as its marker, after the words, in the order the files came.
func AttachmentOf(a Attachment) Block {
	return Block{Type: Text, Text: a.Marker(), Attachments: []Attachment{a}}
}

// With is a result block that carried files beside its output: each is marked on a line
// after the output, and the item the result settles on carries them.
func (b Block) With(atts ...Attachment) Block {
	if len(atts) == 0 {
		return b
	}
	b.Text = Marked(b.Text, atts)
	b.Attachments = append(b.Attachments[:len(b.Attachments):len(b.Attachments)], atts...)
	return b
}

// Marked is text followed by the markers of the files that came with it, one a line.
func Marked(text string, atts []Attachment) string {
	var sb strings.Builder
	sb.WriteString(text)
	for _, a := range atts {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(a.Marker())
	}
	return sb.String()
}

// recorded is what the record keeps of attachments: what each is, without its bytes.
func recorded(atts []Attachment) []model.Attachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]model.Attachment, len(atts))
	for i, a := range atts {
		out[i] = model.Attachment{Mime: a.Mime, Bytes: a.Size, Name: a.Name, Inline: a.Data != ""}
	}
	return out
}
