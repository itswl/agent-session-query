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
for no bound), `limit` (20, at most 100), `offset`.

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
| `GET /api/usage` | `{"total", "windows", "byModel", "byDay", "bySource", "byProject"}` — the rolling windows are 24h, 3d, 7d, 30d, all |
| `GET /api/usage?by=model\|day\|source\|project\|session` | `{"by", "rows", "total"}` |
| `GET /api/failures?tool=` | `{"failures", "total", "byTool"}` — the last 7 days unless `since` says otherwise. `tool` names one tool exactly (`Bash` is not `bash`), or with a trailing `*` by how its name begins; `byTool` counts every tool of the window whichever is asked for |
| `GET /api/digest` | `{"projects", "total"}` — likewise |
| `GET /api/files?path=&changed=1` | `{"files", "total"}` |
| `GET /api/projects` | `{"projects"}` |
| `GET /api/pack?turns=1` | `{"pack", "sessions"}` — Markdown |

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
data: {"running": true, "total": 1276, "done": 412}
```

`changes` names the sessions a sync wrote (`"gone": true` for one that was deleted), or says
`{"all": true}` when more changed than is worth naming. `sync` reports a long read of the stores
as it goes. The stream carries no content: a client that hears a session changed asks for what
it wants of it. A comment line every 20 seconds keeps proxies from closing a quiet stream.

How fresh the answers are: every request first brings the index up to date, waiting at most a
few seconds; while a first read of a large history is still running, answers come from what has
been read so far and `sync` says how far that is. While someone is looking — an open stream, or
a request in the last minute and a half — the stores are checked every two seconds; an idle
server checks every thirty.

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
