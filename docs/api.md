# HTTP API

`asq serve` answers on `http://127.0.0.1:7717`. Everything is `GET`, JSON and read-only, except
signing in, keeping a session, and the MCP endpoint. The page is a client of this API and uses nothing else.

- Field names are those of [the record](model.md).
- Times are RFC 3339. `since` and `until` take a span back from now (`90m`, `12h`, `7d`, `2w`),
  a date, or a date and time.
- An error is `{"error": "…"}` with a status that means it: `400` the request does not parse,
  `401` no token, `403` refused by a guard, `404` no such session, `409` a session that cannot be
  kept, `429` too many wrong tokens.
- Replies are `gzip`-compressed when the caller accepts it, and `Cache-Control: no-store`: the
  record is private and changes under the reader.
- In JSON, `<`, `>` and `&` are written `\u003c`, `\u003e` and `\u0026`. A JSON parser reads
  them back as the characters; a reader that took a reply for a page would find no tag in it.
  A reply carries other people's text and quotes what the caller sent, so this holds for all of
  it, beside the content type and `nosniff`.
- Who may call is in [Deploying](deploy.md#who-may-ask). In short: no token, loopback only, and
  nothing a proxy or a tunnel marks with a forwarding header — `403`, saying so (one that adds
  none cannot be told from this machine's own programs); with a token,
  `Authorization: Bearer <token>`. Wrong tokens, callers refused for too many and sign-ins are
  written to the server's log, and counted in [`/api/health`](#status).

## Sessions

**`GET /api/sessions`** — the list, newest first.

| parameter | |
|---|---|
| `text` | title, id or directory contains this |
| `source`, `project`, `branch` | as on the command line, except that `project` is always a part of the directory: `.` here would be the server's directory, not the caller's |
| `since`, `until` | by when the session was last written |
| `state` | where its latest turn stands: `working`, `waiting`, `idle`, or several, comma-separated — `waiting,idle` is the sessions that wait on the person |
| `archived=1`, `agents=1` | include archived sessions; include subagents' transcripts |
| `limit` (50, at most 500), `offset` | paging |

```json
{"sessions": [{"source": "claude", "id": "3f2a…", "ref": "claude/3f2a…", "title": "…",
               "cwd": "/work/api", "branch": "main", "model": "…", "updatedAt": "…",
               "stats": {"turns": 4, "tools": 31, "failures": 2, "passedLater": 1, "calls": 40, "usage": {…}},
               "agents": 2, "lastAsk": "…", "answer": "…", "active": true, "resume": "cd … && claude --resume …",
               "live": {"state": "waiting", "since": "…", "tool": "Bash", "command": "go test ./...", "seq": 57}}],
 "total": 363, "limit": 50, "offset": 0}
```

`lastAsk` and `answer` are cut to a row's worth here; a session read on its own carries them
whole. `stats.passedLater` is how many of its failures the same command passed later, [as failures
judge it](#reports). `resume` reopens the session from this machine; a session read from a store
added by name has none, and `resumeThere` instead: the command for the machine that store came
from. `active` means written within the last two minutes — recency, not a process check.

`live` is [where the latest turn stands](model.md#where-a-turn-stands), as the transcript
says it, for the CLIs that record it ([which](sources.md#where-a-turn-stands)): `working`,
`waiting` on a call — `tool`, `command` or `file`, and `seq` name it — or `idle`, the agent
done and the person to answer; `since` is when it came to be so. It is said while the
transcript was last written recently enough to believe it: ten minutes for `working`, since
an agent at work writes as it goes and one silent that long has most likely stopped; a day
for `waiting` and `idle`, which last until someone comes back to them. Past that it is
absent, and `state` does not find the session.

**`GET /api/sessions/{source}/{id}`** — one session: `{"session", "turns", "agents", "above"}`,
the session with `active` and `live` as in the list.
`turns` is the **outline** — every turn without its items, each saying how many `steps` it
holds, and of its `failures` how many `passedLater`; an ask or an answer longer than a couple of thousand characters is cut here and whole in
the turn itself. `agents` is the subagent sessions directly beneath it, and `above` — for a
subagent's transcript — the transcripts it sits beneath, the session it ran in first. `{id}` may
be any unambiguous start of the id.

**`GET /api/sessions/{source}/{id}/turns?turns=`** — turns *with* their items.
`turns` is `3`, `3-5`, `last` (the default) or `all`. Long text arrives as a preview — the head
and the tail — with the whole size beside it (`item.bytes`, `tool.outBytes`, `tool.inBytes`);
`full=1` sends everything whole, which for a long session is megabytes.

**`GET /api/sessions/{source}/{id}/items/{seq}`** — one step whole:
`{"session", "turn", "item"}`.

**`GET /api/sessions/{source}/{id}/brief?turn=&from=&since=`** — `{"session", "brief"}`, the
[handoff brief](cli.md#handing-over) as Markdown: of the latest turn, of `turn`, of turn `from`
and every turn after it, of every turn after `since`, or with `since=all` of the whole session.
Its failed calls say where the same command later passed, and its changed files how they stand
now, as `failures` and `files` [have them](#reports).

**`GET /api/sessions/{source}/{id}/export?format=md|json|jsonl|html`** — the whole session, as a
download. Always `Content-Disposition: attachment`: an exported page is a transcript's text laid
out as HTML, and it is not served as a page of this origin.

**`POST /api/sessions/{source}/{id}/keep`** with `{"keep": true}` — keeps a copy of the session
and its subagents, made at once, as [`asq keep <ref>`](cli.md#maintaining) does; `{"keep": false}`
stops keeping it one by one. It writes only in asq's state directory and its folder of copies,
never in a CLI's store. `{"session", "keeping"}`: the session as it stands now, and what was
copied (`transcripts`, `bytes`) or, stopped, why the copy stays (`stays`: `all`, every session of
its CLI is kept; `gone`, its CLI deleted it and the copy is all there is). A session asq does not
keep one by one — a subagent's transcript, one from a store added by name, one of a CLI that keeps
its own — is `409`, saying why. A session read on its own says how it is kept: `kept` is `one`,
`all` or `copy` (its CLI deleted it, and this is the copy), and `keepable` that it can be kept.

## Search

**`GET /api/search?q=`** — the turns that match, with the [query syntax](cli.md#finding) of the
command line. Takes the list's filters, `recent=1` (newest first), `per_session` (default 3, `0`
for no bound), `limit` (20, at most 100), `offset`, and `session`: a session's ref, which
confines the search to that session's own turns — archived or not, and without its subagents'
transcripts, which are sessions of their own. A ref that names no session is `404`.

```json
{"hits": [{"ref": "claude/3f2a…#4", "turn": 4, "at": "…", "ask": "…", "answer": "…",
           "tools": 12, "failures": 2, "passedLater": 1, "score": 14.2,
           "session": {"ref": "claude/3f2a…", "title": "…", "cwd": "…"},
           "matches": [{"kind": "tool", "part": "output", "tool": "Bash", "seq": 57, "snippet": "… ECONNREFUSED 127.0.0.1:5432 …"},
                       {"kind": "reply", "seq": 61, "snippet": "… the pool retried twice …", "word": "retried"}]}],
 "total": 29, "sessions": 24}
```

`total` is turns that matched across all pages; `sessions` how many sessions they are in. A
match's `seq` is the step to open (`/items/{seq}`); `kind` is `title` for one in the session's
title, which is no step (its `seq` is 0). `word` is the word the snippet was cut around when it
is another form of the one searched for, for a page to mark.

How the turn ended: `failures` is its calls that failed or were interrupted, `passedLater` how
many of those were followed — in that turn or the three after it — by the same command
succeeding ([as failures judge it](#reports)), and `interrupted` says the person stopped it.

When no turn holds every word, the answer is the turns that hold the most of them, and says so:

```json
{"hits": [{"ref": "claude/8a80…#1", "lacks": ["queue"], …}],
 "total": 2, "sessions": 1,
 "partial": {"words": 3, "held": 2}, "missing": ["queue"]}
```

`partial` is how many words the query asks for and how many of them the closest hits hold; a
hit's `lacks` is the words it does not hold, as typed (`ask:deploy`, `"exit status 1"`);
`missing` the words no turn the filters select holds at all — which may come back with no hits,
when there is nothing to come close with. A search some turn matches whole has neither, and so
has one that turns would match whole but for a word it excludes: it has no hits.

When the filters given kept such a search from matching whole, `outside` says how many turns
would match it without them:

```json
{"hits": [], "total": 0, "sessions": 0, "missing": ["retry"],
 "outside": {"turns": 7, "sessions": 7, "filters": ["since"]}}
```

`filters` names the parameters given — `source`, `project`, `branch`, `text`, `since`, `until`;
`turns` and `sessions` are what dropping them would find, `0` when nothing would; and
`archived: true` says those turns are in archived sessions alone, which take `archived=1` as
well. When nothing would, `held` names the words of `missing` that turns outside the filters
hold: they are there, only not all in one turn. A search given no filter has `outside` only for
a match archived sessions alone hold, and one confined to a `session` never has it.

## Reports

| | |
|---|---|
| `GET /api/usage` | `{"total", "windows", "byModel", "byDay", "bySource", "byProject"}` — the rolling windows are 24h, 7d, 30d, all; `byDay` is the last 30 days — or, when `until` is in the past, the days from `since` to `until`, or the 30 before `until` with no `since`; the rest cover `since` and `until` |
| `GET /api/usage?by=model\|day\|source\|project\|session` | `{"by", "rows", "total"}` |
| `GET /api/failures?tool=&q=&open=1` | `{"failures", "total", "open", "passedLater", "byTool"}` — the last 7 days unless `since` says otherwise. `tool` names one tool exactly (`Bash` is not `bash`), or with a trailing `*` by how its name begins; `q` keeps the calls whose command or the end of whose output holds each of its words, in any case (`"a phrase"` as written, `-word` not there); `open=1` keeps the calls whose command did not pass later. `open` and `passedLater` count the calls that match by whether it did, whichever `open` asks for; `byTool` counts every tool of the window whichever is asked for; when none in the window match, `outside` is how many match outside it |
| `GET /api/failures?by=command` | `{"commands", "total", "open", "passedLater", "byTool"}` — the same calls by the command that failed, the commands with calls still open first, then those that failed most: each `{"command", "count", "open", "sessions", "last"}`, `command` the program and subcommand (`go test`), or for a tool that runs none `tool` instead, and `last` the latest failure, as `failures` has them. `total` counts the commands |
| `GET /api/digest` | `{"projects", "total"}` — likewise; each counts its `failures`, and how many of them `passedLater` |
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

`changes` names the sessions a sync changed — a turn, or where the latest turn stands — with
`"gone": true` for one that was deleted, or says `{"all": true}` when more changed than is worth
naming. The stream carries no content: a client that hears a session changed asks for what it
wants of it. A comment line every 20 seconds keeps proxies from closing a quiet stream.

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
[not chased](model.md#the-index): a very large one, of a CLI whose files are read whole again,
may be most of a minute behind.

## Status

**`GET /api/health`** — the version, the `executable` the server was started from and whether an
upgrade has `replaced` it since (the server goes on with the old one until restarted), the index
file, what it holds (`totals`: `bytes` the file's size, `free` how much of it no longer holds
anything), where the last sync stands, every store (found or not; `sessions` as the list counts
them, and `transcripts` with subagents', archived and empty ones), and `warnings`: the stores
that could not be read, in whole or in part. Without it, "no sessions" reads the same whether
there are none or the store broke. A store that could be read only in part keeps everything
known of it: what a short listing leaves out is not taken for deleted.

It also says what the guards did over the last day, for a caller they let in:

```json
"guard": {"since": "…", "wrongTokens": 12, "refused": 3, "signIns": 1}
```

`wrongTokens` is how many tokens offered, in a header or at sign-in, were not the token;
`refused`, how many requests were turned away whatever they brought — past the limit on wrong
tokens (`429`), or, without a token, bearing a proxy's forwarding header (`403`); `signIns`, how
many traded the token for the cookie. They are counted by the hour, from `since`: the start of
the hour 23 hours before this one, or the server's start if that is later. The server's log has
them too, a line at a time and never a flood ([Deploying](deploy.md#who-may-ask)).

**`GET /healthz`** — `200 ok` while the process serves. No token needed; it says nothing else.

## Signing in

**`GET /api/auth`** — `{"required": true, "authenticated": false}`; always answers.

**`POST /api/login`** with `{"token": "…"}` — sets the `asq_session` cookie (HttpOnly,
SameSite=Strict, 180 days). **`POST /api/logout`** drops it. Programs do not need either: they
send the header.

## MCP

**`POST /mcp`** — the [MCP server](mcp.md) over HTTP: one JSON-RPC message per request, a JSON
reply. Behind the same guards and token as the API.
