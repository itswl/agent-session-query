package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// Refused is why a line could not be blamed, where what was asked can be set right by the
// one asking — the line is not in the file, the file is in no repository, the path is
// relative over HTTP — in git's words, or asq's. The rest of what goes wrong asking git is
// the server's own, and is not wrapped: git not on the PATH, or saying nothing of the line.
// The HTTP API answers the first as the caller's, 400, and the second as its own, 500.
type Refused struct{ Err error }

func (r Refused) Error() string { return r.Err.Error() }
func (r Refused) Unwrap() error { return r.Err }

// commitLine is the line git commit — and git merge, cherry-pick, revert, which commit
// too — prints for the commit it made: "[main 3f9c2ab] Subject", with "(root-commit)" or a
// detached HEAD's words before the id. Gemini CLI's shell tool hands on what a command
// printed after "Output: ", so its first line opens with that.
var commitLine = regexp.MustCompile(`(?m)^(?:Output: )?\[[^\]\n]* ([0-9a-f]{7,64})\] `)

// made reports whether a command's output is git's word that it made commit.
func made(output, commit string) bool {
	for _, m := range commitLine.FindAllStringSubmatch(output, -1) {
		if strings.HasPrefix(commit, m[1]) {
			return true
		}
	}
	// A shell tool that answers in JSON — Hermes's: {"output": "…", "exit_code": 0} — holds
	// what the command printed in a field, its lines one line of JSON
	var doc struct{ Output, Stdout string }
	if strings.HasPrefix(output, "{") && json.Unmarshal([]byte(output), &doc) == nil {
		return made(doc.Output, commit) || made(doc.Stdout, commit)
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
		return Why{}, Refused{err}
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
	// What the list would show of each turn, as a search's hits are shown: every face that
	// answers with these reads them so
	why.Made, why.Mentioned = ListedHits(index.SearchResult{Hits: why.Made}).Hits, ListedHits(index.SearchResult{Hits: why.Mentioned}).Hits
	return why, nil
}

// WhyOverHTTP is Why for a request that came over HTTP — the API's route, or the MCP
// server's — from someone not at this machine's shell, where no process was started in the
// directory a relative path would be in. So the path must be whole. And the server would run
// git in the directory named to it, so the path must be under one that a session asq reads
// ran in, which projects lists: a token to read the history is not a key to every repository
// on the machine, while the work an agent has in hand is nearly always in one a session ran
// in. A server started on stdio is asked as the command line is (Why).
func (a *App) WhyOverHTTP(ctx context.Context, file string, line int) (Why, error) {
	if !filepath.IsAbs(file) {
		return Why{}, Refused{fmt.Errorf("path %q is relative, and a request over HTTP does not say which directory you mean: give the whole path", file)}
	}
	// The directories of the stores that ran here (hereStores), as Paths takes them
	cwds, err := a.Index.Cwds(ctx, a.hereStores())
	if err != nil {
		return Why{}, err
	}
	if !underAny(cwds, file) {
		return Why{}, Refused{fmt.Errorf("path %q is under no directory a session asq reads ran in, and over HTTP why answers only of those: projects lists them", file)}
	}
	return a.Why(ctx, file, line)
}

// underAny reports whether path is beneath one of dirs, by where each is with its links
// followed — as /tmp is /private/tmp on a Mac, so either spelling of a directory is taken —
// and without regard to case, as dirHolds compares them. The path's side is followed all the
// way, since where git would run is what is judged: a link inside a session's directory that
// leads out of it does not bring what it leads to in reach.
func underAny(dirs []string, path string) bool {
	path = filepath.Clean(path)
	file := filepath.Join(followed(filepath.Dir(path)), filepath.Base(path))
	for _, dir := range dirs {
		if dirHolds(followed(dir), file, true) {
			return true
		}
	}
	return false
}

// followed is dir with its links followed, from the nearest parent of it that is there: a
// directory that is not there is taken to be where its parents lead, and git will only say
// that it is missing.
func followed(dir string) string {
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{real}, tail...)...)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(append([]string{dir}, tail...)...) // the root itself is not there
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
	}
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
		return Blame{}, Refused{fmt.Errorf("line %d: lines are counted from 1", line)}
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
			return Blame{}, Refused{fmt.Errorf("git blame: %s", strings.TrimPrefix(said, "fatal: "))}
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
