# Development

```sh
go build ./... && go test ./...        # Go 1.26+, no cgo, no other toolchain
go test -race ./...                    # what CI runs on Linux and macOS
go run ./cmd/asq doctor
```

There is no build step for the page and nothing to generate: what is in the repository is what
ships.

## Layout

One direction of dependency, top to bottom; nothing below knows what is above it.

```
cmd/asq                 main: hands the arguments to cli
internal/cli            the command line: flags in, text or JSON out
internal/server         the HTTP API, the guards and the token, the live stream; serves web/
internal/mcp            the MCP server: stdio and HTTP, thirteen tools and two prompts
internal/otel           OTLP export: what a turn becomes, the ledger of what was sent
internal/skill          the agent-sessions skill, built in: what asq skill prints
internal/service        asq serve kept running: launchd, systemd, Task Scheduler
web/                    the page: hand-written HTML, CSS and ES modules, embedded as they are
        │
internal/app            the program without a face: the index, the stores, the operations
internal/render         the record as text: lists, turns, briefs, packs, exports
        │
internal/index          the SQLite index: sync, sessions, turns, search, reports
internal/catalog        which CLIs there are, and which stores to read on this machine
        │
internal/sources        the adapter contract (Source, Spec)
internal/sources/<cli>  one adapter per CLI
internal/sources/kit    what adapters share: reading lines, lenient values, SQLite, stamps
internal/transcript     the Writer every adapter feeds: messages in, turns out
internal/command        what a command line runs: its program, and whether its status is that program's
        │
internal/model          the record: Session, Turn, Item, Tool, Usage
internal/txt            cleaning and cutting text
```

The skill is in two places — `internal/skill/SKILL.md`, which the binary carries, and
`.agents/skills/agent-sessions/SKILL.md`, which the agents that read a repository's `.agents/skills`
find here — and a test fails when they differ: edit both.

The three faces — `cli`, `server`, `mcp` — are thin on purpose. Each parses its own kind of
request, calls the same `app` and `index` operations, and renders the answer its own way. A new
operation is written in `index` (or `app`) once and then exposed wherever an asker can reach
it — a person at a terminal, the page, a model — in a few lines each. An operation one of them
leaves out is a gap rather than a design unless the table below says why; an operation added
to one of them brings its row up to date. The page asks only the API, so the two middle cells
go together: what the API has and the page does not is there for a script.

| operation | command line | HTTP API | the page | MCP |
|---|---|---|---|---|
| sessions listed, and searched by title or directory | `ls` | `GET /api/sessions` | the list | `sessions` |
| turns by their words, in every session or in one | `search`, with `--session` | `GET /api/search`, with `session` | the search, and *find in session* | `search`, with `session` |
| a session read: outline, turns, one step | `show` | `…/turns`, `…/items/{seq}` | a session | `read` |
| a brief of one session, a pack of several | `brief`, `pack` | `…/brief`, `GET /api/pack` | the brief of the session open | `brief`, `pack`; the prompts `handoff` and `catchup` |
| the command that resumes a session in its CLI | `resume` | the session's `resume` | shown, to copy | said in a brief |
| usage, failures, digest, files | the commands of those names | the endpoints of those names | views of those names | tools of those names |
| the directories sessions ran in | `projects` | `GET /api/projects` | the project picker | `projects` |
| a session followed as it is written | `show -f` | `GET /api/events` | live | — an agent asks turn by turn: `sessions(state="working")` and `read(turns="last")` are its poll; the server offers no subscriptions, and nothing it offers changes while it runs |
| a session written to a file | `export` | `…/export` | the download links | — a model reads text in a context, which `read` gives; a file is for programs |
| why a line is as it is: the commit that last changed it, and the turns behind it | `why` | `GET /api/why` | — asked of an editor or a shell, not of the history | `why`. Over HTTP both run git where the path says, so both want it whole and under a directory a session ran in |
| a copy kept of a session its CLI deletes | `keep` | `POST …/keep` | the Keep control | `keep`, for one session: the one operation that writes, and only to asq's own state. `--all`, `--off` and `--dir` are a person's, and are not given to an agent |
| a picture pasted into an ask or returned by a tool | — text only | `…/attachments/{n}` | beneath its step | — until #79: an answer is text |
| where asq stands: version, index, each store | `doctor`, `index` | `GET /api/health` | the status bar | `health`, without doctor's reading of a store's format and without the guard counts, which are for whoever runs the server |
| asq itself: served, probed, kept running, traced, taken off | `serve`, `ping`, `service`, `otel`, `uninstall` — and `mcp` and `skill`, which open the last column | — | — | — a person's, about asq rather than about the history |

## How a transcript becomes turns

An adapter does not build turns. It reports *messages* — who spoke, when, and blocks of text,
reasoning, tool calls, tool results and events — to a `transcript.Writer`, and the Writer does
what must be done the same way for every CLI:

