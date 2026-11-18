package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/itswl/agent-session-query/internal/index"
)

// Caller is what a server started on stdio knows of the agent that started it: which
// session it is working in, when its client says, and where it works. A request over HTTP
// carries neither, and a server answering HTTP has no Caller.
type Caller struct {
	// Session is the id of the session the client started this server in. Claude Code
	// sets CLAUDE_CODE_SESSION_ID in the servers it starts on stdio, as it is when it
	// starts them: a server keeps the id it was started with through a /clear, and one
	// started for --continue, or --resume without an id, may be given the id Claude Code
	// started with rather than the one it resumed.
	Session string
	// Dirs are the directories the agent works in: CLAUDE_PROJECT_DIR, which Claude Code
	// sets to the project's root in the servers it starts, and the directory the server
	// was started in.
	Dirs []string
}

// sessionVar is the variable Claude Code names its session in.
const sessionVar = "CLAUDE_CODE_SESSION_ID"

// Stdio is the Caller of a server started on stdio, read from its environment.
func Stdio() *Caller {
	c := &Caller{Session: strings.TrimSpace(os.Getenv(sessionVar))}
	if dir := os.Getenv("CLAUDE_PROJECT_DIR"); dir != "" {
		c.Dirs = append(c.Dirs, dir)
	}
	// As the shell named it, when PWD names it: Getwd prefers that to the system's name, and
	// Self looks under both
	if dir, err := os.Getwd(); err == nil {
		c.Dirs = append(c.Dirs, dir)
	}
	return c
}

// thisRef reads a ref that names the session asking: "this", or "this#4" for one of its
// turns (-1 for none).
func thisRef(ref string) (turn int, ok bool) {
	name, n, has := strings.Cut(strings.TrimSpace(ref), "#")
	if !strings.EqualFold(name, "this") {
		return 0, false
	}
	if !has {
		return -1, true
	}
	turn, err := strconv.Atoi(n)
	if err != nil || turn < 0 {
		return 0, false
	}
	return turn, true
}

// session resolves the ref a tool was given: a session as the tools print it, or "this",
// the session asking. said, for "this", says which session it was taken to be and why:
// the answer is about that session, and the agent must be able to see which it is.
func (s *Server) session(ctx context.Context, ref string) (sess index.Session, turn int, said string, err error) {
	turn, this := thisRef(ref)
	if !this {
		sess, turn, err = s.App.Resolve(ctx, ref)
		return sess, turn, "", err
	}
	if s.Caller == nil {
		return sess, turn, "", errors.New(`ref "this" is the session asking, and only asq mcp started on stdio can tell which that is: Claude Code names it to the servers it starts (` + sessionVar + `), and a request over HTTP carries nothing that does. Name the session by its ref: sessions lists them, newest first, and project narrows them to a directory`)
	}
	c := s.Caller
	source := ""
	if c.Session != "" {
		source = "claude" // the variable is Claude Code's, and so is the session it names
	}
	self, err := s.App.Self(ctx, source, c.Session, c.Dirs)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			err = fmt.Errorf(`ref "this": %w — name the session by its ref: sessions lists them`, err)
		}
		return sess, turn, "", err
	}
	switch {
	case self.ByID:
		said = fmt.Sprintf(`[ref "this" is %s: the session Claude Code named when it started this server (%s).`, self.Ref, sessionVar)
		if n := self.Newer; n != nil {
			said += fmt.Sprintf(` %s, in the same directory, was written to after it: Claude Code goes on in a new session after /clear and this server keeps the name it was given, or another agent works there too — if that one is this conversation, name it by its ref.`, n.Ref)
		}
	case c.Session != "":
		said = fmt.Sprintf(`[ref "this" is %s: the Claude Code session most recently written to in %s. The session %s names, %s, is not in the index: Claude Code started with --continue, or --resume without an id, can name the session it started with rather than the one it resumed.`,
			self.Ref, self.Cwd, sessionVar, c.Session)
	default:
		said = fmt.Sprintf(`[ref "this" is %s: the session most recently written to in %s. The client did not name the session asking (Claude Code does, in %s): if another agent works in that directory, this may be its session — name the one meant by its ref.`,
			self.Ref, self.Cwd, sessionVar)
	}
	return self.Session, turn, said + "]", nil
}
