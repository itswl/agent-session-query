package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/fixture"
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
