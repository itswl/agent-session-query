# Development

```
.
├── cmd/agent-session-query/   # entry point (the implementation lives in internal/app)
├── cmd/healthcheck/           # the container liveness probe
├── internal/app/              # everything: run.go (flags and startup), http.go (routing,
│                              #   auth, rate limiting), api.go (merging, matching, caching),
│                              #   record.go, blocks.go (the one block shape every source
│                              #   produces), source*.go (the data sources),
│                              #   brief.go (rounds and the handoff brief),
│                              #   search.go (full-text search), export.go (Markdown export),
│                              #   mcp.go (the MCP server),
│                              #   filecache.go (file-head cache keyed by mtime),
│                              #   hermes_sqlite.go, ui.go + ui/ (go:embed three-pane page),
│                              #   console_{windows,other}.go (Windows console code page).
│                              #   Tests sit alongside their source (source_*_test.go, ...)
├── docs/internals.md          # parsing details and performance
├── docs/sources.md            # data sources and --path overrides
├── .github/workflows/test.yml     # push / PR: tests on three platforms, gofmt/vet, and a
│                                  #   cross-compile check over all six release targets
├── .github/workflows/release.yml  # on a tag: tests on three platforms, then cross-compile
│                                  #   six targets and publish the Release
├── Dockerfile / docker-compose.yml   # build from source, and compose
├── Dockerfile.release         # what release.yml publishes: the release binaries copied in
└── go.mod / go.sum            # one dependency: a pure-Go SQLite driver
```

```bash
go test -race ./...   # the full suite (no network, no dependency on what is installed here)
gofmt -l .            # formatting check
go vet ./...
node --check internal/app/ui/app.js   # where node exists; the suite runs this too, and CI enforces it
```

Releasing: [release.yml](../.github/workflows/release.yml) uses the tag annotation as the
Release body, so tag with `--cleanup=verbatim` — otherwise git strips Markdown `##` headings
as comment lines:

```bash
git tag -a v0.3.0 --cleanup=verbatim -F notes.md
git push origin v0.3.0
```

Adding a data source: implement the `SessionSource` interface (`Mode` / `Location` /
`Exists` / `List` / `Messages` / `Final`), then walk the lists a source's identity lives
in. A ninth source is a sweep, not a patch — the two real additions touched seven and
twelve files, and a source's name and abilities are spread across:

- `knownModes` and the `factories` map in `buildSources()` (`source.go`), plus `movable`
  and the whitelist inside `pathFlag.Set` if a single directory can relocate it
- `resumeCommands` (`record.go`) if the CLI can reopen a session by id
- an error path: implement `ListError` (see `listErrorReporter`) so a moved schema reads
  as "could not be read" instead of "no sessions" — the SQLite sources show the shape
- a counter in `msgcount.go` whose rule mirrors your `Final` (the list's count and the
  session's final must agree), and usage aliases in `usage.go` if the CLI reports tokens
- the UI's `SOURCE_CLASSES` and `countable` sets (`ui/app.js`)
- `docs/sources.md`, the README's source table and `docs/internals.md`

Build message content with the
helpers in `blocks.go` — `toolCallBlock` with the call's id, `toolResultBlock` with the id it
answers and a `toolOutcome` for how it ended, `thinkingBlock`, `eventBlock` — and honour
`messageQuery.full`, so the page, the brief and the MCP tools read the new source like the
others. Merging across sources, match
ranking, full-text search, project grouping and the `source` tag are all handled once by the
framework. A source whose sessions do not live in files (Hermes when it is all SQLite, for
instance) can additionally implement `searchableSource` to take over searching itself.