- decides where a turn starts (at a person's ask, and nowhere else — `transcript.AskOf` tells an
  ask from what a harness injected);
- pairs each tool result with its call, by id or, failing that, to the latest call still
  waiting;
- folds the rows of one model response into one call, at its final token figures;
- works out which files a call named and whether it changed them, what a call was for, whether
  it failed;
- keeps a fork's inherited rows out of every count.

This is why behaviour is the same across CLIs, and why an adapter is short: it only translates.

## Adding a CLI

One package and one line.

1. **Write `internal/sources/<name>/<name>.go`**, implementing two methods:

   ```go
   // Scan lists the sessions the store holds now, without reading a transcript.
   Scan(ctx context.Context) ([]sources.Ref, error)
   // Read parses one session: it sets what the CLI recorded about it on w.S
   // and adds every message, in order, to w.
   Read(ctx context.Context, ref sources.Ref, w *transcript.Writer) error
   ```

   A `Ref` is a key that stays the same from scan to scan (a path, a row id) and a *stamp* that
   moves whenever the content may have (`kit.Stamp(path)` for a file: size and modification
   time, which every append moves). A session whose stamp has not moved is not read again —
   get the stamp right and the adapter is incremental for free. A file rewritten in place, as
   a database's pages are, can keep both: written twice within one tick of the filesystem's
   clock at the same size. So a database's sessions are stamped from what their rows say, and
   the query that lists them is kept in a `kit.Snapshot`, which asks again whenever the
   database's files move, and every time until they have been still for longer than any tick
   (`kit.Still`).

   In `Read`, set `w.S.ID`, `Cwd`, `Title`, `Model`… as far as the CLI records them, then for
   each row:

   ```go
   w.Add(transcript.Message{Role: transcript.User, At: at, Blocks: []transcript.Block{transcript.TextOf(text)}})
   w.Add(transcript.Message{Role: transcript.Assistant, At: at, Response: responseID, Model: model, Usage: usage,
       Blocks: []transcript.Block{
           transcript.ThinkingOf(reasoning),
           transcript.TextOf(reply),
           transcript.CallOf(callID, "Bash", input),
       }})
   w.Add(transcript.Message{Role: transcript.ToolRole, At: at, Blocks: []transcript.Block{
       transcript.ResultOf(callID, "Bash", output, transcript.Outcome{Status: transcript.StatusOf(failed), Exit: exit}),
   }})
   ```

   A picture or a document a message carried is a block of its own,
   `transcript.AttachmentOf(transcript.Embedded(mime, base64))` (`Referenced(mime, path)` for one
   the transcript only names), and one a tool returned goes on its result:
   `transcript.ResultOf(…).With(file)`. The Writer marks each in the text and keeps its bytes out
   of the record.

   Convert token counts to [the one shape](model.md#usage-one-shape): input without cached
   tokens, output with reasoning. Mark what was not typed by a person (`Injected`), what a fork
   copied (`Inherited`), what was typed mid-turn (`Queued`). For a CLI whose tools take a path
   relative to where they run, give the message that directory (`Dir`): a file named that way is
   then named by its whole path, as a question about it will name it. Leave everything else to
   the Writer.

   Where the CLI records [where a turn stands](sources.md#where-a-turn-stands), say how in
   `w.Tells` — by its last rows, or by a row it writes as every turn ends — and mark that row
   (`Ends`) and a response it is still writing (`Unfinished`); the Writer works out the rest.
   Leave it unset for a CLI whose rows do not show it: a state that is wrong is worse than
   none.

   For a CLI that only ever adds to the end of its files, a read can be taken up where the
   latest turn began instead of read whole each time the file grows. Read with
   `kit.LinesFrom`, which tells each line its place (offset, and the CRC-32C of the bytes
   before it); after the `w.Add` that begins a turn, call `w.MarkTurn` with that place and
   whatever the adapter carries from row to row. When the index offers the mark back in
   `w.From`, check `kit.Before(path, place)`, call `w.Resume(w.From)`, and read on from the
   place with what it carried; a file that changed before the place is read whole. The
   index keeps the latest mark with the session and offers it only to the same reading
   (`Reads`); a change to what a mark holds raises `markVersion` in
   `internal/index/ingest.go`. Claude Code's adapter does this.

   Read leniently — a transcript is being written while you read it, so its last line may be
   half there, and CLIs change formats without notice. `kit` has what that takes: `kit.Lines`
   (any line length, zstd, a truncated tail), `kit.Str` / `kit.Num` / `kit.Time` (a value of the
   wrong type is absent, not a panic), `kit.ReadDB` (a database another process is writing).

2. **Export a `Spec`** — the name, where the store is by default (honouring the CLI's own
   variable for moving it), how to open it, how a session is resumed:

   ```go
   var Spec = sources.Spec{
       Name: "mycli", Title: "My CLI",
       Locate:  func(env sources.Env) string { return filepath.Join(env.Home, ".mycli", "sessions") },
       Open:    func(location string) sources.Source { return &Source{root: location} },
       Movable: true,
       Resume:  "mycli --resume",
       Store:   "~/.mycli/sessions/<id>.jsonl",
       Reads:   1,
   }
   ```

   `Reads` is the version of how the adapter reads the CLI's files. **Raise it with any change
   that would read a session already in an index differently** — a record now read, a figure now
   bounded, a field now taken from elsewhere. The index keeps beside each session the reading it
   was read with, and reads again, once, at the next sync, every session of the CLI an older
   reading made; without it, an index keeps what the older reading made of every session whose
   files have not moved. A session a newer reading made is left alone, so an older `asq` sharing
   the index does not undo it. A change to the Writer reads sessions differently too: raise
   `Reads` for each CLI whose sessions it makes another record of, and for no other — each
   session read again is read whole.

   A change to *which* sessions the adapter lists — their keys: one file read as two sessions,
   or two as one — is not a new reading but a new index file (`schemaVersion` in
   `internal/index/schema.go`). Each `asq` removes from the index what it does not list, so two
   that list differently and share a file would remove each other's sessions on every sync.

   A change to what the search's words table holds for a turn — its columns, what goes in them —
   is neither: it raises `wordsVersion` beside `schemaVersion`. An index whose table an older
   `asq` made has it made anew when it is opened, and the next sync writes every turn's words
   from what the index holds, without reading a transcript. One a newer `asq` made is left as it
   is: the older one searches it and writes the columns it knows, and leaves the turns it writes
   for the newer one to write again. So a change keeps the columns there are, in their order, and
   adds any after them.

   A change to how a failure is judged to have passed later — the rule in
   `internal/index/later.go`, or `command.Key` and `command.Passed` beneath it — raises
   `laterVersion` beside them. Each failure keeps the version that judged it; one an older `asq`
   judged, or none, is judged again at the next sync from the turns the index holds, without
   reading a transcript. One a newer `asq` judged is left to it.

   A change to how sessions are tied together once read — which transcript a subagent ran in,
   which one is a copy of another (`link`, in `internal/index/link.go`) — raises `linkVersion`
   beside them: the next sync links every session again, whether or not it reads a transcript.

   `Retention` is for a CLI that deletes its own old sessions by default: one clause saying what
   it deletes, after how long, and the setting and file that change it, as the CLI's own
   documentation gives them. What the CLI deletes leaves the index too, and `asq doctor` says so
   under the store. A CLI that keeps its sessions leaves it empty. The adapter of a CLI that
   deletes them also says which files a session is made of — `Files`, of `sources.Keeper` —
   so that `asq keep` can copy them into a folder of its own, laid out as in the store, and read
   them there with the same adapter. It gives a session's own files apart from those other
   sessions read too, such as Gemini CLI's `.project_root`: a copy no longer kept takes its own
   with it and leaves the others.

3. **Add it to `catalog.All`.** The list, search, briefs, usage, failures, traces, the page and
   the MCP tools now cover it.

4. **Test it from files.** `internal/sources/sourcetest` writes a transcript and reads it back
   through the adapter and the Writer: `sourcetest.Write(t, path, rows…)`, then
   `sourcetest.One(t, Spec.Open(root))` gives the session, its stats and its turns. Build the
   fixture from what the CLI really writes — record a short session and trim it — including the
   awkward rows: a failed tool call, an interruption, a compaction, a subagent.

`asq doctor` then reads the newest real session of the store whole and says how many turns and
steps it found, which is the quickest check that the adapter and the real format agree.

## Tests

- **Adapters** are tested from fixture files, one package each.
- **Everything above the adapters** — index, app, cli, server, mcp, otel — is tested against
  `internal/fixture`: a small Claude Code store on disk (two sessions, a subagent, a failing
  command, a long output, CJK text, a planted secret), read through the real adapter, Writer and
  index. A test there checks the whole path, not a stand-in for it.
- **Text handling** (`internal/txt`, `internal/render`) is tested with hostile input: markup in
  a title, a fence inside an output, escape sequences, key-shaped strings.

A change in behaviour comes with the test that would have caught its absence.

## Working on the page

```sh
asq serve --page-dir web
```

serves `web/` from disk, re-read on every request: edit, reload. The rules the page is built
under, and why, are in [The page](ui.md).

## Screenshots

The page's screenshots in `docs/screenshots/` are of a synthetic history: nothing in them is
anyone's. `docs/screenshots/make/gen.py` writes it — a Claude Code store and a Codex store under
`/tmp/demo`, and the projects they worked in under `/tmp/work`, both emptied first — with times
relative to now, files whose modification times give the Files view each of its states,
failures that pass later, and a turn that ended on an error. Then serve it and take the pictures:

```sh
python3 docs/screenshots/make/gen.py
go build -o /tmp/asq ./cmd/asq
env -i PATH=/usr/bin:/bin HOME=/tmp/demo ASQ_INDEX=/tmp/demo/index.db ASQ_STATE=/tmp/demo/state \
  /tmp/asq serve --port 7882
```

and run `docs/screenshots/make/shots.js` — a Playwright page function — from the repository's
root. It takes the eleven images at their sizes: 1440×900, and the phone's 390×844.

## Releasing

Tag `vX.Y.Z` with an annotated tag whose message is the release notes (`git tag -a
--cleanup=verbatim`, or git strips Markdown headings as comments) and push it. The release
workflow tests on three platforms, cross-compiles six, publishes the container image from the
same linux binaries, writes `SHA256SUMS`, attaches the installers, and creates the release. Both
images are also built and run on every push, so a tag is not where a broken one is found. The version is set at link time: `-X github.com/itswl/agent-session-query/internal/app.Version=…`.
A binary built without it reports the version Go recorded of the module: the release
`go install …@latest` fetched, or for a build in a checkout a pseudo-version naming the commit
(`v1.5.1-0.20261122134833-b66c73f1d4cf`, `+dirty` with changes not committed). A build that
recorded neither — a container built from a copy of the source, which has no `.git` — is `dev`.

Before the release is created, each archive and installer, and the image, gets a build
attestation (`actions/attest`): a statement of the workflow, the commit and the run that made
it, signed through Sigstore with the run's own identity and kept by GitHub — the image's is
pushed to the registry beside it. `SHA256SUMS` comes from the same release and cannot say that;
`gh attestation verify <file> --repo itswl/agent-session-query` can. Signing for macOS, below,
is apart from it and unchanged by it.

### Homebrew and Scoop

This repository is a Homebrew tap and a Scoop bucket: `Formula/asq.rb` and `bucket/asq.json`
name the latest release, its archives and their checksums, and `brew upgrade` and
`scoop update` see a release once main names it. Neither is edited by hand.
`scripts/package_managers.py vX.Y.Z SHA256SUMS` writes both, and the release's last job runs it
with `--newer` and the `SHA256SUMS` just published and commits the two files to main through the
API, as the workflow's own commit. `--newer` writes nothing for a pre-release (`v1.7.0-rc.1`), nor
for a release older than the one the formula names (`v1.5.1` tagged after `v1.6.0`): either would
move every Homebrew and Scoop user onto it. When main does not take that commit — protected, or moved meanwhile —
the job fails and says to run the script by hand and commit what it writes. CI runs
`scripts/package_managers.py --check`: each file is what the script writes for the release they
both name.

### Signing for macOS

macOS remembers whether a program may read a folder it guards — Documents, Desktop, Downloads,
iCloud Drive, another disk — against the program's signature. Go's own signature is a hash of the
one build, so every release is a new program to it and its users are asked again. Once the
repository has the certificate below, the release signs both macOS binaries with it and the
identifier `io.github.itswl.asq`: every release is then the same program to macOS, and the answer
outlasts an upgrade. A binary built from source is still asked again after every build.

The certificate is self-signed, made once, and must never change: a release signed with another
— or not signed — has every macOS user asked again. It is set up in the repository's settings:

| | |
|---|---|
| secret `MACOS_SIGNING_KEY` | the private key, PEM |
| secret `MACOS_SIGNING_CERT` | the certificate, PEM |
| variable `MACOS_SIGNING_SHA1` | the certificate's SHA-1, which says signing is set up |

```sh
/usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 10950 -keyout key.pem -out cert.pem \
  -subj '/CN=asq' -addext 'basicConstraints=critical,CA:false' \
  -addext 'keyUsage=critical,digitalSignature' -addext 'extendedKeyUsage=critical,codeSigning'
/usr/bin/openssl x509 -outform DER -in cert.pem | shasum      # the variable
gh secret set MACOS_SIGNING_KEY < key.pem
gh secret set MACOS_SIGNING_CERT < cert.pem
gh variable set MACOS_SIGNING_SHA1 --body <the SHA-1>
```

Keep `key.pem` and `cert.pem` somewhere safe offline: GitHub does not give a secret back. The
build signs with [rcodesign](https://github.com/indygreg/apple-platform-rs), pinned by its
digest, refuses a certificate other than the one the variable names, and fails when the variable
is set and a secret is not. A job on macOS then checks both binaries with `codesign` against
`identifier "io.github.itswl.asq" and certificate root = H"<SHA-1>"` before anything is
published. Without the variable, nothing is signed. `codesign -d -r- $(command -v asq)` shows how
an installed binary is signed.
