package kit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func lines(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	if err := Lines(path, func(line []byte) bool { out = append(out, string(line)); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	huge := strings.Repeat("x", 3<<20) // one row larger than any buffer: an image, pasted
	os.WriteFile(path, []byte("one\n\ntwo\r\n"+huge+"\nlast with no newline"), 0o644)
	got := lines(t, path)
	if len(got) != 4 || got[0] != "one" || got[1] != "two" || len(got[2]) != len(huge) || got[3] != "last with no newline" {
		t.Errorf("lines = %d: %q … %q", len(got), got[0], got[len(got)-1])
	}

	// Stopping early stops
	n := 0
	Lines(path, func([]byte) bool { n++; return false })
	if n != 1 {
		t.Errorf("read %d lines after being told to stop", n)
	}

	// A compressed transcript reads the same
	var packed []byte
	enc, _ := zstd.NewWriter(nil)
	packed = enc.EncodeAll([]byte("one\ntwo\nthree\n"), nil)
	enc.Close()
	zst := filepath.Join(dir, "t.jsonl.zst")
	os.WriteFile(zst, packed, 0o644)
	if got := lines(t, zst); strings.Join(got, ",") != "one,two,three" {
		t.Errorf("zstd lines = %q", got)
	}
	// … and one cut short mid-write gives what it holds rather than nothing
	os.WriteFile(zst, packed[:len(packed)-3], 0o644)
	var some []string
	_ = Lines(zst, func(line []byte) bool { some = append(some, string(line)); return true })
	if len(some) == 0 {
		t.Log("a truncated zstd frame yielded no lines (acceptable: nothing whole was decodable)")
	}

	if err := Lines(filepath.Join(dir, "missing.jsonl"), func([]byte) bool { return true }); err == nil {
		t.Error("a file that is not there reads as empty")
	}

	var objs int
	os.WriteFile(path, []byte(`{"a":1}`+"\n"+`not json`+"\n"+`[1,2]`+"\n"+`{"b":2}`+"\n"+`{"cut":`), 0o644)
	Objects(path, func(map[string]any) bool { objs++; return true })
	if objs != 2 {
		t.Errorf("objects = %d: a row that is not an object is skipped, not fatal", objs)
	}
}

func TestTimes(t *testing.T) {
	want := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for _, in := range []any{
		"2026-10-01T10:00:00Z", "2026-10-01T10:00:00.000Z", "2026-10-01T12:00:00+02:00",
		"2026-10-01 10:00:00.000000000+00:00", // as Antigravity's index writes it
		float64(want.Unix()), float64(want.UnixMilli()), want.UnixMilli(), int(want.Unix()), "1790848800",
	} {
		if got := Time(in); !got.Equal(want) {
			t.Errorf("Time(%v) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []any{nil, "", "soon", float64(0), map[string]any{}} {
		if got := Time(in); !got.IsZero() {
			t.Errorf("Time(%v) = %v, want no time", in, got)
		}
	}
	// Seconds, milliseconds, microseconds and nanoseconds are told apart by magnitude
	for _, v := range []float64{float64(want.Unix()), float64(want.UnixMilli()), float64(want.UnixMicro()), float64(want.UnixNano())} {
		if got := Epoch(v); got.Sub(want).Abs() > time.Second {
			t.Errorf("Epoch(%v) = %v", v, got)
		}
	}
}

func TestValues(t *testing.T) {
	if Str(12) != "" || Str("x") != "x" || Str(nil) != "" {
		t.Error("Str is a string or nothing")
	}
	if n, ok := Num(int64(12)); !ok || n != 12 {
		t.Errorf("Num(12) = %v, %v", n, ok)
	}
	if _, ok := Num("12"); ok {
		t.Error("a string is not a number: a count that arrives as text is a format to look at, not to guess through")
	}
	if Int(float64(7)) != 7 || Int(nil) != 0 || Int("x") != 0 {
		t.Error("Int")
	}
	if First("", "", "c", "d") != "c" || First() != "" {
		t.Error("First")
	}
	if !Truthy("x") || !Truthy(float64(1)) || !Truthy(true) || Truthy("") || Truthy(nil) || Truthy(float64(0)) || Truthy(false) {
		t.Error("Truthy")
	}
	content := []any{
		map[string]any{"type": "text", "text": "one"},
		map[string]any{"type": "image", "source": "…"},
		map[string]any{"type": "text", "text": "two"},
		"three",
	}
	if got := ContentText(content); !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Errorf("ContentText = %q", got)
	}
	if ContentText("plain") != "plain" {
		t.Error("ContentText of a string is the string")
	}
}

func TestStampMovesWithTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if stamp, _ := Stamp(path); stamp != "" && Exists(path) {
		t.Error("a missing file exists")
	}
	os.WriteFile(path, []byte("a"), 0o644)
	first, mod := Stamp(path)
	if first == "" || mod.IsZero() || !Exists(path) {
		t.Fatalf("stamp = %q %v", first, mod)
	}
	if again, _ := Stamp(path); again != first {
		t.Error("an untouched file has a new stamp")
	}
	os.WriteFile(path, []byte("ab"), 0o644)
	if grown, _ := Stamp(path); grown == first {
		t.Error("a file that grew keeps its stamp")
	}
}

// A filesystem whose clock moves in coarse ticks (tmpfs) gives a write in place at the
// same size the modification time of the write before: the fingerprint holds through a
// change. While the store was written too recently for that to be ruled out, the Snapshot
// reads again each time; once it has been still for longer, what it remembers stands.
func TestSnapshotTrustsAFingerprintOnceTheStoreIsStill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	os.WriteFile(path, []byte("one"), 0o644)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	at := info.ModTime()
	// rewrite changes the file at its size and puts its time back: nothing about it says so
	rewrite := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if now, _ := os.Stat(path); now.Size() != info.Size() || !now.ModTime().Equal(at) {
			t.Fatalf("the rewrite left %d bytes at %v", now.Size(), now.ModTime())
		}
	}
	var s Snapshot[string]
	reads := 0
	load := func() string {
		t.Helper()
		got, err := s.Load(path, func() (string, error) {
			reads++
			b, err := os.ReadFile(path)
			return string(b), err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	clock := func(now time.Time) {
		Now = func() time.Time { return now }
	}
	t.Cleanup(func() { Now = time.Now })

	// Written a moment ago: every load reads, and sees the change the fingerprint hides
	clock(at.Add(time.Second))
	if got := load(); got != "one" {
		t.Fatalf("first load = %q", got)
	}
	rewrite("two")
	if got := load(); got != "two" || reads != 2 {
		t.Errorf("a store written a second ago, changed at its size and time: %q after %d reads, want what it holds now", got, reads)
	}

	// The same, with the file's time ahead of the clock: a clock behind the file server's
	clock(at.Add(-time.Minute))
	rewrite("six")
	if got := load(); got != "six" || reads != 3 {
		t.Errorf("a store dated a minute ahead, changed at its size and time: %q after %d reads", got, reads)
	}

	// Still for an hour: the read under this fingerprint is remembered, and a change that
	// leaves the fingerprint as it is cannot have happened since — it would have moved it
	clock(at.Add(time.Hour))
	if got := load(); got != "six" || reads != 4 {
		t.Errorf("first load once still = %q after %d reads: what was read before it is only the last answer", got, reads)
	}
	rewrite("ten")
	if got := load(); got != "six" || reads != 4 {
		t.Errorf("an unmoved fingerprint of a still store: %q after %d reads, want the remembered answer and no read", got, reads)
	}
	// … and a fingerprint that moves is read again
	os.WriteFile(path, []byte("eleven"), 0o644)
	if got := load(); got != "eleven" || reads != 5 {
		t.Errorf("a store that grew: %q after %d reads", got, reads)
	}
}

func TestDSNEscapesWhatAURIWouldRead(t *testing.T) {
	for path, want := range map[string]string{
		"/home/me/.hermes/state.db":  "file:/home/me/.hermes/state.db?",
		"/backups/notes #2/state.db": "file:/backups/notes %232/state.db?",
		"/odd/100%/what?/state.db":   "file:/odd/100%25/what%3f/state.db?",
	} {
		if got := DSN(path); !strings.HasPrefix(got, want) {
			t.Errorf("DSN(%q) = %q, want it to start %q", path, got, want)
		}
	}
}

func TestGlobSaysWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a/one.jsonl", "a/two.txt", "b/three.jsonl", "b/deep/sub/agent-1.jsonl", "c/four.jsonl", "top.jsonl"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("x"), 0o644)
	}
	rel := func(paths []string) string {
		out := make([]string, len(paths))
		for i, p := range paths {
			r, _ := filepath.Rel(root, p)
			out[i] = filepath.ToSlash(r)
		}
		return strings.Join(out, " ")
	}
	for pattern, want := range map[string]string{
		"*/*.jsonl":                "a/one.jsonl b/three.jsonl c/four.jsonl",
		"*.jsonl":                  "top.jsonl",
		"*/deep/sub/agent-*.jsonl": "b/deep/sub/agent-1.jsonl",
		"missing/*/*.jsonl":        "",
		"a/one.jsonl":              "a/one.jsonl",
		"a/nothing.jsonl":          "",
		"?/t*.*":                   "a/two.txt b/three.jsonl",
	} {
		got, err := Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil || rel(got) != want {
			t.Errorf("Glob(%s) = %q, %v; want %q", pattern, rel(got), err, want)
		}
		// … and exactly what the standard one finds, when there is nothing unreadable
		std, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if rel(std) != rel(got) {
			t.Errorf("Glob(%s) = %q, filepath.Glob finds %q", pattern, rel(got), rel(std))
		}
	}
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		return // nothing can be made unreadable to root, or by a mode on Windows
	}
	if err := os.Chmod(filepath.Join(root, "b"), 0); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "b"), 0o755) })
	got, err := Glob(filepath.Join(root, "*", "*.jsonl"))
	if err == nil || rel(got) != "a/one.jsonl c/four.jsonl" {
		t.Errorf("with one directory unreadable: %q, %v — want the rest, and an error", rel(got), err)
	}
	if std, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl")); err != nil || len(std) != 2 {
		t.Logf("filepath.Glob: %d matches, %v", len(std), err)
	}
}

