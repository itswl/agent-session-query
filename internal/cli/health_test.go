package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/fixture"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// An index file that is not a database fails doctor, and the next command sets it aside,
// says so, and answers from one made anew.
func TestADamagedIndexIsSetAsideByTheNextCommand(t *testing.T) {
	h, indexPath := setup(t)
	if err := os.WriteFile(indexPath, []byte(strings.Repeat("not a database ", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, code := asq(t, h, indexPath, "doctor")
	if code != 1 || !strings.Contains(out, "✗ index        DAMAGED") || !strings.Contains(out, "file is not a database") || !strings.Contains(out, "asq index rebuild") {
		t.Errorf("doctor of a damaged index: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code = asq(t, h, indexPath, "doctor", "--json")
	var report struct {
		OK    bool `json:"ok"`
		Index struct {
			Found   bool   `json:"found"`
			Problem string `json:"problem"`
		} `json:"index"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 1 || report.OK || !report.Index.Found || !strings.Contains(report.Index.Problem, "not a database") {
		t.Errorf("doctor --json of a damaged index: exit %d, %v\n%s%s", code, err, out, errs)
	}

	out, errs, code = asq(t, h, indexPath, "ls")
	if code != 0 || !strings.Contains(errs, "asq: the index was damaged (file is not a database (26)): it was set aside as "+indexPath+".damaged") || !strings.Contains(out, "Fix flaky test") {
		t.Errorf("ls of a damaged index: exit %d\n%s%s", code, out, errs)
	}
	out, errs, code = asq(t, h, indexPath, "doctor")
	if code != 0 || !strings.Contains(out, "✓ index        ") || !strings.Contains(out, "holds together") {
		t.Errorf("doctor once it is made anew: exit %d\n%s%s", code, out, errs)
	}
}

// Damage deeper in, which a question meets and opening the index does not, ends the
// command with what to do about it; asq index rebuild then does it.
func TestDamageMetByAQuestionNamesTheRebuild(t *testing.T) {
	h, indexPath := setup(t)
	if _, errs, code := asq(t, h, indexPath, "ls"); code != 0 {
		t.Fatalf("ls: %s", errs)
	}
	fixture.Damage(t, indexPath, "words_data")
	out, errs, code := asq(t, h, indexPath, "search", "flaky")
	if code != 1 || !strings.Contains(errs, "asq: the index is damaged: asq index rebuild makes it anew from the stores") {
		t.Errorf("a search that meets damage: exit %d\n%s%s", code, out, errs)
	}
	if out, errs, code := asq(t, h, indexPath, "doctor"); code != 1 || !strings.Contains(out, "✗ index        DAMAGED") {
		t.Errorf("doctor: exit %d\n%s%s", code, out, errs)
	}
	if out, errs, code := asq(t, h, indexPath, "index", "rebuild"); code != 0 {
		t.Fatalf("rebuild: exit %d\n%s%s", code, out, errs)
	}
	if out, errs, code := asq(t, h, indexPath, "search", "flaky"); code != 0 || !strings.Contains(out, "1 turn in 1 session") {
		t.Errorf("a search after the rebuild: exit %d\n%s%s", code, out, errs)
	}
}

// With no store found anywhere, an empty answer says where asq looked; one found and empty
// says so too. A history with sessions in it says nothing of the kind.
func TestNothingFoundSaysWhereItLooked(t *testing.T) {
	h, indexPath := setup(t)
	if _, errs, _ := asq(t, h, indexPath, "ls"); strings.Contains(errs, "Looked in") {
		t.Errorf("a history with sessions says where it looked:\n%s", errs)
	}

	nowhere := filepath.Join(t.TempDir(), "no", "such", "store")
	var out, errs bytes.Buffer
	code := Run([]string{"ls", "--index", filepath.Join(t.TempDir(), "index.db"), "--only", "claude", "--path", "claude=" + nowhere}, strings.NewReader(""), &out, &errs)
	if code != 0 || out.String() != "No sessions.\n" || !strings.Contains(errs.String(), "asq: no CLI's sessions were found. Looked in:\n  claude") ||
		!strings.Contains(errs.String(), nowhere) || !strings.Contains(errs.String(), "not there") || !strings.Contains(errs.String(), "asq doctor says more") {
		t.Errorf("ls with no store: exit %d\n%s%s", code, &out, &errs)
	}

	empty := t.TempDir()
	out.Reset()
	errs.Reset()
	code = Run([]string{"search", "flaky", "--index", filepath.Join(t.TempDir(), "index.db"), "--only", "claude", "--path", "claude=" + empty}, strings.NewReader(""), &out, &errs)
	if code != 0 || !strings.Contains(errs.String(), "there, and empty") {
		t.Errorf("search of an empty store: exit %d\n%s%s", code, &out, &errs)
	}

	// A store that is there and could not be read is not said to be empty
	t.Run("unreadable", func(t *testing.T) {
		locked := filepath.Join(t.TempDir(), "projects")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		sourcetest.Unreadable(t, locked)
		var out, errs bytes.Buffer
		code := Run([]string{"ls", "--index", filepath.Join(t.TempDir(), "index.db"), "--only", "claude", "--path", "claude=" + locked}, strings.NewReader(""), &out, &errs)
		if code != 0 || !strings.Contains(errs.String(), "claude could not be read") || !strings.Contains(errs.String(), "there, and could not be read") ||
			strings.Contains(errs.String(), "there, and empty") {
			t.Errorf("ls of a store that could not be read: exit %d\n%s%s", code, &out, &errs)
		}
	})
}
