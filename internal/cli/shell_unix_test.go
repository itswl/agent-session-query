//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A resumed agent has the terminal, and Ctrl+C is the agent's to read: however many
// come, asq waits for it beneath. A TERM sent to asq is the agent's too.
func TestAResumedAgentOutlivesCtrlC(t *testing.T) {
	said := filepath.Join(t.TempDir(), "said")
	// What a terminal does on Ctrl+C, twice: a SIGINT to asq (and to the agent, which
	// here takes no notice)
	line := `trap '' INT; kill -INT $PPID; sleep 0.2; kill -INT $PPID; sleep 0.2; echo still here > ` + said
	if err := runShell(line); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(said); err != nil || strings.TrimSpace(string(b)) != "still here" {
		t.Fatalf("the agent did not run to its end: %q, %v", b, err)
	}

	start := time.Now()
	err := runShell(`kill -TERM $PPID; exec sleep 5`)
	if err == nil || !strings.Contains(err.Error(), "terminated") || time.Since(start) > 3*time.Second {
		t.Errorf("a TERM to asq was not passed on: %v after %v", err, time.Since(start))
	}
}
