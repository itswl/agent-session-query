# The web page (`/ui`)

Three panes: **session list / message stream / final result**. A token is only requested
when the server was started with `--hook_token`; it stays in the browser's localStorage.

## The panes

### Left — the list

Every session under a readable name (opencode's own title, or the first real user message
elsewhere), grouped by time or project, with a pulsing dot for recent activity (hovering
says how long ago — it means recent writing, not a liveness check) and a message count
filled in by a background scan, so the list request never waits for it. Claude Code
subagents nest under the session that spawned them ("▸ 12 agents"), each opening as a
transcript of its own with a link back, and an `Agent` step in the session leads to the
transcript it started. Sessions a CLI has archived stay out of the list, as they do in the
CLI itself; an **Archived** switch above the list brings them in, marked.

Typing filters sessionId / path / cwd and, in the same query, **searches message bodies**
on the server. Body hits append under an "N more in message bodies" divider, carrying the
matching snippet; with a source picked in the filter, the body search runs within that
source. <kbd>Enter</kbd> skips the wait. A body match picked in the list opens with its
words already in the find bar and the match in view.

### Middle — the conversation, read by rounds

You asked, the agent worked, the agent answered. The work between an ask and its reply
folds to one line (*72 steps · 61 commands · changed 2 files · 1 failed · 24m39s*),
keeping failures in view with the start of their output; a run of reads and searches
folds to "explored — read 4 files, searched twice". Every step opens to its arguments and
output, edits as diffs whichever way the CLI spelled the edit, results with exit code and
duration; what happened between turns (a compaction, an interruption, a hook that failed)
gets a line of its own. Steps are coloured by category (run / read / write / search /
network / delegate).

Five views: **all**, **conversation** (the words alone), and one kind of step alone, round
by round — **changes** (the diffs), **commands** (every command run) and **failures**
(every failed step, opened to its output). Under the toolbar the window's sums — rounds,
tool calls, failures, how long it ran — and the failure count steps through the failures
one at a time (‹ ›, or <kbd>[</kbd> <kbd>]</kbd>), past the loaded window to the nearest
round that failed.

**Find in the session** (the magnifier, or <kbd>f</kbd>) marks the words wherever the
stream draws them and walks the matches with <kbd>Enter</kbd> / <kbd>Shift</kbd>+<kbd>Enter</kbd>,
beyond the loaded window when the match is there; a match inside cut tool output is read
whole before it is shown.

The stream opens on the latest 200 messages and **loads the previous page when you scroll
to the top** (the next one at the bottom, when reading from the earliest); the header's
count says how far into the session you are. Tool output comes cut to a preview, and
"Show full output" fetches the rest. Markdown renders — headings, lists, code, tables,
links — and a message's time shows the clock alone on the session's own day, the date in
front on any other (the full timestamp in the tooltip).

**Copying is where the thing is**: a code block in a reply copies itself, a round's ask
copies that round's brief, and an opened step copies its command, its change as a diff, or
its output — read whole first when the window holds only its start.

### Right — the result and the map

The final result stays visible: stopReason, the answer, the thinking. Then the
**conversation table of contents** — two lines a round, what you asked and what the agent
concluded, with how many steps it took and a red mark when one failed or the round was
interrupted — clicking either jumps into the stream (it covers the loaded window and says
so when the session is longer). Below, the session's facts: the command that reopens it in
its own CLI (copied with the `cd` it needs), the file path, export to Markdown / JSONL /
JSON / a standalone HTML page, usage and cost, and the project's other sessions — with the
project's **pack** (what each of its latest sessions asked and concluded, oldest first) to
copy into another agent or save as a file; a project's header in the list saves it too.

## As an app, on a phone

It installs as a web app: open `/ui` on a phone and "Add to Home Screen" gives it an
icon, a name and a window without browser chrome, honouring the notch and the home
indicator. After a server upgrade an open page reloads itself on its next refresh — an
installed app is never reloaded by hand; the Details screen ends with a card naming the
server's version and how the page was opened. (The icons are drawn by
`go run tools/icongen.go` — standard library only, so the same source produces the same
bytes anywhere.)

On a phone the three panes are screens behind one another, the way an app lays them out:
the list is the home screen, picking a session slides its conversation in, and the
conversation's head carries the way back and the way to the details. The system's back
gesture walks the same way back. Each screen scrolls as a page does, its head fixed at
the top, and keeps its place while another screen is up; the search box and list filters
belong to the list, so a conversation or a details screen opens straight into content.

## The header's services panel

The bar-chart button opens the sources-and-storage panel: which sources are on (a ⚠
beside one that failed to read, with the error below), how many sessions each holds and
what they take on disk — beside what the source keeps anyway — and the age split of the
whole history. It is the difference between "no sessions" and "a source broke", stated
in one place; the data comes from `/health` and `/storage` and is cached for a minute.

## Live follow

A session still being written is followed as it grows: while the open session is active,
a light poll watches it alone, and a new round arrives marked with a soft edge that
eases out over a few seconds, so new work is findable without the stream shouting. It
shares the Auto-refresh switch.

## Behaviour that survives attention

It refreshes every 10 seconds without disturbing what you are reading — expanded blocks,
opened rounds and your place in the text are kept, a rebuild holding the round you were
reading where it was even when something above it changed. Your view controls — grouping,
source filter, order, folded groups — survive a reload. `/ui#<sessionId>` works as a deep
link. Light and dark follow the system; the half-disc button switches, and switching back
hands control to the system again. Each side pane has a bar of its own — its name and the
controls that act on it ("Collapse all" over the project groups) — and a chevron at the
edge folds the pane away to a slim rail, remembered.

## Shortcuts

<kbd>j</kbd> <kbd>k</kbd> move between sessions (metadata matches and body matches alike)
· <kbd>/</kbd> focus search · <kbd>Enter</kbd> search the bodies now · <kbd>g</kbd>
<kbd>G</kbd> jump to the start/end of the stream · <kbd>f</kbd> find in the session ·
<kbd>[</kbd> <kbd>]</kbd> previous/next failure · <kbd>r</kbd> refresh · <kbd>Esc</kbd>
clear the search.