// A number outside an int64's range is not an int64, on any platform.
func TestIntAndCount(t *testing.T) {
	for in, want := range map[any]int64{float64(42): 42, float64(-3): -3, 1e19: 0, -1e300: 0, "7": 0, nil: 0} {
		if got := Int(in); got != want {
			t.Errorf("Int(%v) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[any]int64{float64(42): 42, float64(-3): 0, 1e19: 0, 1e12: 1e12, "7": 0} {
		if got := Count(in); got != want {
			t.Errorf("Count(%v) = %d, want %d", in, got, want)
		}
	}
}

// Each line is told where it begins and the sum of what came before it; a read from one
// of those places goes on as the whole read did from there, and Before tells a file that
// grew at its end from one that changed before the place.
func TestLinesFromAPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	huge := strings.Repeat("y", 3<<20)
	content := "one\n\ntwo\r\n" + huge + "\nfour\n"
	os.WriteFile(path, []byte(content), 0o644)
	type seen struct {
		line string
		at   Place
	}
	var whole []seen
	if err := LinesFrom(path, Place{}, func(line []byte, at Place) bool {
		whole = append(whole, seen{string(line), at})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(whole) != 4 {
		t.Fatalf("lines = %d", len(whole))
	}
	for _, s := range whole {
		if !strings.HasPrefix(content[s.at.At:], s.line) {
			t.Errorf("%.10q is said to begin at %d, where %.10q does", s.line, s.at.At, content[s.at.At:])
		}
		if !Before(path, s.at) {
			t.Errorf("the bytes before %.10q are not the ones it was found after", s.line)
		}
	}
	// From the third line on, as the whole read had it
	var rest []seen
	LinesFrom(path, whole[2].at, func(line []byte, at Place) bool {
		rest = append(rest, seen{string(line), at})
		return true
	})
	if len(rest) != 2 || rest[0] != whole[2] || rest[1] != whole[3] {
		t.Errorf("from %+v: %d lines, first at %+v", whole[2].at, len(rest), rest[0].at)
	}

	at := whole[3].at
	os.WriteFile(path, []byte(content+"five\n"), 0o644)
	if !Before(path, at) {
		t.Error("a file that grew at its end is not taken for the same before the place")
	}
	os.WriteFile(path, []byte("ONE"+content[3:]), 0o644)
	if Before(path, at) {
		t.Error("a file changed before the place is taken for the same")
	}
	os.WriteFile(path, []byte(content[:at.At-1]), 0o644)
	if Before(path, at) {
		t.Error("a file cut short of the place is taken for the same")
	}
}
