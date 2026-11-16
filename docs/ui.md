# The page

```sh
asq serve --open        # http://127.0.0.1:7717
```

The page is the same record as the command line, for reading at length: a list to find a session
in, a timeline to read it by, and three reports. It is a client of the [HTTP API](api.md) and
uses nothing else, so anything it shows can be had from the API too.

![The sessions list](screenshots/sessions.png)

## Sessions

The list is grouped by day, newest first, fifty at a time. A row is the session's title, its last
ask and how it was answered, and the facts worth scanning for: which CLI, how many turns and tool
calls, how many of those failed, subagents, project, branch. A session written in the last two
minutes is marked *live*.

The filters on the left — store, time, project, archived sessions, subagent transcripts — live in
the address, so a filtered view can be bookmarked and Back goes where you expect. The counts
beside each store are what the list shows of it.

## Search

Type in the box (`/` focuses it). Results are **turns**, best match first, each with what was
asked there and the lines the words were found in — a reply, the reasoning, a tool's input or its
output — with the matches marked. The query syntax is the [command line's](cli.md#finding);
`?` shows it. A result opens its session at that turn, on the step that matched.

![Search results](screenshots/search.png)

## A session

`/s/<store>/<id>` — the address is the session's ref, and `#4` on the end is turn 4, `#4.31`
step 31 in it. Any of these can be pasted to someone with the same history.

The head says what the session is: title, ref, project, branch, when, totals, and the command
that reopens it in its own CLI. **Brief** opens the [handoff brief](cli.md#handing-over) with a
copy button; **Export** downloads the session as Markdown, JSON, JSON Lines or a standalone
page. A subagent's transcript links to the session it ran in, and a session lists its subagents.

![A session](screenshots/session.png)

![The handoff brief](screenshots/brief.png)

Below it, the turns: for each, the ask in full, a line of facts (what kinds of tool calls, files
changed, tokens), and the answer. Only this outline is loaded up front — a long session is
megabytes — and a turn's steps are fetched when it is opened.

![A turn's steps](screenshots/session-steps.png)

Opened, a turn is its steps in order. A tool call is one row — the tool, what it was given, how
it ended, how long it took — that opens to its input and output. Long output arrives as its head
and tail; *Show all* fetches the rest. Reasoning and injected context are folded; a compaction or
an interruption is a labelled divider; rows a forked agent copied from its parent are grouped
and dimmed. A call that started a subagent links to it.

How a call ended is said by its mark and, when it is anything but fine, in words: *error*,
*interrupted*, *no result yet* — and **outcome not recorded** when the CLI wrote down no outcome
at all, which is not the same as success and is not shown as one.

While a session is being written the page follows it: new turns appear, open turns refresh, and
if you were reading at the end you stay at the end. Scroll up and it leaves you where you are;
the button at the bottom takes you back to the newest turn, or turns following off.

## Reports

**Usage** — tokens over rolling windows, the last thirty days as stacked bars (cache reads,
cache writes, input, output; *Without cache reads* rescales to what is left, since cache reads
are usually most of it), then tables by model, store and project — with how much of the output
was reasoning, where the provider said, and the share of the prompt that came from cache. Every
rounded figure carries its exact value on hover, and the chart can be read as a table. The
reports are in tokens: a session's own cost, where its CLI recorded one, is on the session's
page and [is not summed](cli.md#measuring).

![Usage](screenshots/usage.png)

**Failures** — the tool calls that failed in the last day, week or month, newest first: the
command, how it ended, the end of what it printed, and a link to the step. The chips narrow to
one tool.

![Failures](screenshots/failures.png)

**Digest** — the same window per project: sessions, turns, tool calls and failures, tokens, files
changed by kind, which models and CLIs did the work.

![Digest](screenshots/digest.png)

## Keys

| | |
|---|---|
| `/` | search |
| `j` `k` | next, previous — a row in a list, a turn in a session |
| `Enter` | open |
| `o` | show or hide a turn's steps |
| `e` | expand every step of the opened turns |
| `g g` · `G` | top · bottom |
| `Esc` | back to the list |
| `?` | search syntax and keys |

## Dark, small, signed in

The page follows the system's colour scheme, and the button in the bar overrides it. It is laid
out for a phone as well as a desk.

<p>
<img src="screenshots/sessions-dark.png" width="62%" alt="The list, dark">
<img src="screenshots/phone-session.png" width="27%" alt="A session on a phone">
</p>

When the server was started with a token the page asks for it once and nothing else is shown
until it has it. The token is exchanged for a cookie that script cannot read, and is not kept by
the page. See [who may ask](deploy.md#who-may-ask).

If the index is still reading a large history, the bar says how far it is and what is on screen
covers what has been read. If a store could not be read, a notice says which and why: an empty
list is never the only sign that something broke.

## How it is built

By hand, and that is a decision rather than an economy.

- **No build step, no dependencies.** HTML, CSS and ES modules, embedded in the binary as they
  are: `web/` is what the browser gets. There is nothing to install, audit or keep up to date,
  and `asq serve --page-dir web` serves the directory live for working on it.
- **Transcript text never becomes markup.** A transcript is other people's text — model output,
  tool output, whatever a web page said to an agent. Every string from the API reaches the page
  as a text node or an attribute value, through one element-building function that refuses the
  alternatives. The Markdown rendering of replies is a small subset (code, emphasis, lists,
  links to `http` and `https` only) built as nodes, not parsed into HTML.
- **A policy that enforces it.** The page is served with a Content-Security-Policy that allows
  scripts and styles from its own files only — no inline script, no inline style, no other
  origin, no framing. If the rule above were ever broken, the injected text still could not run.
- **Tested for what a bundler would have caught.** `web/web_test.go` checks that every module a
  module imports exists, that everything the page points at is embedded, that no script uses a
  sink that turns text into markup, and that the HTML needs nothing inline.

The stream that keeps it live carries no content — only which sessions changed — and the page
asks for what it is showing. So a missed event costs a stale row until the next one, never a
wrong one.
