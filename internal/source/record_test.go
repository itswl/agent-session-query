package source

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEachJSONLLine: every line arrives once, in order, the view valid only for its call
func TestEachJSONLLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	content := "{\"a\":1}\n\n  {\"b\":2}  \n{\"c\":3}" // blank line, spaces, no final newline
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	EachJSONLLine(path, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}

	// Returning false stops the walk
	stopped := 0
	EachJSONLLine(path, func(line []byte) bool { stopped++; return false })
	if stopped != 1 {
		t.Fatalf("early stop read %d lines, want 1", stopped)
	}

	// A missing file is silence, as before
	EachJSONLLine(filepath.Join(dir, "nope.jsonl"), func([]byte) bool {
		t.Fatal("a missing file produced a line")
		return false
	})
}

// TestEachJSONLLineFrom: a resumed counter reads only the tail, and an offset beyond the
// file yields nothing
func TestEachJSONLLineFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	head := "{\"a\":1}\n{\"b\":2}\n"
	if err := os.WriteFile(path, []byte(head+"{\"c\":3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	eachJSONLLineFrom(path, int64(len(head)), func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 1 || got[0] != `{"c":3}` {
		t.Fatalf("tail = %q, want the third line only", got)
	}

	got = nil
	eachJSONLLineFrom(path, 4096, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 0 {
		t.Fatalf("an offset past the end read %q", got)
	}
}

// TestEachJSONLLineLongLine: a line longer than the read buffer arrives whole and once,
// with the lines around it intact — the case the gather path exists for
func TestEachJSONLLineLongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	long := bytes.Repeat([]byte("x"), 3*scanBufSteady+7) // two refills, then the gather
	content := append([]byte("{\"first\":1}\n"), long...)
	content = append(content, '\n')
	content = append(content, []byte("{\"last\":2}\n")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got := [][]byte{}
	EachJSONLLine(path, func(line []byte) bool {
		got = append(got, append([]byte(nil), line...))
		return true
	})
	if len(got) != 3 {
		t.Fatalf("read %d lines, want 3 (first, the long one once, last)", len(got))
	}
	if string(got[0]) != `{"first":1}` || string(got[2]) != `{"last":2}` {
		t.Fatalf("lines around the long one = %q / %q", got[0], got[2])
	}
	if !bytes.Equal(got[1], long) {
		t.Fatalf("the long line came back with %d bytes, want %d", len(got[1]), len(long))
	}
}

// TestEachJSONLLineExactBuffer: a line that exactly fills the buffer, newline next, still
// arrives once (the gather path with the newline as its first byte)
func TestEachJSONLLineExactBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	exact := bytes.Repeat([]byte("y"), scanBufSteady)
	content := append(append([]byte{}, exact...), '\n')
	content = append(content, []byte("{\"next\":1}\n")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	EachJSONLLine(path, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 2 || len(got[0]) != scanBufSteady || got[1] != `{"next":1}` {
		t.Fatalf("lines = %d, first %d bytes, second %q", len(got), len(got[0]), got[1])
	}
}
