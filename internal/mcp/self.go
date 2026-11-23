package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
)

// Caller is what a server started on stdio knows of the agent that started it: which
// session it is working in, when its client says, and where it works. A request over HTTP
// carries neither, and a server answering HTTP has no Caller.
type Caller struct {
	// Session is CLAUDE_CODE_SESSION_ID as the server was started with it, and Project
	// CLAUDE_PROJECT_DIR. Claude Code sets both in the servers it starts on stdio — the
	// session's id as it is when it starts them: a server keeps the id it was started with
	// through a /clear, and one started for --continue, or --resume without an id, may be
	// given the id Claude Code started with rather than the one it resumed.
	//
	// The id is also in the environment of every command Claude Code's Bash tool runs,
	// and so of whatever those start: another agent's CLI run from Claude Code's shell
	// hands it on to the servers it starts in its turn, and there it names the Claude Code
	// session that ran the command, not the agent's own. So both are the caller's only
	// when the client that opened the conversation said it is Claude Code (see client).
	Session string
	Project string
	// Dirs are the directories the server was started in.
	Dirs []string

	mu   sync.Mutex
	name string // the name the client gave itself in initialize, its clientInfo
}

// sessionVar is the variable Claude Code names its session in, and claudeCode the name its
// MCP client gives itself (clientInfo.name) when it opens a conversation.
const (
	sessionVar = "CLAUDE_CODE_SESSION_ID"
	claudeCode = "claude-code"
)

// Stdio is the Caller of a server started on stdio, read from its environment.
func Stdio() *Caller {
	c := &Caller{Session: strings.TrimSpace(os.Getenv(sessionVar)), Project: os.Getenv("CLAUDE_PROJECT_DIR")}
	// As the shell named it, when PWD names it: Getwd prefers that to the system's name, and
	// Self looks under both
	if dir, err := os.Getwd(); err == nil {
		c.Dirs = append(c.Dirs, dir)
	}
	return c
}

// introduce records the name the client gave itself when it opened the conversation.
func (c *Caller) introduce(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name = strings.TrimSpace(name)
}

// client is the name the client gave itself, and whether that is Claude Code.
func (c *Caller) client() (name string, claude bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name, c.name == claudeCode
}

// meta is what a request's _meta says of who sent it, read leniently: a value of another
// shape is absent, and never makes the request unreadable.
type meta struct {
	// thread is the conversation a Codex tool call was made in: Codex sends it with each
	// call (openai/codex#18093, _meta.threadId), and it is the id of that thread's session.
	thread string
	// client is the name the client gives itself in the request, as the 2026-07-28
	// revision has every request say it (io.modelcontextprotocol/clientInfo).
	client string
}

func (m *meta) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	_ = json.Unmarshal(fields["threadId"], &m.thread)
	var info struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(fields[metaClientInfo], &info) == nil {
		m.client = info.Name
	}
	m.thread, m.client = strings.TrimSpace(m.thread), strings.TrimSpace(m.client)
	return nil
}

// asker is what a request said of who sent it.
type asker struct{ thread, client string }

func (m meta) asker() asker { return asker{thread: m.thread, client: m.client} }

// clientOf is the name the client gave itself — in the request, or else when it opened the
// conversation — and whether that is Claude Code.
func (s *Server) clientOf(who asker) (name string, claude bool) {
	if who.client != "" {
		return who.client, who.client == claudeCode
	}
	if s.Caller != nil {
		return s.Caller.client()
	}
	return "", false
}

// thread is the Codex session the request names as the conversation it was made in, when
// it names one and the index has it. why says, when it names one the index does not have,
// that it was named.
func (s *Server) thread(ctx context.Context, who asker) (self app.Self, found bool, why string, err error) {
	if who.thread == "" {
		return self, false, "", nil
	}
	self, err = s.App.Self(ctx, "codex", who.thread, nil)
	switch {
	case err == nil && self.ByID:
		return self, true, "", nil
	case err != nil && !errors.Is(err, index.ErrNotFound):
		return self, false, "", err
	}
	return app.Self{}, false, fmt.Sprintf("The request names Codex thread %s, which is not among the sessions this server reads.", who.thread), nil
}

