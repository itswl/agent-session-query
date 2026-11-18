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
internal/mcp            the MCP server: stdio and HTTP, ten tools
internal/otel           OTLP export: what a turn becomes, the ledger of what was sent
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
        │
internal/model          the record: Session, Turn, Item, Tool, Usage
internal/txt            cleaning and cutting text
```

The three faces — `cli`, `server`, `mcp` — are thin on purpose. Each parses its own kind of
request, calls the same `app` and `index` operations, and renders the answer its own way. A new
question is answered in `index` (or `app`) once and then exposed three times, in a few lines
each.

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
   time). A session whose stamp has not moved is not read again — get the stamp right and the
   adapter is incremental for free.

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

   Convert token counts to [the one shape](model.md#usage-one-shape): input without cached
   tokens, output with reasoning. Mark what was not typed by a person (`Injected`), what a fork
   copied (`Inherited`), what was typed mid-turn (`Queued`). Leave everything else to the Writer.

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
   the index does not undo it.

   A change to *which* sessions the adapter lists — their keys: one file read as two sessions,
   or two as one — is not a new reading but a new index file (`schemaVersion` in
   `internal/index/schema.go`). Each `asq` removes from the index what it does not list, so two
   that list differently and share a file would remove each other's sessions on every sync.

   `Retention` is for a CLI that deletes its own old sessions by default: one clause saying what
   it deletes, after how long, and the setting and file that change it, as the CLI's own
   documentation gives them. What the CLI deletes leaves the index too, and `asq doctor` says so
   under the store. A CLI that keeps its sessions leaves it empty.

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
relative to now, files whose modification times give the Files view each of its states, and
failures that pass later. Then serve it and take the pictures:

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
