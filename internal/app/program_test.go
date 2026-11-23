package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A process notices when the file it was started from has been replaced — renamed over, as
// the installers do, or written over in place — and not when it is only gone.
func TestAProgramReplacedIsNoticed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asq")
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(path, "v1.4.0")
	p := programAt(path)
	if p.replaced() {
		t.Error("a file nobody touched is taken for replaced")
	}

	write(filepath.Join(dir, "asq.new"), "v1.5.0")
	if err := os.Rename(filepath.Join(dir, "asq.new"), path); err != nil {
		t.Fatal(err)
	}
	if !p.replaced() {
		t.Error("a file renamed over the program is not noticed")
	}

	write(path, "v1.4.0")
	q := programAt(path)
	later := time.Now().Add(time.Hour)
	write(path, "v1.4.1") // the same size, written over in place
	os.Chtimes(path, later, later)
	if !q.replaced() {
		t.Error("a file written over in place is not noticed")
	}

	os.Remove(path)
	if programAt(filepath.Join(dir, "none")).replaced() || programAt("").replaced() {
		t.Error("no file at all is taken for one replaced")
	}
	if q.replaced() {
		t.Error("a program file removed is taken for replaced")
	}
}
