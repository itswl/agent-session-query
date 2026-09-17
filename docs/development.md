# Development

```
.
├── cmd/agent-session-query/   # entry point (the implementation lives in internal/app)
├── cmd/healthcheck/           # the container liveness probe
├── internal/app/              # everything: run.go (flags and startup), http.go (routing,
│                              #   auth, rate limiting), api.go (merging, matching, caching),
│                              #   record.go, source*.go (the data sources),
│                              #   search.go (full-text search), export.go (Markdown export),
│                              #   mcp.go (the MCP server),
│                              #   filecache.go (file-head cache keyed by mtime),
│                              #   hermes_sqlite.go, ui.go + ui/ (go:embed three-pane page),
│                              #   console_{windows,other}.go (Windows console code page).
│                              #   Tests sit alongside their source (source_*_test.go, ...)
├── docs/internals.md          # parsing details and performance
├── .github/workflows/test.yml     # push / PR: tests on three platforms, gofmt/vet, and a
│                                  #   cross-compile check over all six release targets
├── .github/workflows/release.yml  # on a tag: tests on three platforms, then cross-compile
│                                  #   six targets and publish the Release
├── Dockerfile / docker-compose.yml
└── go.mod / go.sum            # one dependency: a pure-Go SQLite driver
```

```bash
go test -race ./...   # the full suite (no network, no dependency on what is installed here)
gofmt -l .            # formatting check
go vet ./...
```

Releasing: [release.yml](../.github/workflows/release.yml) uses the tag annotation as the
Release body, so tag with `--cleanup=verbatim` — otherwise git strips Markdown `##` headings
as comment lines:

```bash
git tag -a v0.3.0 --cleanup=verbatim -F notes.md
git push origin v0.3.0
```

Adding a data source: implement the `SessionSource` interface (`Mode` / `Location` /
`Exists` / `List` / `Messages` / `Final`), register it in the `factories` map inside
`buildSources()`, and add the mode name to `knownModes`. Merging across sources, match
ranking, full-text search, project grouping and the `source` tag are all handled once by the
framework. A source whose sessions do not live in files (Hermes when it is all SQLite, for
instance) can additionally implement `searchableSource` to take over searching itself.