// workDir is the directory the agent asking works in, and how that is known: where the
// Codex thread the request names ran, where Claude Code says its project is, or where the
// server was started. Over HTTP, with no thread named, nothing says.
func (s *Server) workDir(ctx context.Context, who asker) (dir, how string, err error) {
	t, found, _, err := s.thread(ctx, who)
	if err != nil {
		return "", "", err
	}
	if found && t.Cwd != "" {
		return t.Cwd, fmt.Sprintf("where %s, the Codex thread the request names, works", t.Ref), nil
	}
	if c := s.Caller; c != nil {
		if _, claude := s.clientOf(who); claude && c.Project != "" {
			return c.Project, "the project Claude Code names to this server (CLAUDE_PROJECT_DIR)", nil
		}
		if len(c.Dirs) > 0 {
			return c.Dirs[0], "the directory this server was started in", nil
		}
	}
	return "", "", errors.New(`"." is the directory you work in, and a request over HTTP does not say which that is unless it names its Codex thread: name the directory, or a part of its path — projects lists them`)
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
func (s *Server) session(ctx context.Context, ref string, who asker) (sess index.Session, turn int, said string, err error) {
	turn, this := thisRef(ref)
	if !this {
		sess, turn, err = s.App.Resolve(ctx, ref)
		return sess, turn, "", err
	}
	// The conversation the request names is the one asking: Codex names it with each call
	t, found, unknown, err := s.thread(ctx, who)
	if err != nil {
		return sess, turn, "", err
	}
	if found {
		return t.Session, turn, fmt.Sprintf(`[ref "this" is %s: the Codex thread the request names (_meta.threadId).]`, t.Ref), nil
	}
	if s.Caller == nil {
		if unknown != "" {
			return sess, turn, "", fmt.Errorf(`ref "this": %s It may be on another machine than this server: name the session by its ref — sessions lists them`, unknown)
		}
		return sess, turn, "", errors.New(`ref "this" is the session asking, and a request over HTTP says which that is only when its client names it, as Codex does with each call (_meta.threadId); asq mcp started on stdio can tell Claude Code's too (` + sessionVar + `). Name the session by its ref: sessions lists them, newest first, and project narrows them to a directory`)
	}
	c := s.Caller
	// Claude Code's variables are taken from Claude Code alone: inherited by another
	// agent, they name the Claude Code session it was started from
	client, claude := s.clientOf(who)
	id, dirs, source := "", c.Dirs, ""
	if claude {
		id = c.Session
		if c.Project != "" {
			dirs = append([]string{c.Project}, dirs...)
		}
		if id != "" {
			source = "claude" // the variable is Claude Code's, and so is the session it names
		}
	}
	self, err := s.App.Self(ctx, source, id, dirs)
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
	case id != "":
		said = fmt.Sprintf(`[ref "this" is %s: the Claude Code session most recently written to in %s. The session %s names, %s, is not in the index: Claude Code started with --continue, or --resume without an id, can name the session it started with rather than the one it resumed.`,
			self.Ref, self.Cwd, sessionVar, c.Session)
	case c.Session != "" && !claude:
		who := "did not say it is Claude Code"
		if client != "" {
			who = fmt.Sprintf("is %q, not Claude Code", client)
		}
		said = fmt.Sprintf(`[ref "this" is %s: the session most recently written to in %s. The client %s, so the %s this server was started with is not taken for its own: every program started from Claude Code's shell inherits that variable, and there it names the Claude Code session the program was started from. If another agent works in that directory, this may be its session — name the one meant by its ref.`,
			self.Ref, self.Cwd, who, sessionVar)
	default:
		said = fmt.Sprintf(`[ref "this" is %s: the session most recently written to in %s. The client did not name the session asking (Claude Code does, in %s): if another agent works in that directory, this may be its session — name the one meant by its ref.`,
			self.Ref, self.Cwd, sessionVar)
	}
	if unknown != "" {
		said += " " + unknown
	}
	return self.Session, turn, said + "]", nil
}
