# Deploying

`asq` needs no deployment to be useful: the command line, and `asq mcp` started by an agent, read
the stores directly. This page is about `asq serve` — the page, the HTTP API and the MCP
endpoint — kept running, and about who may reach it.

```sh
asq serve                 # http://127.0.0.1:7717, this machine only
```

## Who may ask

What `asq serve` reads out is private: every prompt you typed, every file an agent printed.
There are two ways to run it, and each has its own guard.

**Without a token** it listens on loopback only — starting it on any other address is refused,
not warned about — and what protects it is that only this machine can connect. The hole in that
is the browser: any web page you visit can make your browser connect to `localhost`. So a
request must name a loopback `Host` (a DNS name rebound to `127.0.0.1` does not), and must not
come from another site: a foreign `Origin`, or a browser's `Sec-Fetch-Site: cross-site`, is
refused with `403` before anything is read.

**With a token** — `ASQ_TOKEN`, at least 16 characters; `openssl rand -hex 24` makes a good one
— it may listen anywhere, and every request to the API proves it holds the token:

- programs send `Authorization: Bearer <token>` — an agent's MCP client as well, set up as
  [MCP](mcp.md#over-http) shows;
- the page trades the token once, at `POST /api/login`, for a cookie script never sees —
  `HttpOnly`, `SameSite=Strict`, good for 180 days. The cookie holds no secret: it is an expiry
  and its signature, and the server keeps nothing about it. The key it is signed with is made
  from the token and from 32 random bytes the server keeps in its state directory
  (`session.key`, its owner's alone). So nobody can mint or extend a cookie; changing the token
  retires every one, and so do deleting that file and restarting the server, which reads it
  when it starts; and a cookie forged from a guess at the token is refused whether or not the
  guess was right — it is not a way to try tokens. A `session.key` others can read is replaced,
  and a state directory that is not yours, or that anyone may write, is not used. Where there is no directory to keep the
  secret in, the server makes one for the run and says that sign-ins will end with it.

Ten wrong tokens from one caller in a minute, by either route, and that caller is answered `429`
until the minute rolls over, whether or not the token it brings is right. Who "one caller" is decides
whom the limit shuts out:

- **The programs on this machine** — the agents calling the API or `/mcp`, a script with
  `curl` — are one caller: a loopback connection with no forwarding header and no mark of a
  browser. Nothing that arrives through a proxy, and nothing a web page does, is charged to
  them, so neither can shut them out.
- **A page in a browser on this machine** is another: a loopback connection whose request
  carries what browsers add and a page's script cannot remove (`Sec-Fetch-Site`, `Origin`). A
  page elsewhere that points a name of its own at `127.0.0.1` is such a page.
- **A caller from elsewhere** is the address its connection comes from; an IPv6 caller is its
  `/64`, since that is what it holds.
- **What a proxy relays** — a request with a forwarding header (`Forwarded`, `X-Forwarded-*`,
  `X-Real-IP`, `Via`, `CF-Connecting-IP`) — is counted apart from what reached the port
  directly. This needs no setting. Without more, everything one proxy relays is one caller:
  one of its callers guessing shuts out the rest of them for the minute.

To count the callers behind a proxy one by one, say which header names them:
`--proxy-header CF-Connecting-IP` behind Cloudflare, `X-Real-IP` behind the nginx below. The
header is believed only on a connection from this machine or a private network, which is where a
proxy of yours connects from, and only the last address in it, the one your proxy wrote. Name a
header your proxy *sets*: one it merely passes on is the caller's own word. `X-Forwarded-For`
is right only behind a proxy that appends to it — nginx with
`proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for`, not as configured below.

Whatever the header says, every caller it names draws on one more allowance as well, the
proxy's: ten callers' worth of wrong tokens a minute, together. A caller who can choose what the
header says — a page that reaches the proxy's port, a device on the network — gets that and no
more, instead of a fresh allowance with every guess. A caller the header names is counted by its
address and the proxy that named it: naming a machine through one proxy spends nothing of what it
has through another, or calling the port directly.

When the proxy's allowance is spent, the proxy is shut for the rest of the minute to callers that
have not shown the right token in the last day, sign-ins included. A caller that has — an agent
asking with its token, a browser that signed in — is held back only by its own wrong tokens, and
a browser holding its cookie is never held back at all. So callers guessing at ten addresses can
keep newcomers out for as long as they keep at it, and nobody who is already in. Who is known is kept
in the state directory beside the secret (`known.json`, its owner's alone), so a restart does not
make everyone a newcomer. One limit: a caller that can write the header through the same proxy can
name a known caller and spend its ten.

The server remembers 4,096 callers at once. Past that, during a flood of addresses, the callers it
does not know share one allowance of their kind; the callers on this machine are always known.

A tunnel that passes bytes through unchanged (`ssh -R`, a TCP forward) marks nothing, and what
comes through it cannot be told from this machine's own.

Keep the token in the environment, not in `--token`: a flag shows in the process list.

**TLS.** `asq` speaks plain HTTP. On loopback that is what it should speak. Anywhere else, put
it behind something that terminates TLS — a reverse proxy, a tunnel, a tailnet — because a token
sent in the clear is a token shared with the network:

```nginx
location / {
    proxy_pass http://127.0.0.1:7717;             # asq serve --proxy-header X-Real-IP
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;   # lets the cookie be marked Secure
    proxy_set_header X-Real-IP $remote_addr;      # who is calling: wrong tokens are counted per caller
    proxy_buffering off;                           # the live stream is sent as it happens
    proxy_read_timeout 1h;
}
```

**Other origins.** A page served from somewhere else may call the API only if its origin is
listed: `--cors-origin https://dash.example` (repeatable, or `ASQ_CORS_ORIGIN`). Such a page
authenticates with the header; the cookie is never sent across origins. `--cors-origin '*'` is
refused without a token.

## Keeping it running

**Linux — a systemd user service**

```ini
# ~/.config/systemd/user/asq.service
[Unit]
Description=asq — agent session history
After=network.target

[Service]
ExecStart=%h/.local/bin/asq serve
# For anything but loopback: ASQ_HOST=0.0.0.0 and ASQ_TOKEN=… in this file, mode 600
EnvironmentFile=-%h/.config/asq/env
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.cache/asq %h/.local/state/asq
PrivateTmp=true

[Install]
WantedBy=default.target
```

```sh
mkdir -p ~/.cache/asq ~/.local/state/asq
systemctl --user daemon-reload && systemctl --user enable --now asq
sudo loginctl enable-linger "$USER"     # without this the service stops when you log out
```

The home directory is mounted read-only for the service — it reads the stores and can write only
its index and its state. Variables that move a CLI's store (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, …)
go in the environment file too: they are read from the service's environment, not from your
shell's.

**macOS — a LaunchAgent**

```xml
<!-- ~/Library/LaunchAgents/io.github.itswl.asq.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.itswl.asq</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/asq</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <!-- Restart only on an abnormal exit -->
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <!-- Standard, not Background: it answers what a person or an agent asks, and Background
       throttles it as work nobody is waiting for — a search took 17.8 s that way, 0.37 s not -->
  <key>ProcessType</key><string>Standard</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/asq.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.github.itswl.asq.plist
launchctl kickstart -k "gui/$(id -u)/io.github.itswl.asq"     # restart after an edit
launchctl bootout "gui/$(id -u)/io.github.itswl.asq"          # stop and unload
```

- A plist does not expand `~`: every path in it is absolute. This is the usual way a launchd
  setup fails, and it fails quietly.
- A LaunchAgent, not a LaunchDaemon: a daemon runs as root, whose home is not yours, and reads
  nobody's sessions.
- If the plist holds a token (`EnvironmentVariables`), `chmod 600` it.
- The Files view says how each file a turn changed stands now, which means reading the file —
  only when the view, `asq files` or the MCP tool asks, only the files the sessions named, in their
  own directories, and never writing. For a project in a folder macOS guards — Documents,
  Desktop, Downloads, iCloud Drive, another disk — the first read asks on the Mac's screen
  whether `asq` may, and the read waits for the answer. The page does not: it shows those files
  without how they stand, says which folder is waiting, and judges them once the question is
  answered. Allowing that one folder is all it needs; Full Disk Access is far more, and not
  needed. Not allowing it costs only how those files stand.
- macOS keeps the answer across upgrades: the released binaries are all signed with one
  certificate, as `io.github.itswl.asq` (`codesign -d -r- ~/.local/bin/asq` shows it), and it
  remembers the answer against that and against the binary's path — so install upgrades where
  the first one went. A binary built from source is not signed so, and is asked again after each
  build. macOS asks about `asq` itself only when launchd runs it, as here; run from a terminal or
  by an agent's app, it asks about that app, and remembers it for that app.

**Windows — a scheduled task at logon**

```powershell
schtasks /create /tn asq /sc onlogon /tr "$env:LOCALAPPDATA\Programs\asq\asq.exe serve"
```

For a token, set it in the user's environment (`[Environment]::SetEnvironmentVariable('ASQ_TOKEN', '…', 'User')`)
rather than on the command line, which Task Manager shows.

## In a container

A single binary is already easy to run, so a container is not the route for the machine you sit
at. Where it fits is a host you reach only over the network: the box the agents run on.

```sh
ASQ_TOKEN=$(openssl rand -hex 24) docker compose up -d      # builds from this checkout
```

[`docker-compose.yml`](../docker-compose.yml) lists every store's mount with the note each
needs. Each release also publishes a multi-arch image (linux/amd64, linux/arm64) built from the
released binaries, `ghcr.io/itswl/agent-session-query`, so nothing has to be built:

```sh
docker run -d --name asq -p 127.0.0.1:7717:7717 \
  -e ASQ_TOKEN="$ASQ_TOKEN" -e TZ=Europe/Berlin \
  -v asq-index:/data \
  -v ~/.claude/projects:/root/.claude/projects:ro \
  -v ~/.codex/sessions:/root/.codex/sessions:ro \
  ghcr.io/itswl/agent-session-query:latest
```

- **A token is required.** A container listens on all of its interfaces, and `asq` refuses to do
  that without one: the container exits at once with the reason in `docker logs`.
- **Mount stores read-only, and only the sessions.** Inside, the home directory is `/root`, so
  each store mounts where its CLI keeps it under the home directory. Mount `~/.codex/sessions`,
  not `~/.codex`: beside the sessions sit the CLI's credentials. Hermes is the exception — its
  `state.db` needs its `-wal` and `-shm` beside it, so the whole of `~/.hermes` comes along.
- **`/data` is the index, the export ledger and the secret sign-ins are signed with.** A volume
  there means a restart reads only what changed and nobody has to sign in again; without one the
  history is read again on every start, and every start signs everyone out.
- **`TZ`** names the zone a report's days are counted in. The binary carries the zone database,
  so it works in an image that has none.
- **It runs as root, on purpose.** Mounted transcripts keep their owner and their `0600` mode,
  the image cannot know the host user's uid, and root is the one uid that reads any owner's. The
  read-only mounts are the containment. To run as yourself, add `--user "$(id -u):$(id -g)"` and
  make the `/data` volume writable by that uid.
- The image is built from scratch: the binary, root certificates (for `asq otel` to an HTTPS
  collector), nothing else. Its health check is the binary asking itself: `asq ping`.

Stores outside the default home — another machine's, a fleet's bind mount — are mounted anywhere
and named with `ASQ_PATH`:

```yaml
    environment:
      ASQ_PATH: "claude@box2=/mnt/box2"
    volumes:
      - /srv/box2/.claude/projects:/mnt/box2:ro
```

## Exporting continuously

`asq otel --every 10m` is a process of its own; it shares the index with a running server, and
either can be restarted without the other. Under systemd it is a second unit with
`ExecStart=%h/.local/bin/asq otel --every 10m` and the collector's address in its environment
file. See [Telemetry](telemetry.md).

## What it costs to leave running

An idle server looks at the stores every thirty seconds — a `stat` per transcript — and reads
nothing that did not change. While a page is open it looks every two seconds. Memory is the
SQLite page cache, tens of megabytes. The index is a fraction of the size of the transcripts it
caches, and `asq index rm` gets all of it back.
