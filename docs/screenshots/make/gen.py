# Synthetic history for the asq screenshots: a fake HOME with a Claude Code store and a
# Codex store, and the project directories the sessions worked in. Nothing here is real.
import datetime as dt
import json
import os
import random
import shutil
import string

# Both are emptied and written afresh on every run: they are this script's, not anyone's
HOME = '/tmp/demo'
WORK = '/tmp/work'
CLAUDE = HOME + '/.claude/projects'
CODEX = HOME + '/.codex/sessions'

rnd = random.Random(20261117)
NOW = dt.datetime.now().astimezone().replace(microsecond=0)
TODAY = NOW.date()


def ago(minutes):
    return NOW - dt.timedelta(minutes=minutes)


def on(days_ago, hhmm):
    h, m = map(int, hhmm.split(':'))
    d = TODAY - dt.timedelta(days=days_ago)
    return dt.datetime(d.year, d.month, d.day, h, m, rnd.randint(0, 59), tzinfo=NOW.tzinfo)


def iso(t):
    u = t.astimezone(dt.timezone.utc)
    return u.strftime('%Y-%m-%dT%H:%M:%S.') + '%03dZ' % (u.microsecond // 1000)


def rid(prefix, n, alphabet=string.ascii_letters + string.digits):
    return prefix + ''.join(rnd.choice(alphabet) for _ in range(n))


def uid():
    h = '%032x' % rnd.getrandbits(128)
    return '%s-%s-4%s-%s%s-%s' % (h[:8], h[8:12], h[13:16], rnd.choice('89ab'), h[17:20], h[20:32])


def tok(s):
    return max(1, len(s) // 4)


def tabs(s):
    # Go source is written here with four spaces; gofmt indents with tabs
    out = []
    for line in s.split('\n'):
        n = len(line) - len(line.lstrip(' '))
        out.append('\t' * (n // 4) + ' ' * (n % 4) + line[n:])
    return '\n'.join(out)


def catn(content, start=1, end=None):
    lines = content.split('\n')
    if end is None:
        end = len(lines)
    return '\n'.join('%6d→%s' % (i, lines[i - 1]) for i in range(start, min(end, len(lines)) + 1))


WRITES = {}  # absolute path -> last time a session wrote it


def wrote(path, t):
    if path not in WRITES or WRITES[path] < t:
        WRITES[path] = t


def dump(path, rows, mtime):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w') as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False, separators=(',', ':')) + '\n')
    ts = mtime.timestamp()
    os.utime(path, (ts, ts))


def claude_dir(cwd):
    return CLAUDE + '/' + ''.join(c if c.isalnum() else '-' for c in cwd)


# ---------------------------------------------------------------- Claude Code

class Claude:
    def __init__(self, project, branch, title, model='claude-opus-5-5', sid=None, base=None):
        self.cwd = WORK + '/' + project
        self.branch = branch
        self.title = title
        self.model = model
        self.sid = sid or uid()
        self.rows = []
        self.t = None
        self.parent = None
        self.ctx = base or rnd.randint(38000, 52000)  # what the cache holds
        self.fresh = 0                                 # what was added since the last call
        self.agents = []

    def _row(self, kind, extra):
        r = {'parentUuid': self.parent, 'isSidechain': False, 'userType': 'external', 'cwd': self.cwd,
             'sessionId': self.sid, 'version': '2.1.300', 'gitBranch': self.branch, 'type': kind}
        r.update(extra)
        r['uuid'] = uid()
        r['timestamp'] = iso(self.t)
        self.parent = r['uuid']
        self.rows.append(r)
        return r

    def wait(self, lo, hi=None):
        self.t += dt.timedelta(seconds=lo if hi is None else rnd.uniform(lo, hi))

    def ask(self, text, at=None, after=None):
        if at is not None:
            self.t = at
        if after is not None:
            self.wait(after)
        self._row('user', {'message': {'role': 'user', 'content': text}})
        self.fresh += tok(text) + (rnd.randint(2500, 5200) if len(self.rows) == 1 else 40)

    def _respond(self, content, stop, think=0):
        self.wait(4, 14)
        out = sum(tok(json.dumps(c)) for c in content) + think + rnd.randint(60, 420)
        usage = {'input_tokens': rnd.randint(2, 9), 'cache_creation_input_tokens': self.fresh,
                 'cache_read_input_tokens': self.ctx, 'output_tokens': out}
        self._row('assistant', {'message': {'id': rid('msg_01', 22), 'type': 'message', 'role': 'assistant',
                                            'model': self.model, 'content': content, 'stop_reason': stop,
                                            'stop_sequence': None, 'usage': usage}})
        self.ctx += self.fresh + out
        self.fresh = 0

    def tool(self, name, inp, out, error=False, dur=None, think=None, say=None, result_extra=None):
        content = []
        thinking_tokens = 0
        if think:
            content.append({'type': 'thinking', 'thinking': think, 'signature': ''})
            thinking_tokens = tok(think) * 6
        if say:
            content.append({'type': 'text', 'text': say})
        tid = rid('toolu_01', 22)
        content.append({'type': 'tool_use', 'id': tid, 'name': name, 'input': inp})
        self._respond(content, 'tool_use', thinking_tokens)
        d = dur if dur is not None else rnd.uniform(0.6, 2.5)
        self.wait(d)
        res = {'type': 'tool_result', 'tool_use_id': tid, 'content': out}
        if error:
            res['is_error'] = True
        use = {'durationMs': int(d * 1000)}
        if result_extra:
            use.update(result_extra)
        self._row('user', {'message': {'role': 'user', 'content': [res]}, 'toolUseResult': use})
        self.fresh += tok(out if isinstance(out, str) else json.dumps(out)) + rnd.randint(250, 1400)  # the result, and the reminders around it
        if name in ('Edit', 'Write', 'MultiEdit') and not error:
            wrote(inp['file_path'], self.t)
        return tid

    # The tools, as Claude Code names and shapes them
    def bash(self, command, out, desc, error=False, dur=None, **kw):
        return self.tool('Bash', {'command': command, 'description': desc}, out, error, dur, **kw)

    def read(self, path, content, start=1, end=None, **kw):
        inp = {'file_path': path}
        if start != 1 or end is not None:
            inp['offset'] = start
            inp['limit'] = (end or len(content.split('\n'))) - start + 1
        return self.tool('Read', inp, catn(content, start, end), dur=kw.pop('dur', rnd.uniform(0.05, 0.3)), **kw)

    def read_missing(self, path, **kw):
        return self.tool('Read', {'file_path': path},
                         '<tool_use_error>File does not exist. Note: your current working directory is %s.</tool_use_error>' % self.cwd,
                         error=True, dur=0.02, **kw)

    def edit(self, path, old, new, **kw):
        return self.tool('Edit', {'file_path': path, 'old_string': old, 'new_string': new},
                         'The file %s has been updated successfully.' % path, dur=kw.pop('dur', rnd.uniform(0.05, 0.2)), **kw)

    def write(self, path, content, **kw):
        return self.tool('Write', {'file_path': path, 'content': content},
                         'File created successfully at: %s' % path, dur=kw.pop('dur', rnd.uniform(0.05, 0.2)), **kw)

    def grep(self, pattern, out, path=None, mode='content', **kw):
        inp = {'pattern': pattern}
        if path:
            inp['path'] = path
        inp['output_mode'] = mode
        if mode == 'content':
            inp['-n'] = True
        return self.tool('Grep', inp, out, dur=kw.pop('dur', rnd.uniform(0.1, 0.6)), **kw)

    def agent(self, kind, desc, prompt, steps, report, think=None, model='claude-haiku-5-5'):
        """Start a subagent: its transcript is a file of its own, and its report is the result."""
        tid_holder = {}
        start = None
        # the call
        content = []
        if think:
            content.append({'type': 'thinking', 'thinking': think, 'signature': ''})
        tid = rid('toolu_01', 22)
        content.append({'type': 'tool_use', 'id': tid, 'name': 'Agent',
                        'input': {'description': desc, 'prompt': prompt, 'subagent_type': kind}})
        self._respond(content, 'tool_use', tok(think or '') * 6)
        start = self.t
        aid = '%016x' % rnd.getrandbits(64)
        sub = Claude(self.cwd[len(WORK) + 1:], self.branch, desc, model=model, sid=self.sid, base=rnd.randint(11000, 14000))
        sub.t = start + dt.timedelta(seconds=0.4)
        sub.ask(prompt)
        for st in steps:
            st(sub)
        sub.wait(1)
        sub._respond([{'type': 'text', 'text': report}], 'end_turn')
        for r in sub.rows:
            r['isSidechain'] = True
            r['agentId'] = aid
        self.agents.append((aid, sub, {'agentType': kind, 'description': desc, 'toolUseId': tid}))
        self.t = sub.t + dt.timedelta(seconds=0.5)
        self._row('user', {'message': {'role': 'user', 'content': [
            {'type': 'tool_result', 'tool_use_id': tid, 'content': [{'type': 'text', 'text': report}]}]},
            'toolUseResult': {'durationMs': int((self.t - start).total_seconds() * 1000)}})
        self.fresh += tok(report) + 30

    def api_error(self, text):
        """The CLI giving up on the model, once its retries are spent: its words in the model's place."""
        self.wait(25, 45)
        self._row('assistant', {'message': {'id': uid(), 'type': 'message', 'role': 'assistant', 'model': '<synthetic>',
                                            'content': [{'type': 'text', 'text': text}], 'stop_reason': 'stop_sequence',
                                            'stop_sequence': '', 'usage': {'input_tokens': 0, 'output_tokens': 0}},
                                'isApiErrorMessage': True})

    def reply(self, text, think=None):
        content = []
        if think:
            content.append({'type': 'thinking', 'thinking': think, 'signature': ''})
        content.append({'type': 'text', 'text': text})
        self._respond(content, 'end_turn', tok(think or '') * 6)
        self.wait(0.3)

    def save(self):
        d = claude_dir(self.cwd)
        rows = list(self.rows)
        rows.insert(2, {'type': 'ai-title', 'aiTitle': self.title, 'sessionId': self.sid})
        dump('%s/%s.jsonl' % (d, self.sid), rows, self.t)
        for aid, sub, meta in self.agents:
            base = '%s/%s/subagents/agent-%s' % (d, self.sid, aid)
            dump(base + '.jsonl', sub.rows, sub.t)
            with open(base + '.meta.json', 'w') as f:
                json.dump(meta, f)
            os.utime(base + '.meta.json', (sub.t.timestamp(), sub.t.timestamp()))


# ---------------------------------------------------------------- Codex

class Codex:
    def __init__(self, project, branch, model='gpt-6.1', at=None):
        self.cwd = WORK + '/' + project
        self.branch = branch
        self.model = model
        self.sid = uid()
        self.t = at
        self.start = at
        self.rows = []
        self.ctx = rnd.randint(17000, 23000)
        self.turn = None
        self.pending_out = []
        self.rows.append({'timestamp': iso(at), 'type': 'session_meta', 'payload': {
            'id': self.sid, 'timestamp': iso(at), 'cwd': self.cwd, 'originator': 'codex_cli_rs',
            'cli_version': '0.167.0', 'source': 'cli', 'model_provider': 'openai', 'git': {'branch': branch}}})
        self._item({'type': 'message', 'role': 'developer', 'content': [
            {'type': 'input_text', 'text': '<permissions instructions>Filesystem sandboxing: workspace-write. Approval policy: on-request.</permissions instructions>'}]})

    def wait(self, lo, hi=None):
        self.t += dt.timedelta(seconds=lo if hi is None else rnd.uniform(lo, hi))

    def _item(self, payload, kind='response_item'):
        self.rows.append({'timestamp': iso(self.t), 'type': kind, 'payload': payload})

    def ask(self, text, at=None, after=None):
        if at is not None:
            self.t = at
        if after is not None:
            self.wait(after)
        self._item({'type': 'message', 'role': 'user', 'content': [{'type': 'input_text', 'text': text}]})
        self.turn = uid()
        self._item({'turn_id': self.turn, 'cwd': self.cwd, 'approval_policy': 'on-request',
                    'sandbox_policy': {'type': 'workspace-write'}, 'model': self.model, 'effort': 'medium',
                    'summary': 'auto'}, kind='turn_context')
        self.fresh = tok(text) + 60

    def respond(self, reasoning=None, calls=(), say=None):
        """One model response: its reasoning, its words and calls, the record of what it cost,
        then the outputs of its calls."""
        self.wait(3, 11)
        out = rnd.randint(30, 160)
        reasoning_tokens = 0
        if reasoning:
            self._item({'type': 'reasoning', 'id': rid('rs_', 24), 'summary': [{'type': 'summary_text', 'text': reasoning}], 'content': None})
            reasoning_tokens = tok(reasoning) * 3 + rnd.randint(40, 300)
        if say:
            self._item({'type': 'message', 'role': 'assistant', 'id': rid('msg_', 24), 'content': [{'type': 'output_text', 'text': say}]})
            out += tok(say)
        outs = []
        for c in calls:
            cid = rid('call_', 24)
            if c[0] == 'exec':
                _, cmd, output, code, secs = c
                args = json.dumps({'cmd': cmd, 'workdir': self.cwd, 'yield_time_ms': 10000})
                self._item({'type': 'function_call', 'id': rid('fc_', 24), 'name': 'exec_command', 'arguments': args, 'call_id': cid})
                out += tok(args)
                body = 'Exit code: %d\nWall time: %.1f seconds\nOutput:\n%s' % (code, secs, output)
                outs.append(({'type': 'function_call_output', 'call_id': cid, 'output': body}, secs, None))
            elif c[0] == 'patch':
                _, patch, files = c
                self._item({'type': 'custom_tool_call', 'id': rid('ctc_', 24), 'status': 'completed', 'call_id': cid,
                            'name': 'apply_patch', 'input': patch})
                out += tok(patch)
                body = 'Exit code: 0\nWall time: 0.1 seconds\nOutput:\nSuccess. Updated the following files:\n' + \
                       ''.join('%s %s\n' % (k, p) for k, p in files)
                outs.append(({'type': 'custom_tool_call_output', 'call_id': cid, 'output': body}, 0.1, [p for _, p in files]))
        out += reasoning_tokens
        inp = self.ctx + self.fresh
        self._item({'response_id': rid('resp_', 32), 'turn_id': self.turn, 'usage': {
            'input_tokens': inp, 'cached_input_tokens': int(self.ctx * rnd.uniform(0.84, 0.96)) // 128 * 128, 'output_tokens': out,
            'reasoning_output_tokens': reasoning_tokens, 'total_tokens': inp + out}}, kind='token_usage_record')
        self.ctx = inp + out
        self.fresh = 0
        for payload, secs, files in outs:
            self.wait(secs)
            self._item(payload)
            self.fresh += tok(payload['output']) + 20
            for p in files or ():
                wrote(p if p.startswith('/') else self.cwd + '/' + p, self.t)
        self.wait(0.4)

    def save(self):
        d = self.start
        name = 'rollout-%s-%s.jsonl' % (d.strftime('%Y-%m-%dT%H-%M-%S'), self.sid)
        dump('%s/%04d/%02d/%02d/%s' % (CODEX, d.year, d.month, d.day, name), self.rows, self.t)


def patch(*parts):
    return '*** Begin Patch\n' + ''.join(parts) + '*** End Patch'


def update(path, *hunks):
    return '*** Update File: %s\n' % path + ''.join(hunks)


def hunk(ctx_before, minus, plus, ctx_after='', anchor=''):
    s = '@@ %s\n' % anchor if anchor else '@@\n'
    for l in ctx_before.split('\n') if ctx_before else []:
        s += ' ' + l + '\n'
    for l in minus.split('\n') if minus else []:
        s += '-' + l + '\n'
    for l in plus.split('\n') if plus else []:
        s += '+' + l + '\n'
    for l in ctx_after.split('\n') if ctx_after else []:
        s += ' ' + l + '\n'
    return s


def add(path, content):
    return '*** Add File: %s\n' % path + ''.join('+' + l + '\n' for l in content.split('\n'))


# ---------------------------------------------------------------- the projects on disk

FILES = {}  # absolute path -> (content, mtime or None for "when it was last written")


def put(project, rel, content, mtime=None):
    FILES[WORK + '/' + project + '/' + rel] = (content, mtime)


def write_files():
    base = NOW - dt.timedelta(days=45)
    for path, (content, mtime) in FILES.items():
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, 'w') as f:
            f.write(content if content.endswith('\n') else content + '\n')
        t = mtime or WRITES.get(path) or base
        os.utime(path, (t.timestamp(), t.timestamp()))

# ================================================================ start clean
for d in (HOME, WORK):
    shutil.rmtree(d, ignore_errors=True)
os.makedirs(CLAUDE, exist_ok=True)
os.makedirs(CODEX, exist_ok=True)

# ================================================================ uploader (Go)
import re as _re


def grep_out(project, files, pattern):
    """Grep's content output: path:line:text for each match, paths relative to the project."""
    rx = _re.compile(pattern)
    out = []
    for rel, content in files:
        for i, line in enumerate(content.split('\n'), 1):
            if rx.search(line):
                out.append('%s:%d:%s' % (rel, i, line))
    return '\n'.join(out)


UP = 'uploader'
UPD = WORK + '/' + UP

CLIENT_HEAD = tabs('''package upload

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "time"
)

// Client puts objects into the bucket, retrying what the server says to retry.
type Client struct {
    http        *http.Client
    endpoint    string
    bucket      string
    maxAttempts int
    backoff     Backoff
}
''')
CLIENT_LOOP_OLD = tabs('''    for attempt := 0; ; attempt++ {''')
CLIENT_LOOP_NEW = tabs('''    for attempt := 0; attempt < c.maxAttempts; attempt++ {''')
CLIENT_BODY = tabs('''        if attempt > 0 {
            if err := c.wait(ctx, attempt); err != nil {
                return err
            }
            if _, err := body.Seek(0, io.SeekStart); err != nil {
                return fmt.Errorf("upload: rewind %s: %w", key, err)
            }
        }
        status, err := c.send(ctx, key, body)
        switch {
        case err == nil && status < 300:
            return nil
        case err != nil:
            last = err
        case !retryable(status):
            return fmt.Errorf("upload: %s: %s", key, http.StatusText(status))
        default:
            last = fmt.Errorf("upload: %s: %s", key, http.StatusText(status))
        }
    }''')
CLIENT_END_OLD = tabs('''    _ = last
    return nil
}''')
CLIENT_END_NEW = tabs('''    return fmt.Errorf("upload: gave up after %d attempts: %w", c.maxAttempts, last)
}''')
CLIENT_TAIL = tabs('''
func retryable(status int) bool {
    return status == http.StatusTooManyRequests || status >= 500
}

// wait sleeps for the backoff before an attempt, or until the context ends.
func (c *Client) wait(ctx context.Context, attempt int) error {
    t := time.NewTimer(c.backoff.Delay(attempt))
    defer t.Stop()
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-t.C:
        return nil
    }
}''')


def client_go(loop, end, doc='// put sends one object, retrying on 429 and 5xx.'):
    return (CLIENT_HEAD + '\n' + doc + '\n' +
            tabs('func (c *Client) put(ctx context.Context, key string, body io.ReadSeeker) error {\n    var last error') + '\n' +
            loop + '\n' + CLIENT_BODY + '\n' + end + '\n' + CLIENT_TAIL)


CLIENT_BEFORE = client_go(CLIENT_LOOP_OLD, CLIENT_END_OLD)
CLIENT_AFTER = client_go(CLIENT_LOOP_NEW, CLIENT_END_NEW)

TEST_BEFORE = tabs('''package upload

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"
)

func testClient(srv *httptest.Server, attempts int) *Client {
    return &Client{http: srv.Client(), endpoint: srv.URL, bucket: "test", maxAttempts: attempts,
        backoff: Backoff{Base: time.Millisecond, Max: time.Millisecond}}
}

func TestPutStopsOnSuccess(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
    defer srv.Close()
    if err := testClient(srv, 3).put(context.Background(), "a/b.bin", strings.NewReader("x")); err != nil {
        t.Fatal(err)
    }
}

func TestPutFailsOn503(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusServiceUnavailable)
    }))
    defer srv.Close()
    ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
    defer cancel()
    if err := testClient(srv, 3).put(ctx, "a/b.bin", strings.NewReader("x")); err == nil {
        t.Fatal("put returned nil for a store that always answers 503")
    }
}''')
TEST_ANCHOR = tabs('''        t.Fatal("put returned nil for a store that always answers 503")
    }
}''')
TEST_NEW = tabs('''
func TestPutGivesUp(t *testing.T) {
    calls := make(chan struct{}, 10)
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        calls <- struct{}{}
        w.WriteHeader(http.StatusServiceUnavailable)
    }))
    defer srv.Close()

    err := testClient(srv, 3).put(context.Background(), "a/b.bin", strings.NewReader("x"))
    if err == nil || !strings.Contains(err.Error(), "gave up after 3 attempts") {
        t.Fatalf("put = %v, want it to give up after 3 attempts", err)
    }
    if n := len(calls); n != 3 {
        t.Fatalf("put made %d attempts, want 3", n)
    }
}''')
TEST_AFTER = TEST_BEFORE + '\n' + TEST_NEW
FAIL_LINE = TEST_BEFORE.split('\n').index('\t\tt.Fatal("put returned nil for a store that always answers 503")') + 1

CONFIG_LINE_OLD = tabs('''        MaxAttempts: defaultMaxAttempts,''')
CONFIG_LINE_NEW = tabs('''        MaxAttempts: maxAttempts(log, os.Getenv("UPLOAD_MAX_ATTEMPTS")),''')
CONFIG_FUNC = tabs('''
// maxAttempts reads UPLOAD_MAX_ATTEMPTS: anything that is not a positive number falls
// back to the default, with a warning.
func maxAttempts(log *slog.Logger, v string) int {
    if v == "" {
        return defaultMaxAttempts
    }
    n, err := strconv.Atoi(v)
    if err != nil || n < 1 {
        log.Warn("UPLOAD_MAX_ATTEMPTS is not a positive number; using the default",
            "value", v, "default", defaultMaxAttempts)
        return defaultMaxAttempts
    }
    return n
}''')


def config_go(line, func=''):
    return tabs('''package upload

import (
    "log/slog"
    "os"
    "strconv"
    "time"
)

const defaultMaxAttempts = 6

// Config is what the uploader reads from its environment.
type Config struct {
    Endpoint    string
    Bucket      string
    MaxAttempts int
    Backoff     Backoff
}

// FromEnv reads the configuration.
func FromEnv(log *slog.Logger) Config {
    return Config{
        Endpoint:    os.Getenv("UPLOAD_ENDPOINT"),
        Bucket:      os.Getenv("UPLOAD_BUCKET"),
''') + line + '\n' + tabs('''        Backoff:     Backoff{Base: 200 * time.Millisecond, Max: 20 * time.Second},
    }
}''') + func


CONFIG_BEFORE = config_go(CONFIG_LINE_OLD)
CONFIG_AFTER = config_go(CONFIG_LINE_NEW, '\n' + CONFIG_FUNC)

BACKOFF_IMPORT_OLD = tabs('import "time"')
BACKOFF_IMPORT_NEW = tabs('''import (
    "math/rand/v2"
    "time"
)''')
BACKOFF_DELAY_OLD = tabs('''// Delay is the wait before the given attempt (1 is the first retry).
func (b Backoff) Delay(attempt int) time.Duration {
    d := b.Base << min(attempt-1, 16)
    if d <= 0 || d > b.Max {
        d = b.Max
    }
    return d
}''')
BACKOFF_DELAY_NEW = tabs('''// Delay is the wait before the given attempt (1 is the first retry): a random
// duration up to the exponential ceiling, so that uploaders failing together do not
// retry together ("full jitter").
func (b Backoff) Delay(attempt int) time.Duration {
    ceiling := b.Base << min(attempt-1, 16)
    if ceiling <= 0 || ceiling > b.Max {
        ceiling = b.Max
    }
    return rand.N(ceiling + 1)
}''')
BACKOFF_TYPE = tabs('''
// Backoff is how long to wait before an attempt: exponential from Base, capped at Max.
type Backoff struct {
    Base time.Duration
    Max  time.Duration
}
''')
BACKOFF_BEFORE = 'package upload\n\n' + BACKOFF_IMPORT_OLD + '\n' + BACKOFF_TYPE + '\n' + BACKOFF_DELAY_OLD
BACKOFF_AFTER = 'package upload\n\n' + BACKOFF_IMPORT_NEW + '\n' + BACKOFF_TYPE + '\n' + BACKOFF_DELAY_NEW

BACKOFF_TEST_OLD = tabs('''func TestDelayDoubles(t *testing.T) {
    b := Backoff{Base: 100 * time.Millisecond, Max: time.Second}
    for attempt, want := range []time.Duration{100, 200, 400, 800, 1000} {
        if got := b.Delay(attempt + 1); got != want*time.Millisecond {
            t.Errorf("attempt %d: %v, want %v", attempt+1, got, want*time.Millisecond)
        }
    }
}''')
BACKOFF_TEST_NEW = tabs('''func TestDelayStaysUnderTheCeiling(t *testing.T) {
    b := Backoff{Base: 100 * time.Millisecond, Max: time.Second}
    for attempt := 1; attempt <= 12; attempt++ {
        ceiling := min(b.Base<<(attempt-1), b.Max)
        for range 1000 {
            if d := b.Delay(attempt); d < 0 || d > ceiling {
                t.Fatalf("attempt %d: delay %v outside [0, %v]", attempt, d, ceiling)
            }
        }
    }
}''')
BACKOFF_TEST_AFTER = tabs('package upload\n\nimport (\n    "testing"\n    "time"\n)\n\n') + BACKOFF_TEST_NEW

ENDPOINT_DIAL_OLD = tabs('''    return d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", e.Host, e.Port))''')
ENDPOINT_DIAL_NEW = tabs('''    return d.DialContext(ctx, "tcp", net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))''')


def endpoint_go(dial, imports):
    return tabs('package s3\n\nimport (\n' + ''.join('    "%s"\n' % i for i in imports) + ''')

// Endpoint is the object store's address.
type Endpoint struct {
    Host string
    Port int
    TLS  bool
}

// URL is the base every request is made against.
func (e Endpoint) URL() string {
    scheme := "http"
    if e.TLS {
        scheme = "https"
    }
    return scheme + "://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// Dial opens a connection to the endpoint, for the health check.
func (e Endpoint) Dial(ctx context.Context) (net.Conn, error) {
    var d net.Dialer
''') + dial + '\n}'


ENDPOINT_BEFORE = endpoint_go(ENDPOINT_DIAL_OLD, ['context', 'fmt', 'net', 'strconv'])
ENDPOINT_AFTER = endpoint_go(ENDPOINT_DIAL_NEW, ['context', 'net', 'strconv'])
VET_LINE = ENDPOINT_BEFORE.split('\n').index(ENDPOINT_DIAL_OLD) + 1

GOMOD_BEFORE = '''module uploader

go 1.24.0

toolchain go1.24.6

require golang.org/x/sync v0.11.0
'''
GOMOD_AFTER_TIDY = '''module uploader

go 1.27.0

require golang.org/x/sync v0.19.0
'''

MULTIPART_NEW = tabs('''// uploadParts sends the parts that have not been sent, each with the client's own
// retry and backoff, and remembers which went through: a part that failed is the only
// one sent again.
func (c *Client) uploadParts(ctx context.Context, u *multipartUpload) error {
    for i := range u.parts {
        p := &u.parts[i]
        if p.etag != "" {
            continue
        }
        etag, err := c.putPart(ctx, u, p)
        if err != nil {
            return fmt.Errorf("upload: part %d of %s: %w", p.number, u.key, err)
        }
        p.etag = etag
    }
    return nil
}''')
MULTIPART_GO = tabs('''package upload

import (
    "context"
    "fmt"
)

type part struct {
    number int
    offset int64
    size   int64
    etag   string
}

type multipartUpload struct {
    key   string
    id    string
    parts []part
}

''') + MULTIPART_NEW

HEALTH_GO = tabs('''package server

import (
    "context"
    "net/http"
    "time"
)

// Pinger is what the health check asks: can the bucket be reached.
type Pinger interface {
    Ping(ctx context.Context) error
}

// Healthz answers 200 when the bucket answers within two seconds, and 503 when it does not.
func Healthz(p Pinger) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
        defer cancel()
        if err := p.Ping(ctx); err != nil {
            http.Error(w, "bucket unreachable: "+err.Error(), http.StatusServiceUnavailable)
            return
        }
        w.Write([]byte("ok\\n"))
    }
}''')

MAIN_ROUTE_NEW = tabs('''    mux.Handle("GET /healthz", server.Healthz(store))''')
MAIN_GO = tabs('''package main

import (
    "log/slog"
    "net/http"
    "os"

    "uploader/internal/s3"
    "uploader/internal/server"
    "uploader/internal/upload"
)

func main() {
    log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
    cfg := upload.FromEnv(log)
    store := s3.New(cfg.Endpoint, cfg.Bucket)
    mux := http.NewServeMux()
    mux.Handle("POST /objects/{key...}", upload.Handler(store, cfg))
''') + MAIN_ROUTE_NEW + tabs('''
    log.Info("listening", "addr", ":8080")
    if err := http.ListenAndServe(":8080", mux); err != nil {
        log.Error("server stopped", "err", err)
        os.Exit(1)
    }
}''')

README_UP_NEW = '''## Running it locally

The uploader talks to anything that speaks the S3 API. For development, start a local
object store in a container and point the uploader at it:

```sh
docker compose up -d objectstore
export UPLOAD_ENDPOINT=http://localhost:9000
export UPLOAD_BUCKET=uploads-dev
go run ./cmd/uploader
```

`UPLOAD_MAX_ATTEMPTS` (default 6) bounds how many times a put is tried.'''
README_UP = '''# uploader

Receives files over HTTP and puts them into object storage, retrying what the store
says to retry.

''' + README_UP_NEW + '''

## Tests

```sh
go test ./...
```
'''

put(UP, 'go.mod', GOMOD_AFTER_TIDY, ago(60 * 19 + 40))   # tidied by hand the evening after the bump
put(UP, 'internal/upload/client.go', CLIENT_AFTER)
put(UP, 'internal/upload/client_test.go', TEST_AFTER)
put(UP, 'internal/upload/config.go', CONFIG_AFTER)
put(UP, 'internal/upload/backoff.go', BACKOFF_AFTER)
put(UP, 'internal/upload/backoff_test.go', BACKOFF_TEST_AFTER)
put(UP, 'internal/upload/multipart.go', MULTIPART_GO)
put(UP, 'internal/s3/endpoint.go', ENDPOINT_AFTER)
put(UP, 'internal/server/health.go', HEALTH_GO)
put(UP, 'cmd/uploader/main.go', MAIN_GO)
put(UP, 'README.md', README_UP)

# ---------------------------------------------------------------- S21: healthz (Oct 22)
s = Claude(UP, 'main', 'Add a /healthz endpoint that checks the bucket')
s.ask('Add a /healthz endpoint that answers 503 when the bucket cannot be reached. The load balancer will use it.', at=on(26, '10:44'))
s.read(UPD + '/cmd/uploader/main.go', MAIN_GO.replace(MAIN_ROUTE_NEW + '\n', ''),
       think='A handler in internal/server that takes something with Ping, so the test does not need a real bucket.')
s.write(UPD + '/internal/server/health.go', HEALTH_GO)
s.edit(UPD + '/cmd/uploader/main.go', tabs('    mux.Handle("POST /objects/{key...}", upload.Handler(store, cfg))'),
       tabs('    mux.Handle("POST /objects/{key...}", upload.Handler(store, cfg))\n') + MAIN_ROUTE_NEW)
s.bash('go build ./... && go test ./internal/server/...', 'ok  \tuploader/internal/server\t0.214s', 'Build and test the server package', dur=6.8)
s.reply('Added `GET /healthz`. It pings the bucket with a two-second timeout: `200 ok` when it answers, `503 bucket unreachable: …` when it does not.\n\nThe handler takes a `Pinger`, so its test uses a stub rather than a bucket.')
s.save()

# ---------------------------------------------------------------- S19: multipart (Oct 28, Codex)
c = Codex(UP, 'main', at=on(20, '14:02'))
c.ask('Make the multipart upload retry only the parts that failed, with the same backoff as single puts. Today one bad part restarts the whole upload.')
c.respond('**Reading the multipart path**\n\nThe upload restarts from part 1 on any error; parts need to remember their ETag.',
          [('exec', "sed -n '1,80p' internal/upload/multipart.go", MULTIPART_GO.replace(MULTIPART_NEW, tabs('''func (c *Client) uploadParts(ctx context.Context, u *multipartUpload) error {
    for i := range u.parts {
        if err := c.putPartOnce(ctx, u, &u.parts[i]); err != nil {
            return c.restart(ctx, u, err)
        }
    }
    return nil
}''')), 0, 0.1)])
c.respond('**Keeping the ETags**', [('patch', patch(update('internal/upload/multipart.go', hunk('', tabs('''func (c *Client) uploadParts(ctx context.Context, u *multipartUpload) error {
    for i := range u.parts {
        if err := c.putPartOnce(ctx, u, &u.parts[i]); err != nil {
            return c.restart(ctx, u, err)
        }
    }
    return nil
}'''), MULTIPART_NEW))), [('M', 'internal/upload/multipart.go')])])
c.respond(None, [('exec', 'go test ./internal/upload/...', 'ok  \tuploader/internal/upload\t1.904s\n', 0, 4.6)])
c.respond(None, say='Done. A part now keeps its ETag once it is through, and `uploadParts` skips parts that have one, so a retry after a failure sends only what failed. Each part goes through `putPart`, which uses the client\'s retry and backoff — the same as a single put.\n\n`go test ./internal/upload/...` passes.')
c.save()

# ---------------------------------------------------------------- S14: README (Nov 9)
s = Claude(UP, 'main', 'Document running the uploader locally')
s.ask('Write a README section on running the uploader locally against a local S3-compatible store. Keep it short.', at=on(8, '16:05'))
s.read(UPD + '/README.md', README_UP.replace(README_UP_NEW + '\n\n', ''))
s.edit(UPD + '/README.md', '## Tests', README_UP_NEW + '\n\n## Tests')
s.reply('Added **Running it locally** above **Tests**: start the object store from `docker compose`, set `UPLOAD_ENDPOINT` and `UPLOAD_BUCKET`, `go run ./cmd/uploader`.')
s.save()

# ---------------------------------------------------------------- S9: the retry loop (Fri 13) — the session the session shots show
s = Claude(UP, 'fix/retry-loop', 'Fix the retry loop in the uploader')
HERO = s
s.ask('The uploader retries forever when S3 returns 503. Find out why and fix it.', at=on(4, '09:58'))
s.grep('retry|attempt', grep_out(UP, [('internal/upload/client.go', CLIENT_BEFORE), ('internal/upload/config.go', CONFIG_BEFORE),
                                       ('internal/upload/backoff.go', BACKOFF_BEFORE)], 'retry|attempt'),
       path='internal/upload', think='Find where the attempts are counted before changing anything.', dur=3.2)
s.wait(6)
s.read(UPD + '/internal/upload/client.go', CLIENT_BEFORE, dur=1.1)
s.wait(14)
s.edit(UPD + '/internal/upload/client.go', CLIENT_LOOP_OLD, CLIENT_LOOP_NEW, dur=5.2)
s.wait(4)
s.tool('Bash', {'command': 'go test ./internal/upload/...'},
       '--- FAIL: TestPutFailsOn503 (0.01s)\n    client_test.go:%d: put returned nil for a store that always answers 503\nFAIL\nFAIL\tuploader/internal/upload\t0.412s\nFAIL' % FAIL_LINE,
       error=True, dur=3.0)
s.wait(11)
s.edit(UPD + '/internal/upload/client.go', CLIENT_END_OLD, CLIENT_END_NEW, dur=8.2)
s.wait(6)
s.edit(UPD + '/internal/upload/client_test.go', TEST_ANCHOR, TEST_ANCHOR + '\n' + TEST_NEW, dur=3.9)
s.wait(3)
s.tool('Bash', {'command': 'go test ./internal/upload/...'}, 'ok  \tuploader/internal/upload\t0.388s', dur=2.0)
s.wait(5)
s.reply('''The loop had no upper bound: `for attempt := 0; ; attempt++` retried until the context ended, and a 503 never ends it.

**What changed**

- `put` now stops after `maxAttempts` (default 6) and returns an error naming how many it made
- a test, `TestPutGivesUp`, pins that

```go
for attempt := 0; attempt < c.maxAttempts; attempt++ {
```

The first run of the tests failed because the function fell through to a leftover `return nil`; it now returns the last error.''')
s.ask('Good. Make the limit configurable from the environment.', after=26 * 60 + 40)
s.edit(UPD + '/internal/upload/config.go', CONFIG_LINE_OLD, CONFIG_LINE_NEW, dur=4.1,
       think='FromEnv already reads the other settings. Parse UPLOAD_MAX_ATTEMPTS there; a bad value should not stop the uploader from starting.')
s.edit(UPD + '/internal/upload/config.go', tabs('''        Backoff:     Backoff{Base: 200 * time.Millisecond, Max: 20 * time.Second},
    }
}'''), tabs('''        Backoff:     Backoff{Base: 200 * time.Millisecond, Max: 20 * time.Second},
    }
}''') + '\n' + CONFIG_FUNC, dur=6.0)
s.bash('go test ./...', 'ok  \tuploader/cmd/uploader\t0.102s\nok  \tuploader/internal/s3\t0.131s\nok  \tuploader/internal/server\t0.208s\nok  \tuploader/internal/upload\t0.402s',
       'Run all tests', dur=7.4)
s.reply('`UPLOAD_MAX_ATTEMPTS` sets it; anything that is not a positive number falls back to 6 with a warning in the log.')
s.save()

# ---------------------------------------------------------------- S6: jitter (Mon 16)
s = Claude(UP, 'main', 'Add jitter to the retry backoff')
s.ask('Add jitter to the retry backoff so a fleet of uploaders does not retry in step after an outage.', at=on(1, '09:41'))
s.read(UPD + '/internal/upload/backoff.go', BACKOFF_BEFORE,
       think='Full jitter: a random wait between zero and the exponential ceiling. It spreads retries best and is the simplest.')
s.edit(UPD + '/internal/upload/backoff.go', BACKOFF_IMPORT_OLD, BACKOFF_IMPORT_NEW)
s.edit(UPD + '/internal/upload/backoff.go', BACKOFF_DELAY_OLD, BACKOFF_DELAY_NEW)
s.edit(UPD + '/internal/upload/backoff_test.go', BACKOFF_TEST_OLD, BACKOFF_TEST_NEW,
       say='The old test asserted exact delays, which no longer holds; it now checks the bounds.')
s.bash('go test ./internal/upload/ -run Delay -count=1 -v', '=== RUN   TestDelayStaysUnderTheCeiling\n--- PASS: TestDelayStaysUnderTheCeiling (0.01s)\nPASS\nok  \tuploader/internal/upload\t0.297s',
       'Run the backoff tests', dur=3.6)
s.reply('`Backoff.Delay` now returns a random duration between zero and the exponential ceiling ("full jitter"), so uploaders that fail together spread their retries out instead of hitting the store again at the same moment.\n\nThe ceiling is unchanged — 200 ms doubling to 20 s — so the worst case is the same as before; the average wait halves. `TestDelayDoubles` asserted exact delays and is replaced by `TestDelayStaysUnderTheCeiling`.')
s.save()

# ---------------------------------------------------------------- S2: toolchain (Mon 16, Codex)
c = Codex(UP, 'main', at=on(1, '16:44'))
c.ask('go.mod says 1.24. Bump the toolchain to 1.27 and fix whatever breaks.')
c.respond('**Checking the module and toolchain**', [('exec', 'head -5 go.mod && go version', GOMOD_BEFORE.split('\n\nrequire')[0] + '\ngo version go1.27.3 linux/amd64\n', 0, 0.2)])
c.respond('**Bumping go.mod**\n\nThe toolchain line can go: the go line is enough once it names 1.27.', [('patch', patch(update('go.mod', hunk('module uploader\n', 'go 1.24.0\n\ntoolchain go1.24.6', 'go 1.27.0'))), [('M', 'go.mod')])])
c.respond(None, [('exec', 'go build ./...', '', 0, 9.8)])
c.respond('**Running vet under the new toolchain**', [('exec', 'go vet ./...', '# uploader/internal/s3\ninternal/s3/endpoint.go:%d:35: address format "%%s:%%d" does not work with IPv6\n' % VET_LINE, 1, 4.1)])
c.respond('**Fixing the dial address**\n\nvet\'s hostport check is new since 1.25. net.JoinHostPort brackets an IPv6 host.',
          [('patch', patch(update('internal/s3/endpoint.go',
                                  hunk('import (\n\t"context"', '\t"fmt"', '', '\t"net"\n\t"strconv"'),
                                  hunk('\tvar d net.Dialer', ENDPOINT_DIAL_OLD, ENDPOINT_DIAL_NEW, '}'))), [('M', 'internal/s3/endpoint.go')])])
c.respond(None, [('exec', 'go vet ./...', '', 0, 3.7)])
c.respond(None, [('exec', 'go test ./...', 'ok  \tuploader/cmd/uploader\t0.098s\nok  \tuploader/internal/s3\t0.127s\nok  \tuploader/internal/server\t0.211s\nok  \tuploader/internal/upload\t0.415s\n', 0, 12.3)])
c.respond(None, say='Bumped to Go 1.27 (`go 1.27.0`; the `toolchain` line is gone, since the go line now names a toolchain that is installed).\n\nOne thing broke: `go vet` now reports `fmt.Sprintf("%s:%d", …)` passed to a dial as an address that does not work with IPv6. `Endpoint.Dial` uses `net.JoinHostPort` now, which brackets an IPv6 host. Build, vet and tests pass.')
c.save()

# ================================================================ billing-api (Go, Postgres)
BI = 'billing-api'
BID = WORK + '/' + BI


def sha():
    return '%07x' % rnd.getrandbits(28)


QUERY_GO = tabs('''package invoices

// listQuery is one account's invoices in the given states, newest first, after the
// cursor when there is one.
const listQuery = `
SELECT id, number, status, total_cents, currency, created_at
FROM invoices
WHERE account_id = $1
  AND status = ANY($2)
  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
ORDER BY created_at DESC, id DESC
LIMIT $5`
''')
QUERY_OLD = tabs('''ORDER BY created_at DESC
LIMIT $3 OFFSET $4`''')
QUERY_BEFORE = QUERY_GO.replace('''  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
ORDER BY created_at DESC, id DESC
LIMIT $5`''', QUERY_OLD)
QUERY_NEW = '''  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
ORDER BY created_at DESC, id DESC
LIMIT $5`'''

HANDLER_PAGE_OLD = tabs('''    page, _ := strconv.Atoi(r.URL.Query().Get("page"))
    rows, err := h.list(r.Context(), account, states, pageSize, max(page-1, 0)*pageSize)''')
HANDLER_PAGE_NEW = tabs('''    after, err := h.position(r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    if r.URL.Query().Has("page") {
        w.Header().Set("Deprecation", "true") // page= goes in 2.5; cursor= replaces it
    }
    rows, err := h.list(r.Context(), account, states, after, pageSize+1)''')
HANDLER_NEXT_OLD = tabs('''    writeJSON(w, listResponse{Invoices: rows})''')
HANDLER_NEXT_NEW = tabs('''    var next *string // null on the last page
    if len(rows) > pageSize {
        c := encodeCursor(rows[pageSize-1])
        next, rows = &c, rows[:pageSize]
    }
    writeJSON(w, listResponse{Invoices: rows, NextCursor: next})''')
HANDLER_GO = tabs('''package invoices

import (
    "net/http"
)

const pageSize = 50

// List answers GET /invoices.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
    account := accountFrom(r.Context())
    states := statesParam(r)
''') + HANDLER_PAGE_NEW + tabs('''
    if err != nil {
        h.log.ErrorContext(r.Context(), "list invoices", "err", err)
        h.fail(w, r, err)
        return
    }
''') + HANDLER_NEXT_NEW + '\n}'

DELIVER_OLD = tabs('''    d.Attempts++
    d.NextAttemptAt = now.Add(time.Minute)
}''')
DELIVER_NEW = tabs('''    d.Attempts++
    if d.Attempts >= maxAttempts {
        d.Status = StatusFailed
        d.NextAttemptAt = time.Time{}
        return
    }
    d.NextAttemptAt = now.Add(retryBackoff(d.Attempts))
}''')
DELIVER_FUNC = tabs('''
// maxAttempts is how many times a delivery is tried before it is marked failed.
const maxAttempts = 8

// retryBackoff is the wait before the next retry: a minute, doubling, at most six hours.
func retryBackoff(attempts int) time.Duration {
    return min(time.Minute<<min(attempts-1, 10), 6*time.Hour)
}''')
DELIVER_HEAD = tabs('''package webhooks

import "time"

// failed records a delivery attempt that did not get a 2xx, and schedules the next one.
func (d *Delivery) failed(now time.Time, status int) {
    d.LastStatus = status
''')
DELIVER_BEFORE = DELIVER_HEAD + DELIVER_OLD
DELIVER_AFTER = DELIVER_HEAD + DELIVER_NEW + '\n' + DELIVER_FUNC
DELIVER_TEST_NEW = tabs('''func TestRetryBackoff(t *testing.T) {
    for attempts, want := range map[int]time.Duration{
        1: time.Minute, 2: 2 * time.Minute, 5: 16 * time.Minute, 9: 4*time.Hour + 16*time.Minute, 10: 6 * time.Hour, 30: 6 * time.Hour,
    } {
        if got := retryBackoff(attempts); got != want {
            t.Errorf("retryBackoff(%d) = %v, want %v", attempts, got, want)
        }
    }
}''')
DELIVER_TEST = tabs('package webhooks\n\nimport (\n    "testing"\n    "time"\n)\n\n') + DELIVER_TEST_NEW

BACKFILL_GO = tabs('''// Command backfill_totals fills invoices.total_cents where the 2.3 migration left it
// null, from the invoice's lines. It is safe to run twice: it only touches nulls.
//
//    go run ./scripts/backfill_totals.go -dry-run
package main

import (
    "context"
    "flag"
    "fmt"
    "log"
    "os"

    "billing-api/internal/db"
    "billing-api/internal/invoices"
)

func main() {
    dry := flag.Bool("dry-run", false, "count what would change, and change nothing")
    batch := flag.Int("batch", 500, "invoices per transaction")
    flag.Parse()

    ctx := context.Background()
    pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()

    n, batches, err := invoices.BackfillTotals(ctx, pool, *batch, *dry)
    if err != nil {
        log.Fatal(err)
    }
    verb := "updated"
    if *dry {
        verb = "would update"
    }
    fmt.Printf("%s %d invoices in %d batches of %d\\n", verb, n, batches, *batch)
}''')

MIGRATION_0042 = '''-- The invoices list filters by account and sorts newest first; without this it scans
-- every invoice of the account and sorts them.
CREATE INDEX CONCURRENTLY IF NOT EXISTS invoices_account_created_at_idx
    ON invoices (account_id, created_at DESC, id DESC);'''

LOGGING_GO = tabs('''package httpx

import (
    "log/slog"
    "net/http"
    "time"
)

// Logging logs every request as one JSON line, with the request id the router set.
func Logging(log *slog.Logger, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        rw := &recorder{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(rw, r)
        log.Info("request", "id", RequestID(r.Context()), "method", r.Method, "path", r.URL.Path,
            "status", rw.status, "ms", time.Since(start).Milliseconds())
    })
}''')

d16 = (TODAY - dt.timedelta(days=1)).isoformat()
CHANGELOG_ENTRY = '''## 2.4.0 — %s

### Added
- `/invoices` paginates by cursor (`cursor`, `next_cursor`); `page` still works for one more release and answers with a `Deprecation` header
- A one-off backfill for invoice totals left null by the 2.3 migration (`scripts/backfill_totals.go`)

### Changed
- Logs are JSON lines through `log/slog`, with the request id on every line
- Webhook deliveries back off exponentially, from a minute to six hours, and are marked failed after 8 attempts

''' % d16
CHANGELOG_OLD = '''## 2.3.2 — 2026-10-21

### Fixed
- Credit notes for a partly refunded invoice no longer double-count tax

## 2.3.1 — 2026-10-14

### Fixed
- `/invoices/{id}/pdf` rounds line totals the way the invoice does'''
CHANGELOG_BEFORE = '# Changelog\n\n' + CHANGELOG_OLD
CHANGELOG_AFTER = '# Changelog\n\n## Unreleased\n\n- An index for the invoices list by account, newest first (migration 0042)\n\n' + CHANGELOG_ENTRY + CHANGELOG_OLD

put(BI, 'internal/invoices/query.go', QUERY_GO)
put(BI, 'internal/invoices/handler.go', HANDLER_GO)
put(BI, 'internal/webhooks/deliver.go', DELIVER_AFTER)
put(BI, 'internal/webhooks/deliver_test.go', DELIVER_TEST)
put(BI, 'internal/httpx/logging.go', LOGGING_GO)
put(BI, 'scripts/backfill_totals.go', BACKFILL_GO)
put(BI, 'scripts/seed_dev.go', 'package main\n\n// Command seed_dev fills a development database with accounts and invoices.\nfunc main() {}')
put(BI, 'migrations/0041_invoice_totals.sql', 'ALTER TABLE invoices ADD COLUMN total_cents bigint;')
put(BI, 'migrations/0042_invoices_account_created_at.sql', MIGRATION_0042)
put(BI, 'CHANGELOG.md', CHANGELOG_AFTER, ago(38))   # someone noted the new index under Unreleased
put(BI, 'go.mod', 'module billing-api\n\ngo 1.27.0')
put(BI, 'scripts/explain/invoices_list.sql', "EXPLAIN ANALYZE\nSELECT id, number, status, total_cents, currency, created_at\nFROM invoices\nWHERE account_id = :account AND status = ANY('{open,paid}')\nORDER BY created_at DESC, id DESC\nLIMIT 51;")

# ---------------------------------------------------------------- S18: cursor pagination (Oct 30)
s = Claude(BI, 'feat/cursor-pagination', 'Cursor pagination for /invoices')
s.ask('Add cursor pagination to GET /invoices. Keep page= working for one more release, with a Deprecation header.', at=on(18, '10:02'))
s.read(BID + '/internal/invoices/query.go', QUERY_BEFORE,
       think='Keyset on (created_at, id): created_at alone is not unique, two invoices made in the same millisecond would be skipped or repeated.')
s.edit(BID + '/internal/invoices/query.go', QUERY_OLD, QUERY_NEW)
s.edit(BID + '/internal/invoices/handler.go', HANDLER_PAGE_OLD, HANDLER_PAGE_NEW)
s.bash('go test ./internal/invoices/...', 'ok  \tbilling-api/internal/invoices\t1.284s', 'Run the invoices tests', dur=8.1)
s.reply('`GET /invoices` takes `cursor=` now and returns `next_cursor`. The cursor is the last row\'s `(created_at, id)`, so two invoices created in the same instant are neither skipped nor repeated.\n\n`page=` still works; a request that uses it gets `Deprecation: true`.')
s.ask('Return next_cursor as null on the last page, not an empty string.', after=5 * 60)
s.edit(BID + '/internal/invoices/handler.go', HANDLER_NEXT_OLD, HANDLER_NEXT_NEW,
       think='Ask for one row more than the page: if it comes back there is a next page. Then the cursor is a pointer, nil on the last page.')
s.bash('go test ./internal/invoices/...', 'ok  \tbilling-api/internal/invoices\t1.301s', 'Run the invoices tests', dur=7.6)
s.reply('Done: the handler asks for one row more than a page, and only when it comes back is there a `next_cursor`; on the last page it is `null`.')
s.save()

# ---------------------------------------------------------------- S16: slog (Nov 5, Codex)
c = Codex(BI, 'main', at=on(12, '15:03'))
c.ask('The service logs with log.Printf. Switch it to log/slog with JSON output, and put the request id on every request line.')
c.respond('**Counting the log calls**', [('exec', 'rg -c "log\\.Printf" --type go | sort -t: -k2 -nr | head', 'internal/invoices/handler.go:7\ninternal/webhooks/worker.go:6\ninternal/db/pool.go:3\ncmd/billing-api/main.go:3\ninternal/httpx/router.go:2\n', 0, 0.1)])
c.respond('**Adding a request logger**', [('patch', patch(add('internal/httpx/logging.go', LOGGING_GO)), [('A', 'internal/httpx/logging.go')])])
c.respond(None, [('exec', 'go build ./...', '# billing-api/internal/invoices\ninternal/invoices/handler.go:57:3: undefined: logger\ninternal/invoices/handler.go:88:3: undefined: logger\n', 1, 6.2)])
c.respond('**Passing the logger to the handlers**\n\nThe handlers used a package-level logger; they get one from the Handler instead.',
          [('patch', patch(update('internal/invoices/handler.go', hunk('', '\t\tlogger.Printf("list invoices: %v", err)', '\t\th.log.ErrorContext(r.Context(), "list invoices", "err", err)'))), [('M', 'internal/invoices/handler.go')])])
c.respond(None, [('exec', 'go build ./... && go test ./...', 'ok  \tbilling-api/internal/db\t0.611s\nok  \tbilling-api/internal/httpx\t0.144s\nok  \tbilling-api/internal/invoices\t1.322s\nok  \tbilling-api/internal/webhooks\t0.508s\n', 0, 18.4)])
c.respond(None, say='Logging goes through `log/slog` with a JSON handler now. `httpx.Logging` writes one line per request with `id`, `method`, `path`, `status` and `ms`; the 21 `log.Printf` calls are `slog` calls with the error as an attribute.\n\nThe first build failed on a package-level `logger` the invoices handlers used; they take the Handler\'s logger now. Build and tests pass.')
c.save()

# ---------------------------------------------------------------- S11: webhook retries (Thu 12)
s = Claude(BI, 'main', 'Cap webhook retries with exponential backoff')
s.ask('Failed webhook deliveries are retried every minute, forever. Cap the attempts and back off exponentially.', at=on(5, '10:04'))


WORKER_GO = tabs("""package webhooks

// due is the deliveries whose next attempt has come.
const due = `SELECT id FROM webhook_deliveries WHERE status = 'pending' AND next_attempt_at <= now()`""")
ENQUEUE_GO = tabs("""package webhooks

func newDelivery(event Event) *Delivery {
    return &Delivery{Event: event, Status: StatusPending, NextAttemptAt: time.Now()}
}""")
put(BI, 'internal/webhooks/worker.go', WORKER_GO)
put(BI, 'internal/webhooks/enqueue.go', ENQUEUE_GO)


def survey(a):
    a.grep('NextAttemptAt|next_attempt_at', grep_out(BI, [('internal/webhooks/deliver.go', DELIVER_BEFORE),
                                                          ('internal/webhooks/enqueue.go', ENQUEUE_GO),
                                                          ('internal/webhooks/worker.go', WORKER_GO)], 'NextAttemptAt|next_attempt_at'),
           path=BID + '/internal')
    a.read(BID + '/internal/webhooks/deliver.go', DELIVER_BEFORE)


s.agent('Explore', 'Find where webhook deliveries are scheduled',
        'In ' + BID + ', find every place that sets or reads a webhook delivery\'s next attempt time, and say what each does. Report file:line.',
        [survey],
        'Three places touch the next attempt:\n\n- `internal/webhooks/enqueue.go:4` — a new delivery is due at once (`NextAttemptAt: time.Now()`)\n- `internal/webhooks/deliver.go:9` — `failed` sets the next attempt a minute out, every time; nothing counts against a limit, though `Attempts` is incremented\n- `internal/webhooks/worker.go:4` — the worker picks up pending deliveries whose `next_attempt_at` has passed\n\nNo cap anywhere: a delivery stays pending until it succeeds.',
        think='Find every place that schedules a delivery before changing one of them.')
s.read(BID + '/internal/webhooks/deliver.go', DELIVER_BEFORE)
s.edit(BID + '/internal/webhooks/deliver.go', DELIVER_OLD, DELIVER_NEW + '\n' + DELIVER_FUNC,
       think='A minute doubling, capped at six hours: 8 attempts cover about four hours, enough for a receiver that is down for a deploy or an afternoon.')
s.write(BID + '/internal/webhooks/deliver_test.go', DELIVER_TEST)
s.bash('go test ./internal/webhooks/...', 'ok  \tbilling-api/internal/webhooks\t0.493s', 'Run the webhooks tests', dur=5.5)
s.reply('''Each retry now waits twice as long as the one before — exponential backoff from a minute, capped at six hours — and after the 8th attempt the delivery is marked **failed** and the worker leaves it alone.

| attempt | then waits |
|---|---|
| 1 | 1 min |
| 2 | 2 min |
| 5 | 16 min |
| 8 | marked failed |

The worker's query needed no change: it only picks up `pending` deliveries.''')
s.ask('What happens to deliveries that are already past 8 attempts?', after=3 * 60)
s.bash('psql "$DEV_DATABASE_URL" -c "SELECT count(*) FROM webhook_deliveries WHERE status = \'pending\' AND attempts >= 8"',
       ' count \n-------\n    37\n(1 row)', 'Count pending deliveries past the cap', dur=0.4)
s.reply('They are failed on their next attempt: `failed` increments `Attempts`, finds it at or past 8, and marks the delivery failed instead of scheduling it. There are 37 such deliveries in the dev database; each gets one more try first. If you would rather not send those, a one-line `UPDATE … SET status = \'failed\' WHERE attempts >= 8` before the deploy does it.')
s.save()

# ---------------------------------------------------------------- S8: backfill (Sat 14)
s = Claude(BI, 'main', 'Backfill null invoice totals')
s.ask('Write a one-off backfill for invoices whose total_cents is still null after the 2.3 migration. Dry run first.', at=on(3, '16:12'))
s.bash('psql "$DEV_DATABASE_URL" -c "SELECT count(*) FROM invoices WHERE total_cents IS NULL"', ' count \n-------\n  1284\n(1 row)', 'Count invoices with no total', dur=0.5)
s.write(BID + '/scripts/backfill_totals.go', BACKFILL_GO,
        think='Batches in their own transactions, so a failure halfway keeps what was done; only nulls are touched, so it can run twice.')
s.bash('go run ./scripts/backfill_totals.go -dry-run', 'would update 1284 invoices in 3 batches of 500', 'Dry-run the backfill', dur=4.3)
s.reply('`scripts/backfill_totals.go` is in. It sums each invoice\'s lines into `total_cents` where it is null, 500 invoices per transaction, and only touches nulls, so running it twice is harmless.\n\nThe dry run against the dev database: **would update 1284 invoices in 3 batches of 500**. Run it for real with `go run ./scripts/backfill_totals.go`.')
s.save()

# ---------------------------------------------------------------- S4: changelog (Mon 16)
s = Claude(BI, 'main', 'Changelog for 2.4.0')
s.ask('Write the changelog entry for 2.4.0 from the commits since v2.3.2.', at=on(1, '11:21'))
log_lines = '\n'.join('%s %s' % (sha(), m) for m in [
    'webhooks: back off exponentially and stop after 8 attempts',
    'scripts: backfill null invoice totals',
    'invoices: next_cursor is null on the last page',
    'invoices: cursor pagination; page= is deprecated',
    'log through slog, with the request id on every line',
    'deps: bump golang.org/x/sync',
    'ci: cache the module download'])
s.bash('git log --oneline --no-merges v2.3.2..HEAD', log_lines, 'List commits since v2.3.2', dur=0.1)
s.read(BID + '/CHANGELOG.md', CHANGELOG_BEFORE, 1, 8)
s.edit(BID + '/CHANGELOG.md', '## 2.3.2 — 2026-10-21', CHANGELOG_ENTRY + '## 2.3.2 — 2026-10-21',
       think='Leave out the dependency bump and the CI change: nobody reading the changelog can tell the difference.')
s.reply('Added **2.4.0** above 2.3.2:\n\n- *Added*: cursor pagination for `/invoices` (with `page` deprecated), the totals backfill\n- *Changed*: JSON logs through slog, webhook backoff and the 8-attempt cap\n\nThe `x/sync` bump and the CI cache are left out, as changes no user of the API would notice.')
s.save()

# ---------------------------------------------------------------- S1: the index (today, Codex)
c = Codex(BI, 'main', at=ago(53))
c.ask('The invoices list endpoint takes 4 seconds for large accounts. Look at the query and add an index if it needs one.')
c.respond('**Finding the list query**', [('exec', 'rg -n "FROM invoices" internal/', 'internal/invoices/query.go:7:FROM invoices\ninternal/invoices/totals.go:19:FROM invoices i\n', 0, 0.1)])
c.respond(None, [('exec', "sed -n '1,20p' internal/invoices/query.go", QUERY_GO, 0, 0.1)])
plan_before = '''Limit  (cost=48211.37..48211.50 rows=51 width=64) (actual time=3907.112..3907.131 rows=51 loops=1)
  ->  Sort  (cost=48211.37..48390.97 rows=71840 width=64) (actual time=3907.109..3907.119 rows=51 loops=1)
        Sort Key: created_at DESC, id DESC
        Sort Method: top-N heapsort  Memory: 39kB
        ->  Seq Scan on invoices  (cost=0.00..45811.20 rows=71840 width=64) (actual time=0.041..3881.502 rows=71840 loops=1)
              Filter: ((account_id = 4127) AND (status = ANY ('{open,paid}'::text[])))
              Rows Removed by Filter: 1688310
Planning Time: 0.214 ms
Execution Time: 3907.198 ms
'''
c.respond('**Measuring the query for the largest dev account**', [('exec', 'psql "$DEV_DATABASE_URL" -v account=4127 -f scripts/explain/invoices_list.sql', plan_before, 0, 4.2)])
c.respond(None, [('exec', 'ls migrations | tail -3', '0039_credit_notes.sql\n0040_webhook_attempts.sql\n0041_invoice_totals.sql\n', 0, 0.1)])
c.respond('**Adding the index**\n\nSeq scan over 1.7M rows, then a sort. An index on (account_id, created_at DESC, id DESC) serves the filter and the order, and the cursor\'s row comparison.',
          [('patch', patch(add('migrations/0042_invoices_account_created_at.sql', MIGRATION_0042)), [('A', 'migrations/0042_invoices_account_created_at.sql')])])
plan_after = '''Limit  (cost=0.56..61.84 rows=51 width=64) (actual time=0.038..0.871 rows=51 loops=1)
  ->  Index Scan using invoices_account_created_at_idx on invoices  (cost=0.56..86305.10 rows=71840 width=64) (actual time=0.036..0.862 rows=51 loops=1)
        Index Cond: (account_id = 4127)
        Filter: (status = ANY ('{open,paid}'::text[]))
        Rows Removed by Filter: 3
Planning Time: 0.302 ms
Execution Time: 0.912 ms
'''
c.respond(None, [('exec', 'make migrate-dev && psql "$DEV_DATABASE_URL" -v account=4127 -f scripts/explain/invoices_list.sql', 'migrate: applied 0042_invoices_account_created_at.sql (11.8s)\n' + plan_after, 0, 13.6)])
c.respond(None, [('exec', 'go test ./internal/invoices/...', 'ok  \tbilling-api/internal/invoices\t1.297s\n', 0, 7.9)])
c.respond(None, say='It needed one. For the largest dev account the query did a sequential scan of 1.7M invoices and a sort: **3.9 s**.\n\nAdded `migrations/0042_invoices_account_created_at.sql`, an index on `(account_id, created_at DESC, id DESC)`. It serves the account filter, the order and the cursor comparison, so the plan is now an index scan that stops after 51 rows: **0.9 ms**.\n\nThe index is built `CONCURRENTLY`, so applying it does not lock the table; it took 12 s on dev.')
c.save()

# ================================================================ dashboard (TypeScript web app)
DA = 'dashboard'
DAD = WORK + '/' + DA

DAYS_OLD = '''// A day key is the day a moment falls on: YYYY-MM-DD.
export function dayKey(d: Date): string {
  return d.toISOString().slice(0, 10);
}'''
DAYS_EDIT_OLD = '''export function dayKey(d: Date): string {
  return d.toISOString().slice(0, 10);
}'''
DAYS_EDIT_NEW = '''export function dayKey(d: Date): string {
  // The reader's calendar day, not UTC's: toISOString() is a day behind east of UTC before dawn
  return [d.getFullYear(), String(d.getMonth() + 1).padStart(2, '0'), String(d.getDate()).padStart(2, '0')].join('-');
}'''
DAYS_FORMATTED = '''export function dayKey(d: Date): string {
  // The reader's calendar day, not UTC's: toISOString() is a day behind east of UTC before dawn
  return [
    d.getFullYear(),
    String(d.getMonth() + 1).padStart(2, '0'),
    String(d.getDate()).padStart(2, '0'),
  ].join('-');
}'''
DAYS_REST = '''

// The keys of the last n days, oldest first, ending today.
export function lastDays(n: number, today = new Date()): string[] {
  const out: string[] = [];
  for (let i = n - 1; i >= 0; i--) {
    const d = new Date(today);
    d.setDate(d.getDate() - i);
    out.push(dayKey(d));
  }
  return out;
}'''
DAYS_BEFORE = DAYS_OLD + DAYS_REST
DAYS_AFTER = '// A day key is the day a moment falls on: YYYY-MM-DD.\n' + DAYS_FORMATTED + DAYS_REST

DAYS_TEST_ANCHOR = '''  it('counts back across a month', () => {'''
DAYS_TEST_NEW = '''  it('puts an early-morning timestamp on its local day', () => {
    vi.stubEnv('TZ', 'Asia/Tokyo'); // 07:30 there is 22:30 the day before in UTC
    expect(dayKey(new Date(2026, 10, 17, 7, 30))).toBe('2026-11-17');
  });

''' + DAYS_TEST_ANCHOR
DAYS_TEST_BEFORE = '''import { afterEach, describe, expect, it, vi } from 'vitest';
import { dayKey, lastDays } from '../../src/lib/days';

afterEach(() => vi.unstubAllEnvs());

describe('dayKey', () => {
  it('formats as YYYY-MM-DD', () => {
    expect(dayKey(new Date(2026, 0, 5, 12))).toBe('2026-01-05');
  });

''' + DAYS_TEST_ANCHOR + '''
    expect(lastDays(3, new Date(2026, 10, 1, 12))).toEqual(['2026-10-30', '2026-10-31', '2026-11-01']);
  });
});'''
DAYS_TEST_AFTER = DAYS_TEST_BEFORE.replace(DAYS_TEST_ANCHOR, DAYS_TEST_NEW)
DAYS_TEST_FAIL_LINE = DAYS_TEST_AFTER.split('\n').index("    expect(dayKey(new Date(2026, 10, 17, 7, 30))).toBe('2026-11-17');") + 1

CHECK_BUCKETS = '''// Scratch: which bar a usage row lands on, in a few zones. Not for the repo.
import { execFileSync } from 'node:child_process';

const zones = ['Asia/Tokyo', 'America/New_York', 'UTC'];
const at = '2026-11-16T22:30:00Z';
for (const tz of zones) {
  const out = execFileSync('node', ['-e', `import('./src/lib/days.ts').then(m => console.log(m.dayKey(new Date('${at}'))))`], {
    env: { ...process.env, TZ: tz },
  });
  console.log(tz.padEnd(18), at, '→', String(out).trim());
}'''

USAGE_CHART_TSX = '''import { dayKey, lastDays } from '../lib/days';
import type { UsageRow } from '../api/types';

export function bucket(rows: UsageRow[], days = 30) {
  const bars = new Map(lastDays(days).map((d) => [d, 0]));
  for (const row of rows) {
    const key = dayKey(new Date(row.at));
    if (bars.has(key)) bars.set(key, bars.get(key)! + row.tokens);
  }
  return bars;
}

export function UsageChart({ usage }: { usage: UsageRow[] }) {
  return (
    <BarChart
        bars={bucket(usage)}
        aria-label="Usage by day"
    />
  );
}'''

VITEST_OLD = '''    poolOptions: { threads: { singleThread: true } }, // the date tests are flaky in parallel'''
VITEST_CONFIG = '''import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'jsdom',
    include: ['test/**/*.test.{ts,tsx}'],
    setupFiles: ['test/setup.ts'],
  },
});'''
VITEST_BEFORE = VITEST_CONFIG.replace("    setupFiles: ['test/setup.ts'],", "    setupFiles: ['test/setup.ts'],\n" + VITEST_OLD)
SETUP_NEW = '''// A test that fakes the clock and fails before restoring it must not leave the clock
// fake for the tests after it, in the same worker.
afterEach(() => {
  vi.useRealTimers();
});'''
SETUP_TS = '''import '@testing-library/jest-dom/vitest';
import { afterEach, vi } from 'vitest';

''' + SETUP_NEW

CSV_TS = '''// toCSV renders rows as CSV: the given columns, in their order, quoted where a field
// needs it (RFC 4180), with CRLF line ends so spreadsheets open it as they should.
export function toCSV<T>(rows: T[], columns: { key: keyof T; label: string }[]): string {
  const field = (v: unknown) => {
    const s = v == null ? '' : String(v);
    return /[",\\r\\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s;
  };
  const lines = [columns.map((c) => field(c.label)).join(',')];
  for (const row of rows) lines.push(columns.map((c) => field(row[c.key])).join(','));
  return lines.join('\\r\\n') + '\\r\\n';
}'''
CSV_BAD = "    return /[,\\r\\n]/.test(s) ? '\"' + s + '\"' : s;"
CSV_GOOD = "    return /[\",\\r\\n]/.test(s) ? '\"' + s.replaceAll('\"', '\"\"') + '\"' : s;"
CSV_TEST = '''import { describe, expect, it } from 'vitest';
import { toCSV } from '../../src/lib/csv';

describe('toCSV', () => {
  it('quotes fields with commas and doubles quotes', () => {
    const rows = [{ n: 'INV-0042', c: 'Acme, "North"' }];
    expect(toCSV(rows, [{ key: 'n', label: 'Number' }, { key: 'c', label: 'Customer' }])).toBe(
      'Number,Customer\\r\\nINV-0042,"Acme, ""North"""\\r\\n',
    );
  });
});'''
INVOICES_EXPORT_NEW = '''      <ExportButton
        filename={`invoices-${filters.month}-${filters.status}.csv`}
        rows={() => toCSV(rows, visibleColumns)}
      />'''
INVOICES_TSX = '''import { toCSV } from '../lib/csv';
import { ExportButton } from '../components/ExportButton';

export function Invoices() {
  const { rows, filters, visibleColumns } = useInvoices();
  return (
    <Page title="Invoices">
      <Toolbar>
        <Filters value={filters} />
''' + INVOICES_EXPORT_NEW + '''
      </Toolbar>
      <InvoiceTable rows={rows} columns={visibleColumns} />
    </Page>
  );
}'''

CLIENT_TS_NEW = '''// get fetches a JSON resource, retrying a failure that may pass — a network error, a
// 502, 503 or 504 — with exponential backoff: 1 s, 2 s, 4 s, then it gives up and throws.
export async function get<T>(path: string, attempts = 4): Promise<T> {
  for (let attempt = 1; ; attempt++) {
    try {
      const res = await fetch(BASE + path, { credentials: 'include' });
      if (res.ok) return (await res.json()) as T;
      if (![502, 503, 504].includes(res.status) || attempt === attempts) throw new HttpError(res.status, path);
    } catch (e) {
      if (e instanceof HttpError || attempt === attempts) throw e;
    }
    await sleep(1000 * 2 ** (attempt - 1));
  }
}'''
CLIENT_TS_OLD = '''export async function get<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path, { credentials: 'include' });
  if (!res.ok) throw new HttpError(res.status, path);
  return (await res.json()) as T;
}'''
CLIENT_TS = '''import { HttpError } from './errors';

const BASE = '/api';
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

''' + CLIENT_TS_NEW
USAGE_PAGE_NEW = '''  const { data, error, refetch } = useQuery(['usage', range], () => get<Usage>(`/usage?range=${range}`), {
    retry: false, // get() retries with backoff itself; the query must not loop on top of it
  });
  if (error) return <ErrorPanel error={error} onRetry={refetch} />;'''
USAGE_PAGE = '''import { get } from '../api/client';

export function UsagePage({ range }: { range: string }) {
''' + USAGE_PAGE_NEW + '''
  return (
    <section>
      <h1>Usage</h1>
      <UsageChart usage={data} />
    </section>
  );
}'''

PICKER_NEW = '''  // A tap on the month arrows is inside the picker even though the arrow re-renders
  // before the event reaches the document: test the path, not the target.
  const inside = (e: PointerEvent) => e.composedPath().includes(ref.current!);'''
PICKER_TSX = '''import { useEffect, useRef } from 'react';

export function DatePicker({ open, onClose }: Props) {
  const ref = useRef<HTMLDivElement>(null);
''' + PICKER_NEW + '''
  useEffect(() => {
    const away = (e: PointerEvent) => !inside(e) && onClose();
    document.addEventListener('pointerdown', away);
    return () => document.removeEventListener('pointerdown', away);
  }, [onClose]);
  return open ? <div ref={ref} className="picker">{/* … */}</div> : null;
}'''

put(DA, 'src/lib/days.ts', DAYS_AFTER)
put(DA, 'test/lib/days.test.ts', DAYS_TEST_AFTER)
put(DA, 'src/charts/UsageChart.tsx', USAGE_CHART_TSX)
put(DA, 'scripts/gen-icons.ts', "// Renders the SVG icons in src/icons to the sizes the manifest names.\nexport {};")
put(DA, 'vitest.config.ts', VITEST_CONFIG)
put(DA, 'test/setup.ts', SETUP_TS)
put(DA, 'src/lib/csv.ts', CSV_TS)
put(DA, 'test/lib/csv.test.ts', CSV_TEST)
put(DA, 'src/pages/Invoices.tsx', INVOICES_TSX)
put(DA, 'src/api/client.ts', CLIENT_TS)
put(DA, 'src/pages/Usage.tsx', USAGE_PAGE)
put(DA, 'src/components/DatePicker.tsx', PICKER_TSX)
put(DA, '.prettierrc', '{ "singleQuote": true, "printWidth": 100 }')
put(DA, 'package.json', '{\n  "name": "dashboard",\n  "private": true,\n  "type": "module",\n  "scripts": {\n    "dev": "vite",\n    "build": "tsc --noEmit && vite build",\n    "test": "vitest run"\n  }\n}')


# ---------------------------------------------------------------- S20: date picker (Oct 26)
s = Claude(DA, 'main', 'Date picker closes on the month arrows')
s.ask('The date picker closes when you tap the month arrows on a phone. Fix it.', at=on(22, '09:31'))
s.read(DAD + '/src/components/DatePicker.tsx', PICKER_TSX.replace(PICKER_NEW, '  const inside = (e: PointerEvent) => ref.current!.contains(e.target as Node);'),
       think='The arrow re-renders on pointerdown, so by the time the document listener runs, e.target is detached and contains() says no. composedPath() is captured when the event is dispatched.')
s.edit(DAD + '/src/components/DatePicker.tsx', '  const inside = (e: PointerEvent) => ref.current!.contains(e.target as Node);', PICKER_NEW)
s.bash('pnpm vitest run test/components/DatePicker.test.tsx', ' ✓ test/components/DatePicker.test.tsx (6 tests) 188ms\n\n Test Files  1 passed (1)\n      Tests  6 passed (6)', 'Run the date picker tests', dur=3.4)
s.reply('The arrow buttons re-render on `pointerdown`, so when the document\'s listener ran, the event\'s target had already been removed from the picker and `contains()` said the tap was outside. The check uses `event.composedPath()` now, which is fixed when the event is dispatched. Desktop never showed it because a mouse click there lands on the button\'s icon, which is not replaced.')
s.save()

# ---------------------------------------------------------------- S15: API retry (Nov 6)
s = Claude(DA, 'main', 'Retry the usage API with backoff')
s.ask('When the API is down the usage page refetches in a tight loop. Add a retry with backoff, and show an error after the last attempt.', at=on(11, '10:28'))
s.read(DAD + '/src/api/client.ts', CLIENT_TS.replace(CLIENT_TS_NEW, CLIENT_TS_OLD))
s.read(DAD + '/src/pages/Usage.tsx', USAGE_PAGE.replace(USAGE_PAGE_NEW, "  const { data } = useQuery(['usage', range], () => get<Usage>(`/usage?range=${range}`), { retry: true, retryDelay: 0 });"),
       think='retry: true with retryDelay 0 is the loop. Retry in get(), with backoff, for the failures that can pass; the query does not retry on top of it.')
s.edit(DAD + '/src/api/client.ts', CLIENT_TS_OLD, CLIENT_TS_NEW)
s.edit(DAD + '/src/pages/Usage.tsx', "  const { data } = useQuery(['usage', range], () => get<Usage>(`/usage?range=${range}`), { retry: true, retryDelay: 0 });", USAGE_PAGE_NEW)
s.bash('pnpm vitest run test/api', ' ✓ test/api/client.test.ts (9 tests) 41ms\n\n Test Files  1 passed (1)\n      Tests  9 passed (9)', 'Run the API client tests', dur=2.9)
s.reply('`get()` now makes up to four attempts for a network error, 502, 503 or 504, waiting 1 s, 2 s and 4 s between them (exponential backoff), and throws after the last; other errors throw at once. The usage query had `retry: true` with no delay — that was the tight loop — and now leaves retrying to `get()`; when it gives up the page shows the error with a *Try again* button.')
s.save()

# ---------------------------------------------------------------- S13: CSV export (Tue 10)
s = Claude(DA, 'feat/csv-export', 'CSV export for the invoices table')
s.ask('Add a CSV export to the invoices table: the columns that are shown, in the order shown.', at=on(7, '11:02'))
s.read(DAD + '/src/pages/Invoices.tsx', INVOICES_TSX.replace(INVOICES_EXPORT_NEW + '\n', '').replace("import { toCSV } from '../lib/csv';\nimport { ExportButton } from '../components/ExportButton';\n\n", ''))
s.write(DAD + '/src/lib/csv.ts', CSV_TS.replace(CSV_GOOD, CSV_BAD))
s.write(DAD + '/test/lib/csv.test.ts', CSV_TEST)
s.bash('pnpm vitest run test/lib/csv.test.ts', ''' FAIL  test/lib/csv.test.ts > toCSV > quotes fields with commas and doubles quotes
AssertionError: expected 'Number,Customer\\r\\nINV-0042,"Acme, "N…' to be 'Number,Customer\\r\\nINV-0042,"Acme, ""…' // Object.is equality

 Test Files  1 failed (1)
      Tests  1 failed (1)''', 'Run the CSV tests', error=True, dur=2.2)
s.edit(DAD + '/src/lib/csv.ts', CSV_BAD, CSV_GOOD, think='A quote inside a field has to be doubled, and a field with a quote has to be quoted.')
s.bash('pnpm vitest run test/lib/csv.test.ts', ' ✓ test/lib/csv.test.ts (1 test) 3ms\n\n Test Files  1 passed (1)\n      Tests  1 passed (1)', 'Run the CSV tests again', dur=2.0)
s.edit(DAD + '/src/pages/Invoices.tsx', "        <Filters value={filters} />", "        <Filters value={filters} />\n" + INVOICES_EXPORT_NEW.replace('invoices-${filters.month}-${filters.status}.csv', 'invoices.csv'))
s.reply('Added an **Export CSV** button to the invoices toolbar. It writes the visible columns in their order, quoting fields as RFC 4180 says (a `"` inside a field is doubled — the first version missed that, and the test caught it), with CRLF line ends so spreadsheets open it cleanly.')
s.ask('Name the file after the filters, like invoices-2026-11-open.csv.', after=4 * 60)
s.edit(DAD + '/src/pages/Invoices.tsx', INVOICES_EXPORT_NEW.replace('invoices-${filters.month}-${filters.status}.csv', 'invoices.csv'), INVOICES_EXPORT_NEW)
s.reply('Done: `invoices-${month}-${status}.csv`, e.g. `invoices-2026-11-open.csv`.')
s.save()

# ---------------------------------------------------------------- S10: suite time (Fri 13, Codex)
c = Codex(DA, 'main', at=on(4, '15:19'))
c.ask('The test suite takes 6 minutes in CI; it took 90 seconds last month. Find out where the time goes.')
c.respond('**Looking at what changed in the test setup**', [('exec', 'git log --since=2026-10-01 --oneline -- vitest.config.ts test/setup.ts', '4be81d2 test: run in a single thread to stop the flaky date tests\n', 0, 0.1)])
c.respond(None, [('exec', 'cat vitest.config.ts', VITEST_BEFORE + '\n', 0, 0.1)])
c.respond('**Timing the suite as it is**', [('exec', 'pnpm vitest run --reporter=dot', '·' * 64 + '\n\n Test Files  38 passed (38)\n      Tests  209 passed (209)\n   Duration  341.62s (transform 4.21s, setup 12.88s, collect 38.12s, tests 281.40s)\n', 0, 343.0)])
c.respond('**Finding why the date tests were flaky**\n\nsingleThread hides a leak: some tests fake the clock and never restore it.',
          [('exec', 'rg -l "useFakeTimers" test | xargs rg -L "useRealTimers"', 'test/lib/relative-time.test.ts\ntest/charts/UsageChart.test.tsx\ntest/pages/Usage.test.tsx\n', 0, 0.2)])
c.respond('**Restoring the clock after every test**', [('patch', patch(update('vitest.config.ts', hunk("    setupFiles: ['test/setup.ts'],", VITEST_OLD, '', '  },')),
                                                                       update('test/setup.ts', hunk("import { afterEach, vi } from 'vitest';\n", '', SETUP_NEW))),
                                                                 [('M', 'vitest.config.ts'), ('M', 'test/setup.ts')])])
c.respond(None, [('exec', 'pnpm vitest run --reporter=dot', '·' * 64 + '\n\n Test Files  38 passed (38)\n      Tests  209 passed (209)\n   Duration  81.37s (transform 4.02s, setup 11.94s, collect 36.40s, tests 74.88s)\n', 0, 83.0)])
c.respond(None, say='The time goes to `singleThread: true`, added on 2 Oct to stop flaky date tests: the whole suite runs in one worker, 342 s here.\n\nThe flakiness was three test files that fake the clock with `vi.useFakeTimers()` and never restore it, so whichever test ran next in that worker saw a frozen clock. `test/setup.ts` now restores real timers after every test, and the single-thread setting is gone: **81 s**.')
c.ask('Run it three times to be sure the date tests are not flaky any more.', after=2 * 60)
c.respond(None, [('exec', 'for i in 1 2 3; do pnpm vitest run --reporter=dot test/lib test/charts test/pages || exit 1; done',
                  ''.join(' Test Files  21 passed (21)\n      Tests  117 passed (117)\n   Duration  %.2fs\n\n' % d for d in (44.18, 42.91, 43.56)), 0, 131.5)])
c.respond(None, say='Three runs of the date-heavy directories, all 117 tests passing each time.')
c.save()

# ---------------------------------------------------------------- S3: the tooltip (today)
s = Claude(DA, 'fix/usage-tooltip', 'Usage tooltip shows the previous day')
s.ask('The usage chart\'s tooltip shows yesterday\'s numbers on today\'s bar, but only before 8 in the morning. Find out why and fix it.', at=ago(3 * 60 + 52))
s.read_missing(DAD + '/src/lib/date.ts',
               think='Before 8 in the morning here is the day before in UTC. Something keys days with toISOString(); the date helpers are the first place to look.')


def buckets(a):
    a.grep('dayKey|toISOString', grep_out(DA, [('src/charts/UsageChart.tsx', USAGE_CHART_TSX), ('src/lib/days.ts', DAYS_BEFORE)], 'dayKey|toISOString'),
           path=DAD + '/src')
    a.read(DAD + '/src/charts/UsageChart.tsx', USAGE_CHART_TSX)


s.agent('Explore', 'Find where usage is bucketed by day',
        'In ' + DAD + ', find every place that turns a timestamp into a day key or groups usage rows by day. For each, say file:line and whether it computes the UTC day or the local one.',
        [buckets],
        'One function decides the day, and everything goes through it:\n\n- `src/lib/days.ts:3` — `dayKey` is `d.toISOString().slice(0, 10)`: the **UTC** day\n- `src/lib/days.ts:12` — `lastDays` builds the bars from local midnights, then keys them with `dayKey`\n- `src/charts/UsageChart.tsx:7` — each usage row is put on `dayKey(new Date(row.at))`\n\nSo bars and rows both use the UTC day. Before 08:00 in UTC+8, today\'s local midnight keys as yesterday, so today\'s bar carries yesterday\'s total.')
s.read(DAD + '/src/lib/days.ts', DAYS_BEFORE)
s.edit(DAD + '/test/lib/days.test.ts', DAYS_TEST_ANCHOR, DAYS_TEST_NEW,
       think='Pin it with a test first: a moment early in the morning east of UTC, which toISOString() puts on the day before.')
t_fail = s.t
s.bash('pnpm vitest run test/lib/days.test.ts', ''' FAIL  test/lib/days.test.ts > dayKey > puts an early-morning timestamp on its local day
AssertionError: expected '2026-11-16' to be '2026-11-17' // Object.is equality

- Expected
+ Received

- 2026-11-17
+ 2026-11-16

 ❯ test/lib/days.test.ts:%d:56

 Test Files  1 failed (1)
      Tests  1 failed | 2 passed (3)''' % DAYS_TEST_FAIL_LINE, 'Run the day tests', error=True, dur=2.6)
s.edit(DAD + '/src/lib/days.ts', DAYS_EDIT_OLD, DAYS_EDIT_NEW,
       think='Build the key from the local date parts. toLocaleDateString would work too but depends on the locale data the browser ships.')
s.bash('pnpm vitest run test/lib/days.test.ts', ' ✓ test/lib/days.test.ts (3 tests) 5ms\n\n Test Files  1 passed (1)\n      Tests  3 passed (3)', 'Run the day tests again', dur=2.4)
s.write(DAD + '/scripts/check-buckets.ts', CHECK_BUCKETS,
        say='The test pins one zone. A quick script to see the same instant in a few zones before calling it done:')
s.bash('pnpm tsx scripts/check-buckets.ts', 'Asia/Tokyo         2026-11-16T22:30:00Z → 2026-11-17\nAmerica/New_York   2026-11-16T22:30:00Z → 2026-11-16\nUTC                2026-11-16T22:30:00Z → 2026-11-16',
       'Print the day key in three zones', dur=1.9)
s.bash('pnpm prettier --write src/lib/days.ts test/lib/days.test.ts', 'src/lib/days.ts 41ms\ntest/lib/days.test.ts 12ms (unchanged)', 'Format the changed files', dur=1.1)
wrote(DAD + '/src/lib/days.ts', s.t)  # prettier rewrote it, inside the turn
s.reply('''`dayKey` used `toISOString().slice(0, 10)`, which is the **UTC** day. The chart builds its bars from local midnights and keys them with the same function, so before 08:00 here (UTC+8) today's midnight keyed as yesterday — and today's bar showed yesterday's total.

**What changed**

- `dayKey` builds the key from the local date: `getFullYear`, `getMonth`, `getDate`
- a test, *puts an early-morning timestamp on its local day*, runs in Tokyo time and failed before the fix

`scripts/check-buckets.ts` is a scratch script that prints the key in three zones; it is not meant to be committed.''')
s.ask('Delete the scratch script, then run the whole suite and the type check before I push.', after=6 * 60 + 15)
s.bash('rm scripts/check-buckets.ts', '', 'Delete the scratch script', dur=0.1)
s.bash('pnpm vitest run --reporter=dot', '·' * 66 + '\n\n Test Files  38 passed (38)\n      Tests  211 passed (211)\n   Duration  79.84s', 'Run the whole test suite', dur=81.2)
s.bash('pnpm tsc --noEmit', '', 'Type-check the project', dur=14.6)
s.reply('Deleted. All 211 tests pass and the type check is clean.')
s.save()

# ================================================================ infra (Terraform, Kubernetes)
IN = 'infra'
IND = WORK + '/' + IN

DB_VAR_OLD = 'variable "instance_class" {'
DB_VAR_NEW = '''variable "identifier" {
  description = "The instance's name. An instance created before the module named them keeps its own."
  type        = string
  default     = null
}

variable "instance_class" {'''
DB_ID_OLD = '  identifier              = "${var.env}-db"'
DB_ID_NEW = '  identifier              = coalesce(var.identifier, "${var.env}-db")'
DB_MAIN_BEFORE = '''variable "env" {
  type = string
}

variable "instance_class" {
  type    = string
  default = "db.t4g.medium"
}

resource "aws_db_instance" "main" {
''' + DB_ID_OLD + '''
  engine                  = "postgres"
  engine_version          = "16.4"
  instance_class          = var.instance_class
  allocated_storage       = 100
  storage_encrypted       = true
  backup_retention_period = 7
  deletion_protection     = true
  skip_final_snapshot     = false
}'''
# What a reviewer made of it afterwards: the name moved into a local
DB_MAIN_NOW = DB_MAIN_BEFORE.replace(DB_VAR_OLD, DB_VAR_NEW).replace('resource "aws_db_instance" "main" {', '''locals {
  # An instance created before the module named instances keeps the name it has
  identifier = coalesce(var.identifier, "${var.env}-db")
}

resource "aws_db_instance" "main" {''').replace(DB_ID_OLD, '  identifier              = local.identifier')

STG_OLD = '  env            = "staging"'
STG_NEW = '''  env            = "staging"
  identifier     = "staging-postgres" # created before the module named instances'''
STG_MAIN_BEFORE = '''module "db" {
  source         = "../../modules/db"
''' + STG_OLD + '''
  instance_class = "db.t4g.medium"
}

module "uploader" {
  source   = "../../modules/service"
  env      = "staging"
  replicas = 3
}'''
STG_MAIN_NOW = STG_MAIN_BEFORE.replace(STG_OLD, STG_NEW).replace('"db.t4g.medium"', '"db.t4g.large"')

BACKUP_OLD = '''      backoffLimit: 0'''
BACKUP_NEW = '''      backoffLimit: 3            # a DNS hiccup at 02:00 should not cost the night's backup
      activeDeadlineSeconds: 5400 # however many retries, give up after 90 minutes'''
BACKUP_YAML = '''apiVersion: batch/v1
kind: CronJob
metadata:
  name: db-backup
  namespace: data
spec:
  schedule: "0 2 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
''' + BACKUP_NEW + '''
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: backup
              image: registry.internal/tools/pg-backup:1.6
              args: ["--to", "s3://backups-db/nightly/"]'''

VALUES_NEW = '  UPLOAD_BUCKET: uploads-staging'
VALUES_YAML = '''replicaCount: 3

image:
  tag: "2.8.1"

env:
  UPLOAD_ENDPOINT: http://objectstore.staging.svc:9000
''' + VALUES_NEW + '''
  UPLOAD_MAX_ATTEMPTS: "6"'''

RULES_YAML = '''groups:
  - name: uploader
    rules:
      - alert: UploaderErrorRate
        expr: |
          sum by (env) (rate(http_requests_total{service="uploader", code=~"5.."}[5m]))
            / sum by (env) (rate(http_requests_total{service="uploader"}[5m])) > 0.05
        for: 10m
        labels:
          severity: page
        annotations:
          summary: "uploader 5xx above 5% in {{ $labels.env }} for 10 minutes"
      - alert: UploaderErrorRateWarning
        expr: |
          sum by (env) (rate(http_requests_total{service="uploader", code=~"5.."}[5m]))
            / sum by (env) (rate(http_requests_total{service="uploader"}[5m])) > 0.01
        for: 30m
        labels:
          severity: ticket'''

VERSIONS_TF = '''terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.14"
    }
  }
}'''

put(IN, 'modules/db/main.tf', DB_MAIN_NOW, ago(60 + 27))
put(IN, 'envs/staging/main.tf', STG_MAIN_NOW, ago(60 + 26))
put(IN, 'envs/production/main.tf', STG_MAIN_BEFORE.replace('staging', 'production').replace('db.t4g.medium', 'db.r7g.large'))
put(IN, 'envs/staging/versions.tf', VERSIONS_TF)
put(IN, 'envs/production/versions.tf', VERSIONS_TF)
put(IN, 'k8s/cronjobs/db-backup.yaml', BACKUP_YAML)
put(IN, 'charts/uploader/values-staging.yaml', VALUES_YAML)
put(IN, 'monitoring/rules/uploader.yaml', RULES_YAML)

# ---------------------------------------------------------------- S22: pin providers (Oct 20, Codex)
c = Codex(IN, 'main', at=on(28, '15:33'))
c.ask('Pin the provider versions in every environment and update the lock files. Staging and production drifted apart last week.')
c.respond('**Comparing the environments**', [('exec', 'rg -n "version" envs/*/versions.tf', 'envs/staging/versions.tf:2:  required_version = ">= 1.9"\nenvs/production/versions.tf:2:  required_version = ">= 1.9"\n', 0, 0.1)])
c.respond('**Pinning both to the same minor series**', [('patch', patch(*[update('envs/%s/versions.tf' % e, hunk('      source  = "hashicorp/aws"', '', '      version = "~> 6.14"', '    }')) for e in ('staging', 'production')]),
                                                       [('M', 'envs/staging/versions.tf'), ('M', 'envs/production/versions.tf')])])
c.respond(None, [('exec', 'for e in staging production; do (cd envs/$e && terraform init -upgrade -no-color | tail -1); done',
                  'Terraform has been successfully initialized!\nTerraform has been successfully initialized!\n', 0, 21.7)])
c.respond(None, say='Both environments now require the provider `~> 6.14`, and `terraform init -upgrade` rewrote both lock files to 6.14.2. Neither had a version constraint before, which is how staging picked up 6.16 while production stayed on 6.11.')
c.save()

# ---------------------------------------------------------------- S17: alert (Nov 3)
s = Claude(IN, 'main', 'Alert on the uploader 5xx rate')
s.ask('Add an alert for the uploader\'s 5xx rate in staging and production: page when it is over 5% for 10 minutes.', at=on(14, '11:16'))
s.write(IND + '/monitoring/rules/uploader.yaml', RULES_YAML,
        think='Ratio of 5xx to all requests per env. A second, lower threshold as a ticket, so a slow rise is seen before it pages.')
s.bash('promtool check rules monitoring/rules/uploader.yaml', 'Checking monitoring/rules/uploader.yaml\n  SUCCESS: 2 rules found', 'Validate the alert rules', dur=0.7)
s.reply('Added `monitoring/rules/uploader.yaml` with two rules on the 5xx share of the uploader\'s requests, per environment:\n\n- **UploaderErrorRate** pages above 5% for 10 minutes\n- **UploaderErrorRateWarning** opens a ticket above 1% for 30 minutes\n\n`promtool check rules` passes.')
s.save()

# ---------------------------------------------------------------- S12: backup retries (Wed 11)
s = Claude(IN, 'main', 'Make the nightly backup retry')
s.ask('The nightly database backup failed twice this week on a DNS lookup. Make the job retry instead of giving up.', at=on(6, '14:31'))
s.read(IND + '/k8s/cronjobs/db-backup.yaml', BACKUP_YAML.replace(BACKUP_NEW, BACKUP_OLD),
       think='backoffLimit: 0 means one pod and no retry. The Job controller already backs off exponentially between retries.')
s.edit(IND + '/k8s/cronjobs/db-backup.yaml', BACKUP_OLD, BACKUP_NEW)
s.bash('kubectl apply --dry-run=server -f k8s/cronjobs/db-backup.yaml', 'error: You must be logged in to the server (the server has asked for the client to provide credentials)',
       'Validate the CronJob against the cluster', error=True, dur=1.3)
s.bash('kubectl config get-contexts -o name', 'dev\nproduction\nstaging', 'List kubectl contexts', dur=0.2)
s.bash('kubectl apply --context production --dry-run=server -f k8s/cronjobs/db-backup.yaml', 'cronjob.batch/db-backup configured (server dry run)',
       'Validate against the production context', dur=1.6)
s.reply('The Job had `backoffLimit: 0`, so a pod that failed was the end of it. It is `3` now: a failed backup is tried again, with the Job controller\'s own backoff between attempts (10 s, 20 s, 40 s), and `activeDeadlineSeconds: 5400` stops the whole thing after 90 minutes so a retry never overlaps the next night\'s run.\n\nThe server-side dry run passes against the production context; the first try failed because the current context was not logged in.')
s.save()

# ---------------------------------------------------------------- S7: rollout hangs (Sun 15, Codex)
c = Codex(IN, 'main', at=on(2, '11:28'))
c.ask('kubectl rollout status for the uploader in staging never finishes. Why?')
c.respond('**Checking the rollout**', [('exec', 'kubectl rollout status deploy/uploader -n staging --timeout=90s',
                                        'Waiting for deployment "uploader" rollout to finish: 1 out of 3 new replicas have been updated...\nerror: timed out waiting for the condition\n', 1, 90.4)])
c.respond(None, [('exec', 'kubectl get pods -n staging -l app=uploader',
                  'NAME                        READY   STATUS    RESTARTS   AGE\nuploader-6f8b9c7d54-hx2lq   1/1     Running   0          3d\nuploader-6f8b9c7d54-r7wzn   1/1     Running   0          3d\nuploader-6f8b9c7d54-zt5mk   1/1     Running   0          3d\nuploader-85d4f6b9cc-q9vjp   0/1     Running   0          14m\n', 0, 0.6)])
c.respond('**The new pod never gets ready; reading its log**', [('exec', 'kubectl logs -n staging uploader-85d4f6b9cc-q9vjp --tail=5',
                  '{"level":"INFO","msg":"listening","addr":":8080"}\n{"level":"WARN","msg":"healthz","err":"bucket unreachable: bucket name is empty"}\n{"level":"WARN","msg":"healthz","err":"bucket unreachable: bucket name is empty"}\n', 0, 0.5)])
c.respond(None, [('exec', 'git log -3 --oneline -- charts/uploader/', 'b7e04d1 uploader: read the bucket from UPLOAD_BUCKET, not --bucket\n2c91fa6 uploader: values for 2.8\n91d3e5b uploader: readiness probe on /healthz\n', 0, 0.1)])
c.respond('**Adding the variable the new version reads**', [('patch', patch(update('charts/uploader/values-staging.yaml', hunk('  UPLOAD_ENDPOINT: http://objectstore.staging.svc:9000', '', VALUES_NEW, '  UPLOAD_MAX_ATTEMPTS: "6"'))),
                                                            [('M', 'charts/uploader/values-staging.yaml')])])
c.respond(None, say='The rollout is stuck on its first new pod, which runs but never becomes ready. Its readiness probe is `/healthz`, and `/healthz` fails with *bucket name is empty*.\n\n`b7e04d1` made the uploader read its bucket from `UPLOAD_BUCKET` instead of a `--bucket` flag, and the staging values never set it. I added `UPLOAD_BUCKET: uploads-staging` to `charts/uploader/values-staging.yaml`; I did not deploy it. The three old pods are still serving, so nothing is down meanwhile.')
c.save()

# ---------------------------------------------------------------- S5: plan replaces the database (today)
s = Claude(IN, 'main', 'Staging plan wants to replace the database')
s.ask('terraform plan for staging wants to replace the database. Find out why before anyone applies it.', at=ago(2 * 60 + 12))
s.bash('cd envs/staging && terraform plan -no-color', '''╷
│ Error: Backend initialization required, please run "terraform init"
│
│ Reason: Backend configuration block has changed
│
│ The "backend" is the interface that Terraform uses to store state,
│ perform operations, etc. If this message is showing up, it means that the
│ Terraform configuration you're using is using a custom configuration for
│ the Terraform backend.
│
│ Changes to backend configurations require reinitialization. This allows
│ Terraform to set up the new configuration, copy existing state, etc.
╵''', 'Plan staging', error=True, dur=2.7, think='See the plan for myself first.')
s.bash('cd envs/staging && terraform init -reconfigure -no-color', 'Initializing the backend...\n\nSuccessfully configured the backend!\nInitializing modules...\nInitializing provider plugins...\n- Reusing previous version from the dependency lock file\n\nTerraform has been successfully initialized!',
       'Reinitialize the staging backend', dur=9.4)
s.bash('cd envs/staging && terraform plan -no-color', '''module.db.aws_db_instance.main: Refreshing state... [id=staging-postgres]
module.uploader.kubernetes_deployment.main: Refreshing state... [id=staging/uploader]

Terraform used the selected providers to generate the following execution
plan. Resource actions are indicated with the following symbols:
-/+ destroy and then create replacement

Terraform will perform the following actions:

  # module.db.aws_db_instance.main must be replaced
-/+ resource "aws_db_instance" "main" {
      ~ id                       = "staging-postgres" -> (known after apply)
      ~ identifier               = "staging-postgres" -> "staging-db" # forces replacement
        # (38 unchanged attributes hidden)
    }

Plan: 1 to add, 0 to change, 1 to destroy.''', 'Plan staging again', dur=31.8)
s.read(IND + '/modules/db/main.tf', DB_MAIN_BEFORE,
       think='The identifier is forced: the module names the instance "${var.env}-db", and staging\'s instance is called staging-postgres. When did the module start naming it?')
s.bash("git log -3 --format='%h %ad %s' --date=short -- modules/db/", '%s %s db: name instances after their environment\n%s 2026-09-02 db: engine 16.4\n%s 2026-06-18 db: module for the service databases' % (
    sha(), (TODAY - dt.timedelta(days=5)).isoformat(), sha(), sha()), 'See recent changes to the db module', dur=0.1)
s.edit(IND + '/modules/db/main.tf', DB_VAR_OLD, DB_VAR_NEW,
       think='Do not rename the instance: a new identifier means a new instance. Let an environment keep the name it has, and default to the new scheme.')
s.edit(IND + '/modules/db/main.tf', DB_ID_OLD, DB_ID_NEW)
s.edit(IND + '/envs/staging/main.tf', STG_OLD, STG_NEW)
s.bash('cd envs/staging && terraform plan -no-color', 'module.db.aws_db_instance.main: Refreshing state... [id=staging-postgres]\nmodule.uploader.kubernetes_deployment.main: Refreshing state... [id=staging/uploader]\n\nNo changes. Your infrastructure matches the configuration.',
       'Plan staging with the fix', dur=28.5)
s.reply('''Don't apply that plan: it destroys the staging database and creates an empty one.

**Why:** last week's change to `modules/db` names every instance `"${var.env}-db"`. The staging instance was created before that, as `staging-postgres`, and an instance's identifier cannot be changed in place — so the plan replaces it.

**What changed**

- `modules/db` takes an optional `identifier`, and uses `"${var.env}-db"` only when it is not set
- `envs/staging` sets `identifier = "staging-postgres"`

The plan for staging is now *No changes*. Production's instance is already called `production-db`, so it is unaffected.

The first plan failed only because the backend block had changed since this checkout was initialised; `terraform init -reconfigure` fixed that.''')
s.save()

# ================================================================ a few more, older and shorter
# Oct 21: a question about memory, answered without an edit
s = Claude(UP, 'main', 'Memory growth during multipart uploads', base=rnd.randint(52000, 64000))
s.ask('Why does the uploader\'s memory grow to 2 GB during a large multipart upload? It should stream.', at=on(27, '15:12'))
s.bash('go test -run TestLargeMultipart -memprofile /tmp/mem.out ./internal/upload/ && go tool pprof -top -sample_index=alloc_space /tmp/mem.out | head -12',
       '''ok  \tuploader/internal/upload\t6.120s
Showing nodes accounting for 2.01GB, 99.12% of 2.03GB total
      flat  flat%   sum%        cum   cum%
    1.88GB 92.61% 92.61%     1.88GB 92.61%  uploader/internal/upload.(*Client).putPartOnce
    0.10GB  4.93% 97.54%     0.10GB  4.93%  bytes.growSlice
    0.03GB  1.58% 99.12%     0.03GB  1.58%  net/http.(*persistConn).writeLoop''', 'Profile allocations in the large multipart test', dur=11.8,
       think='Profile it rather than guess: allocations by function in the large multipart test.')
s.read(UPD + '/internal/upload/multipart.go', MULTIPART_GO.replace(MULTIPART_NEW, tabs('''func (c *Client) putPartOnce(ctx context.Context, u *multipartUpload, p *part) error {
    buf := make([]byte, p.size) // the whole part, to compute its checksum before sending
    if _, err := u.src.ReadAt(buf, p.offset); err != nil {
        return err
    }
    return c.sendPart(ctx, u, p, buf)
}''')))
s.reply('Each part is read whole into a fresh buffer before it is sent — `make([]byte, p.size)` in `putPartOnce` — to compute its checksum first. With 64 MiB parts and the default concurrency of 32, that is 2 GiB in flight, and since every buffer is new the GC only catches up between parts.\n\nTwo ways out, either enough on its own:\n\n- hash while streaming: an `io.TeeReader` into the hash over an `io.SectionReader`, and send the checksum as a trailer\n- keep the buffers, but reuse them from a `sync.Pool` and cap concurrency at 8: 512 MiB at most\n\nI have not changed anything; say which you prefer.')
s.save()

# Oct 27: how invoice numbers are allocated (Codex, read only)
c = Codex(BI, 'main', at=on(21, '10:37'))
c.ask('Explain how invoice numbers are allocated, and whether two concurrent requests can get the same number.')
c.respond('**Finding where numbers are made**', [('exec', 'rg -n "nextNumber|invoice_number_seq" internal/ migrations/', 'internal/invoices/number.go:14:func nextNumber(ctx context.Context, tx pgx.Tx, account int64) (string, error) {\ninternal/invoices/number.go:16:\t\t`UPDATE invoice_counters SET last = last + 1 WHERE account_id = $1 RETURNING last`, account)\nmigrations/0017_invoice_counters.sql:1:CREATE TABLE invoice_counters (\n', 0, 0.1)])
c.respond(None, [('exec', "sed -n '1,40p' internal/invoices/number.go", tabs('''package invoices

// nextNumber takes the account's next invoice number, inside the transaction that
// creates the invoice: the counter row stays locked until it commits.
func nextNumber(ctx context.Context, tx pgx.Tx, account int64) (string, error) {
    var n int64
    err := tx.QueryRow(ctx,
        `UPDATE invoice_counters SET last = last + 1 WHERE account_id = $1 RETURNING last`, account).Scan(&n)
    if err != nil {
        return "", err
    }
    return fmt.Sprintf("INV-%05d", n), nil
}'''), 0, 0.1)])
c.respond(None, say='Numbers come from a per-account counter row, `invoice_counters`, incremented with `UPDATE … SET last = last + 1 … RETURNING last` inside the transaction that creates the invoice.\n\nTwo concurrent requests cannot get the same number: the `UPDATE` locks the account\'s counter row, so the second waits until the first commits or rolls back, then increments what is there. A rollback undoes the increment as well, so there are no gaps either.\n\nThe cost is that creating invoices is serialised per account; across accounts it is not.')
c.save()

# Nov 4: a rename in the dashboard
s = Claude(DA, 'main', 'Rename Spend to Usage in the UI', base=rnd.randint(50000, 62000))
s.ask('Rename "Spend" to "Usage" everywhere in the UI, but not in the API types.', at=on(13, '16:41'))
s.grep('Spend', 'src/pages/Usage.tsx:21:      <h1>Spend</h1>\nsrc/components/Nav.tsx:12:  { href: \'/usage\', label: \'Spend\' },\nsrc/charts/UsageChart.tsx:58:        aria-label="Spend by day"\nsrc/api/types.ts:31:export interface SpendRow {',
       path=DAD + '/src', think='Labels only: SpendRow in the API types stays.')
s.edit(DAD + '/src/pages/Usage.tsx', '      <h1>Spend</h1>', '      <h1>Usage</h1>')
s.edit(DAD + '/src/components/Nav.tsx', "  { href: '/usage', label: 'Spend' },", "  { href: '/usage', label: 'Usage' },")
s.edit(DAD + '/src/charts/UsageChart.tsx', '        aria-label="Spend by day"', '        aria-label="Usage by day"')
s.bash('pnpm vitest run test/pages test/components test/charts', ' Test Files  14 passed (14)\n      Tests  73 passed (73)', 'Run the UI tests', dur=19.4)
s.reply('Renamed in three places: the page heading, the nav link and the chart\'s accessible name. `SpendRow` in `src/api/types.ts` is left as it is, as asked. Tests pass.')
s.save()
put(DA, 'src/components/Nav.tsx', "export const links = [\n  { href: '/', label: 'Invoices' },\n  { href: '/usage', label: 'Usage' },\n];")

# Today: a turn the model's provider turned away, which ends on that error. Drawn from the
# generator's state and then put back, so that every session above keeps its ids and times
before = rnd.getstate()
s = Claude(UP, 'main', 'TestRetryAfter flakes on CI')
s.ask('TestRetryAfter fails on CI about one run in ten. Find out why.', at=ago(47))
s.bash('go test -run TestRetryAfter -count=50 ./internal/upload/',
       '--- FAIL: TestRetryAfter (1.00s)\n    client_test.go:88: waited 998ms, want at least 1s\nFAIL\nFAIL\tuploader/internal/upload\t9.214s',
       'Run the test fifty times', error=True, dur=9.3, think='Reproduce it first: fifty runs should catch one in ten.')
s.api_error('API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}')
s.save()
rnd.setstate(before)

# ================================================================ the files on disk
write_files()
n_claude = sum(1 for r, _, fs in os.walk(CLAUDE) for f in fs if f.endswith('.jsonl') and '/subagents' not in r)
n_codex = sum(1 for r, _, fs in os.walk(CODEX) for f in fs if f.endswith('.jsonl'))
print('now', NOW.isoformat(), 'claude', n_claude, 'codex', n_codex, 'files', len(FILES))
