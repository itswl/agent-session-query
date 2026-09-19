---
name: agent-sessions
description: 'Query the session history of every agent CLI on this machine — Claude Code, Codex, Gemini CLI, Pi, Hermes, OpenClaw, OpenCode — through the read-only `agent-sessions` MCP server, and hand a stretch of that history to another agent. Use when the user asks what they worked on before, which session covered a topic, how something was solved previously, whether they have dealt with a problem already, or wants context migrated to another agent or machine. Trigger phrases include "之前怎么解决的", "上周做了什么", "我是不是弄过这个", "把上下文给另一个 AI".'
metadata:
  version: 1.0.0
---

# Query the agent session history on this machine

Every agent CLI keeps a record of what it did; this server reads them. It is **read-only** —
it never modifies session data — and it covers all seven sources at once, so a question can
span a Claude Code session and a Codex one in the same answer.

## Ground rules

- **Never pull a whole session into context.** A long session runs to tens of thousands of
  messages; `get_messages` returns a window for exactly that reason. Page with `cursor` if
  you must, or take the file (below) and do not read it all.
- **Sessions are evidence, not truth.** The "concluded" text is what an agent said at the
  end of its run, not a verified outcome.
- **Ask before quoting session content back to the user verbatim at length** — sessions
  routinely contain credentials, internal URLs and personal notes. Prefer summarising and
  citing the `sessionId`.

## Which tool answers which question

| The user is asking | Reach for | Why |
|---|---|---|
| "Which session was X in?" | `search_sessions(query=…)` | Hits carry a sessionId, role and snippet, so often no session needs opening |
| "What did I ask about X?" | `search_sessions(query=…, role="user")` | `role=user` drops the answers that merely repeat the question |
| "What was in that one session?" | `search_sessions(pattern=<id>, query=…)` | Scopes the scan to one session instead of all of them |
| "What did we work on last week?" | `list_sessions(since="7d")` | Filter by `source` / `project` too |
| "Have I used agent X on repo Y?" | `list_projects` then `list_sessions(project=…)` | Projects are cwds |
| "How did that end?" | `get_session(pattern=<id>)` | Metadata plus the final result — usually the whole answer, for about a kilobyte |
| "Show me around there" | `get_messages(pattern=<id>, order="desc")` | The latest N is where a session's outcome lives |
| "…right where that match was" | `get_messages(pattern=<id>, at=<hit timestamp>)` | **Lands on the hit.** Paging towards it is the mistake this exists to prevent |

### Landing on a search hit

`search_sessions` returns a `timestamp` per match. Pass it as `at` and the window opens
there instead of at an end:

```
search_sessions(query="502", role="user")     → hit in session S at timestamp T
get_messages(pattern=S, at=T, order="desc")   → the messages ending at that hit
```

Without `at`, reaching a hit 8 000 messages into a session means paging through it — on a
large session that is hundreds of requests.

## Reading a whole session

`get_messages` gives a window, not a session, and that is deliberate — the whole thing
would be tens of megabytes of context. When the file itself is what you need (to grep, to
analyse with code, to hand to something else), that is the **HTTP export**, which is not an
MCP tool. Confirm the server is running first (`/health`), then:

```
GET /sessions/<sessionId>/export?format=jsonl   # one JSON object per line, for jq
GET /sessions/<sessionId>/export                # Markdown, for reading
```

Write it to a file and query it there; do not read it into context. The document states how
much of the session it holds — read that line before trusting it.

## Handing work to another agent

`GET /export?project=&since=` assembles **many sessions into one document**, oldest first:
one entry per session, each carrying what was asked and what that session concluded. It is
the right shape for "here is what I have been doing, catch up".

```
GET /export?since=7d                    # a week of work, every source
GET /export?project=/repos/api-gateway  # one project's history
GET /export?mode=full&since=2d          # with the transcripts inline (large)
```

A few kilobytes per session, because it carries the pair rather than the transcripts.

Three things to say to the user when handing one over, because a reader will otherwise get
them wrong:

- **It is a record, not a summary.** Nothing in it says what is *currently* true. Where two
  entries disagree, the later one was said later — that is all it can tell you.
- **The conclusions are the agents' own claims.** Nobody verified them.
- **Do not summarise it away before handing it on.** A summary decides what matters before
  anyone knows what will be asked; the pack exists so that decision can be made later, by
  whoever has the question.

## When not to use this

- The question is about the world, not about this machine's history — general knowledge does
  not live here.
- The user wants you to *do* something. This server reads; it has no write path at all.
