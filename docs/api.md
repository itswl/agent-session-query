# HTTP API

`asq serve` answers on `http://127.0.0.1:7717`. Everything is `GET`, JSON and read-only, except
signing in and the MCP endpoint. The page is a client of this API and uses nothing else.

- Field names are those of [the record](model.md).
- Times are RFC 3339. `since` and `until` take a span back from now (`90m`, `12h`, `7d`, `2w`),
  a date, or a date and time.
- An error is `{"error": "…"}` with a status that means it: `400` the request does not parse,
  `401` no token, `403` refused by a guard, `404` no such session, `429` too many wrong tokens.
- Replies are `gzip`-compressed when the caller accepts it, and `Cache-Control: no-store`: the
  record is private and changes under the reader.
- In JSON, `<`, `>` and `&` are written `\u003c`, `\u003e` and `\u0026`. A JSON parser reads
  them back as the characters; a reader that took a reply for a page would find no tag in it.
  A reply carries other people's text and quotes what the caller sent, so this holds for all of
  it, beside the content type and `nosniff`.
- Who may call is in [Deploying](deploy.md#who-may-ask). In short: no token, loopback only; with
  a token, `Authorization: Bearer <token>`.

## Sessions

**`GET /api/sessions`** — the list, newest first.

| parameter | |
|---|---|
| `text` | title, id or directory contains this |
| `source`, `project`, `branch` | as on the command line |
| `since`, `until` | by when the session was last written |
| `archived=1`, `agents=1` | include archived sessions; include subagents' transcripts |
| `limit` (50, at most 500), `offset` | paging |

```json
{"sessions": [{"source": "claude", "id": "3f2a…", "ref": "claude/3f2a…", "title": "…",
               "cwd": "/work/api", "branch": "main", "model": "…", "updatedAt": "…",
               "stats": {"turns": 4, "tools": 31, "failures": 1, "calls": 40, "usage": {…}},
               "agents": 2, "lastAsk": "…", "answer": "…", "active": true, "resume": "cd … && claude --resume …"}],
 "total": 363, "limit": 50, "offset": 0}
```

`lastAsk` and `answer` are cut to a row's worth here; a session read on its own carries them
whole. `active` means written within the last two minutes — recency, not a process check.

**`GET /api/sessions/{source}/{id}`** — one session: `{"session", "turns", "agents", "above"}`.
`turns` is the **outline** — every turn without its items, each saying how many `steps` it
holds; an ask or an answer longer than a couple of thousand characters is cut here and whole in
the turn itself. `agents` is the subagent sessions directly beneath it, and `above` — for a
subagent's transcript — the transcripts it sits beneath, the session it ran in first. `{id}` may
be any unambiguous start of the id.

**`GET /api/sessions/{source}/{id}/turns?turns=`** — turns *with* their items.
`turns` is `3`, `3-5`, `last` (the default) or `all`. Long text arrives as a preview — the head
and the tail — with the whole size beside it (`item.bytes`, `tool.outBytes`, `tool.inBytes`);
`full=1` sends everything whole, which for a long session is megabytes.

**`GET /api/sessions/{source}/{id}/items/{seq}`** — one step whole:
`{"session", "turn", "item"}`.

**`GET /api/sessions/{source}/{id}/brief?turn=&since=`** — `{"session", "brief"}`, the
[handoff brief](cli.md#handing-over) as Markdown.

**`GET /api/sessions/{source}/{id}/export?format=md|json|jsonl|html`** — the whole session, as a
download. Always `Content-Disposition: attachment`: an exported page is a transcript's text laid
out as HTML, and it is not served as a page of this origin.

## Search

**`GET /api/search?q=`** — the turns that match, with the [query syntax](cli.md#finding) of the
command line. Takes the list's filters, `recent=1` (newest first), `per_session` (default 3, `0`
for no bound), `limit` (20, at most 100), `offset`, and `session`: a session's ref, which
confines the search to that session's own turns — archived or not, and without its subagents'
transcripts, which are sessions of their own. A ref that names no session is `404`.

```json
{"hits": [{"ref": "claude/3f2a…#4", "turn": 4, "at": "…", "ask": "…", "answer": "…",
           "tools": 12, "failures": 1, "score": 14.2,
           "session": {"ref": "claude/3f2a…", "title": "…", "cwd": "…"},
           "matches": [{"kind": "tool", "part": "output", "tool": "Bash", "seq": 57, "snippet": "… ECONNREFUSED 127.0.0.1:5432 …"}]}],
 "total": 29, "sessions": 24}
```

`total` is turns that matched across all pages; `sessions` how many sessions they are in. A
match's `seq` is the step to open (`/items/{seq}`).

## Reports

| | |
|---|---|
| `GET /api/usage` | `{"total", "windows", "byModel", "byDay", "bySource", "byProject"}` — the rolling windows are 24h, 7d, 30d, all; `byDay` is the last 30 days — or, when `until` is in the past, the days from `since` to `until`, or the 30 before `until` with no `since`; the rest cover `since` and `until` |
| `GET /api/usage?by=model\|day\|source\|project\|session` | `{"by", "rows", "total"}` |
| `GET /api/failures?tool=` | `{"failures", "total", "byTool"}` — the last 7 days unless `since` says otherwise. `tool` names one tool exactly (`Bash` is not `bash`), or with a trailing `*` by how its name begins; `byTool` counts every tool of the window whichever is asked for |
| `GET /api/digest` | `{"projects", "total"}` — likewise |
| `GET /api/files?path=&changed=1` | `{"files", "total"}` — each file with `now`, how it stands against what that turn left (below); `waiting` instead names the folder a look at it is waiting in, as macOS asks on the Mac whether `asq` may read it |
| `GET /api/projects` | `{"projects"}` |
| `GET /api/pack?turns=1` | `{"pack", "sessions"}` — Markdown |

A failure whose command later succeeded in the same session carries
`"passedLater": {"turn", "seq", "at", "command"}`: the first later run, by the same tool, of the
same program and subcommand, in that turn or the three after it, that succeeded.

- *The same program and subcommand* — `go test`, `git push`, `npm run build`, `uv run pytest` —
  is read from the line as a shell reads it, past what prepares for it (a `cd`, a `source` of an
  environment, variables set in front) and past the programs that only run another (`sudo`,
  `env`, `time`, `timeout`); a shell's `-c` is the script it runs. Code given inline
  (`python -c`, `node -e`) is no command that comes back, and is not matched. The match is loose
  on purpose — an agent rarely runs a failed command again word for word: it narrows the test,
  adds a flag — so `command` is what was run then, and whether it is the same thing passing is
  for the reader to judge.
- *Succeeded* is taken strictly: an exit status of 0, or where none was recorded, an `ok`; with
  nothing after the command on its line that could hide a failure (a pipe without
  `pipefail`, a `;`, an `||`, an `&`); and an answer that is the command's end, not word that it
  runs on in the background. A run asked for beside the failure, in the same response, or begun
  before its answer came back, is not later than it.

The turns between are how it was got past. It is left out when there is none; another session
is not looked in.

A file's `now` is `{"state", "by"}`: `state` is `same`, `changed`, `written` or `deleted`, and
`by` the evidence it rests on. Two things are weighed: whether the text that turn's last write
put in the file — what an edit inserted, the whole of a file it wrote, the lines a patch added —
is all still there, and whether the file's modification time is after the turn ended.

| | |
|---|---|
| `same` / `content` | the text is there, and nothing has written the file since — or the turn wrote it whole and it is that, to the byte |
| `written` / `content` | written since, with the text still in it: what else changed is not known |
| `changed` / `content` | written since, and the text is not all there |
| `same` / `time` | not written since. Where the text is not all there, it changed within the turn — by the turn, or a formatter it ran — and the file is as the turn ended it |
| `written` / `time` | written since, with no text to look for: a turn that only read the file, a write that only removed text, a file over 8 MB. A checkout or a formatter writes a file as well as an edit does |
| `deleted` | gone, from a directory that is still here |

`now` is absent when nothing can be said. Only the machine `asq` runs on is looked at: a file
that is not on it, a session from a labelled store (`--path claude@box2=…`, as a rule another
machine's), and a session whose directory is not here have none.

All take the list's filters. A usage row is
`{"key", "sessions", "calls", "usage": {"input", "output", "cacheRead", "cacheWrite", "reasoning"}, "total", "cacheHit"}`
(a subagent's calls count toward the session it ran in). It carries no cost — a cost is a
session's own, in its `stats.costUsd` where the CLI recorded one, and [is not summed](cli.md#measuring);
`total` beside the rows is the whole they are parts of, which a list cut to its largest groups
does not add up to.

## Live

**`GET /api/events`** — Server-Sent Events.

```
event: hello
data: {"version": "1.0.0", "sync": {"running": false, "at": "…"}}

event: changes
data: {"sessions": [{"source": "claude", "id": "3f2a…"}]}

event: sync
data: {"running": true, "total": 1276, "done": 412, "stores": "5f0c1a9e2b"}
```

`changes` names the sessions a sync wrote (`"gone": true` for one that was deleted), or says
`{"all": true}` when more changed than is worth naming. The stream carries no content: a client
that hears a session changed asks for what it wants of it. A comment line every 20 seconds keeps
proxies from closing a quiet stream.

`sync` speaks of a read worth waiting for: the first one, or a catch-up — eight or more
transcripts still to read, two seconds in — which it then follows to its end, `done` moving
toward `total`. The read that follows a session as it is written is not announced. On a machine
with an agent at work that happens every few seconds, its result is the `changes` event, and a
status that said so each time would never hold still. A quiet stream still says when the stores
were last read (`at`, `tookMs`), about every twenty seconds. `stores` is a few characters that
change when the condition of the stores does — one appeared, one could not be read — which is
when `/api/health` has something new to say.

How fresh the answers are. While someone is looking — an open stream, or a request in the last
minute and a half — the stores are checked every two seconds, and a request is answered at once
from what the index holds: a few seconds behind at most, and never held up for a transcript that
is being read again. A server nobody has asked for a while checks every thirty seconds; the
first request then has the stores read at once and waits for that, four seconds at most. While
a first read of a large history is still running, answers come from what has been read so far
and `sync` says how far that is. A transcript that is being written is itself
[not chased](model.md#the-index): a very large one may be most of a minute behind.

## Status

**`GET /api/health`** — the version, the index file, what it holds, where the last sync stands,
every store (found or not; `sessions` as the list counts them, and `transcripts` with subagents',
archived and empty ones), and `warnings`: the stores that could not be read, in whole or in
part. Without it, "no sessions" reads the same whether there are none or the store broke. A
store that could be read only in part keeps everything known of it: what a short listing leaves
out is not taken for deleted.

**`GET /healthz`** — `200 ok` while the process serves. No token needed; it says nothing else.

## Signing in

**`GET /api/auth`** — `{"required": true, "authenticated": false}`; always answers.

**`POST /api/login`** with `{"token": "…"}` — sets the `asq_session` cookie (HttpOnly,
SameSite=Strict, 180 days). **`POST /api/logout`** drops it. Programs do not need either: they
send the header.

## MCP

**`POST /mcp`** — the [MCP server](mcp.md) over HTTP: one JSON-RPC message per request, a JSON
reply. Behind the same guards and token as the API.
