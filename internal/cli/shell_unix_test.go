//go:build unix

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// asq resume --run becomes the agent: the same process, in the session's directory, so
// that a TERM sent to it is the agent's and nothing is left beneath the agent to die.
func TestResumeHandsTheProcessOver(t *testing.T) {
	if dir := os.Getenv("ASQ_TEST_RESUME_INTO"); dir != "" {
		// The process the test starts: hand it over to a stand-in agent that says who and
		// where it is, then waits for a TERM
		agent := `trap 'echo terminated >> said; exit 0' TERM; echo "$$ $(pwd -P)" > said.new; mv said.new said; i=0; while [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done`
		err := resume(dir, []string{"sh", "-c", agent}, "")
		fmt.Fprintln(os.Stderr, "not handed over:", err)
		os.Exit(3)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestResumeHandsTheProcessOver$")
	cmd.Env = append(os.Environ(), "ASQ_TEST_RESUME_INTO="+dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	said := filepath.Join(dir, "said")
	var line []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if line, err = os.ReadFile(said); err == nil {
			break
		}
	}
	if want := fmt.Sprintf("%d %s", cmd.Process.Pid, dir); strings.TrimSpace(string(line)) != want {
		_ = cmd.Process.Kill()
		t.Fatalf("the agent said %q, want %q: the same process, in the session's directory", line, want)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("the agent, sent a TERM: %v", err)
	}
	if b, _ := os.ReadFile(said); !strings.Contains(string(b), "terminated") {
		t.Errorf("the TERM did not reach the agent: %q", b)
	}
}
