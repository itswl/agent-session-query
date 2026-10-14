# Roadmap

Where the project stands and what might come next, kept current with every release. An
open item is a candidate, not a promise: each says what it would answer and how worth doing
it looks now, and an item can leave the list as readily as it joins it.

## Shipped

| Release | What it brought |
|---|---|
| v0.31.0 | **Archived sessions, one rule for all**: what Grok, Codex, OpenCode and Hermes put away is read the same way — left out by default as each CLI does, brought in by `include_archived` or the page's *Archived* switch, marked `archived` |
| v0.30.0 | **File history** — which sessions touched a file, in which round, and what was asked there (`/files?path=`, MCP `file_history`, and the page's *Files changed* card); rounds now see every file a call names, Codex's patches included |
| v0.29.1 | Prompts typed while the agent worked appear where they were typed; a harness's closing blocks fold under the words; find in the session opens the fold a match is in |
| v0.29.0 | Claude Code subagents as sessions of their own, nested under the session they ran in and searched with it; a full read returns the output Claude saved beside the transcript |
| v0.28.1 | Claude search keeps to the rows the reader draws; harness blocks out of titles and asked lines; SQLite reads wait out a write |
| v0.28.0 | Search narrowed by source, project and branch before the scan; MCP `get_session_pack`; find in the session and failure navigation in the page |

## Open

Roughly in the order they look worth doing.

| Item | What it would answer | How it looks |
|---|---|---|
| Remote sources | One view over several machines: another instance's sessions read through its HTTP API, labelled like a `--path` instance | Worth doing. The API already has everything a remote source needs; the work is a source that pages through it, and the failure modes of a network in the middle |
| `--path` for the database sources | Hermes, OpenClaw and OpenCode data on a mounted disk, a backup or a container volume | Worth doing when someone has such a copy to read; mechanical |
| Pack and copy buttons in the page | The pack of a project without writing its URL; copying a round's brief, a step's command or patch | Likely. Small, and the server side exists |
| Live follow | A running session followed as it grows, with a mark where new content starts | Maybe. The page already refreshes every ten seconds; shorter polling for the open, active session would do most of it |
| Bookmarks and notes | A session or a round marked to come back to | Maybe. Browser storage keeps the server read-only; worth it only if the list is hard to find things in |
| Edits made with shell commands | `sed -i`, a redirect or a script counted as changing a file, not only as naming it | Maybe. A command's effect has to be guessed from its text, and a wrong guess is worse than the honest "named in a command" file history gives now |
| Search inside saved outputs | Words in the large tool outputs Claude Code keeps beside the transcript | Maybe. They are read whole on demand already; searching them means scanning their size again on every query |
| Config file and `--check` | Long multi-path setups out of service units and compose files | Maybe, once setups grow past a few flags |
| Storage report | Disk used by source, project and age, as a read-only report | Maybe. Cheap to compute; the service never deletes anything |
| Two-session compare | Two attempts at the same task side by side: conclusions, failures, files changed, time | Doubtful. Most of what it would line up is already per session in the pack, `/rounds` and file history; it needs a concrete case first |
| `/metrics` | Prometheus scraping: latency, scan volume, queueing, source errors | Doubtful unless the service runs under monitoring; `/health` and `/stats` cover the rest |

## Not planned

- **Sending history to a cloud model** for summaries or tags: it would end local, offline,
  private reading, which is the point of the service.
- **A vector database or embedding model**: it would end the single binary with no cgo that
  builds for every platform, and lexical search over the raw bytes answers in well under a
  second here.
- **A persistent full-text index**: eight CLIs append to, compact, truncate and rotate their
  files concurrently, and an index of them goes stale in ways a scan cannot; the scan is
  fast enough (see [internals](internals.md#full-text-search)).
- **Discovering Aider's history**: Aider keeps Markdown files inside each repository, with no
  common directory to find them in; looking for them means scanning the disk and guessing.
