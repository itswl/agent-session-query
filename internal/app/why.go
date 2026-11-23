package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
)

// Blame is what git says of one line of a file: the commit that last changed it.
type Blame struct {
	// Commit is the commit's whole id; empty for a line not committed yet.
	Commit  string    `json:"commit,omitempty"`
	Subject string    `json:"subject,omitempty"`
	At      time.Time `json:"at,omitzero"` // when it was committed
	Text    string    `json:"text"`        // the line as it is now
}

// Why is which session wrote a line of a file: the commit that last changed the line, the
// turns whose calls made that commit, and the other turns that name it.
type Why struct {
	File string `json:"file"` // as it was given
	Path string `json:"path"` // the whole path git was asked of
	Line int    `json:"line"`
	Blame
	Made      []index.Hit `json:"made"`
	Mentioned []index.Hit `json:"mentioned"`
}

// commitLine is the line git commit — and git merge, cherry-pick, revert, which commit
// too — prints for the commit it made: "[main 3f9c2ab] Subject", with "(root-commit)" or a
// detached HEAD's words before the id.
var commitLine = regexp.MustCompile(`(?m)^\[[^\]\n]* ([0-9a-f]{7,64})\] `)

// made reports whether a command's output is git's word that it made commit.
func made(output, commit string) bool {
	for _, m := range commitLine.FindAllStringSubmatch(output, -1) {
		if strings.HasPrefix(commit, m[1]) {
			return true
		}
	}
	return false
}

// Why answers which session wrote line of file, and why: git blame names the commit that
// last changed the line, and the turns are those whose calls printed git's word that they
// made it — found by its id, however short a turn holds it — and the others that name it.
// git is only asked to read: nothing is locked or written in the repository.
func (a *App) Why(ctx context.Context, file string, line int) (Why, error) {
	path, err := filepath.Abs(file)
	if err != nil {
		return Why{}, err
	}
	why := Why{File: file, Path: path, Line: line}
	if why.Blame, err = blame(ctx, path, line); err != nil || why.Commit == "" {
		return why, err
	}
	f := index.Filter{Stores: a.Stores(), Archived: true, Agents: true}
	res, err := a.Search(ctx, index.Query{Terms: []index.Term{{Text: why.Commit}}}, f,
		index.SearchOptions{Limit: 100, PerSession: -1, Recent: true})
	if err != nil {
		return why, err
	}
	why.Made, why.Mentioned = []index.Hit{}, []index.Hit{}
	for _, h := range res.Hits {
		if a.madeIn(ctx, &h, why.Commit) {
			why.Made = append(why.Made, h)
		} else {
			why.Mentioned = append(why.Mentioned, h)
		}
	}
	return why, nil
}

// madeIn reports whether one of a turn's calls printed git's word that it made commit.
func (a *App) madeIn(ctx context.Context, h *index.Hit, commit string) bool {
	turns, err := a.Index.Turns(ctx, &h.Session, index.TurnRange{First: h.Turn, Last: h.Turn})
	if err != nil || len(turns) == 0 {
		return false
	}
	for _, it := range turns[0].Items {
		// A call that failed may have committed all the same — git commit && go test —
		// and git's word that it did is what counts
		if it.Tool == nil || it.Inherited {
			continue
		}
		if made(it.Tool.Output, commit) {
			return true
		}
		if it.Tool.OutBytes > 0 { // a preview: the line may be in the part it leaves out
			if whole, _, err := a.Index.Item(ctx, &h.Session, it.Seq); err == nil && whole.Tool != nil && made(whole.Tool.Output, commit) {
				return true
			}
		}
	}
	return false
}

// blame asks git which commit last changed one line of a file. It runs git blame in the
// file's directory, only to read: optional locks off, so that git writes nothing to the
// repository, no text conversion a repository configures, and no monitor git would start.
func blame(ctx context.Context, path string, line int) (Blame, error) {
	if line < 1 {
		return Blame{}, fmt.Errorf("line %d: lines are counted from 1", line)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", filepath.Dir(path), "-c", "core.fsmonitor=false",
		"blame", "--porcelain", "--no-textconv", "-L", fmt.Sprintf("%d,%d", line, line), "--", filepath.Base(path))
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Blame{}, errors.New("git is not on the PATH: asq asks git blame which commit changed the line")
		}
		if said := strings.TrimSpace(stderr.String()); said != "" {
			said, _, _ = strings.Cut(said, "\n")
			return Blame{}, fmt.Errorf("git blame: %s", strings.TrimPrefix(said, "fatal: "))
		}
		return Blame{}, fmt.Errorf("git blame: %w", err)
	}
	return parseBlame(stdout.String())
}

// parseBlame reads what git blame --porcelain says of one line: the commit's id and
// lines, then the line itself after a tab. A line not committed yet is git's commit of
// zeros.
func parseBlame(out string) (Blame, error) {
	var b Blame
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			id, _, _ := strings.Cut(line, " ")
			if len(id) != 40 && len(id) != 64 { // SHA-1, or SHA-256
				return Blame{}, fmt.Errorf("git blame said %q, which names no commit", line)
			}
			if strings.Trim(id, "0") != "" {
				b.Commit = id
			}
			continue
		}
		if text, ok := strings.CutPrefix(line, "\t"); ok {
			b.Text = text
			break
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "summary":
			if b.Commit != "" {
				b.Subject = value
			}
		case "committer-time":
			if secs, err := strconv.ParseInt(value, 10, 64); err == nil && b.Commit != "" {
				b.At = time.Unix(secs, 0).UTC()
			}
		}
	}
	if first {
		return Blame{}, errors.New("git blame said nothing of the line")
	}
	return b, sc.Err()
}
