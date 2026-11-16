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

- programs send `Authorization: Bearer <token>`;
- the page trades the token once, at `POST /api/login`, for a cookie script never sees —
  `HttpOnly`, `SameSite=Strict`, good for 180 days. The cookie holds no secret: it is an expiry
  and its signature, and the server keeps nothing about it. The key it is signed with is made
  from the token and from 32 random bytes the server keeps in its state directory
  (`session.key`, readable by its owner). So nobody can mint or extend a cookie; changing the
  token retires every one at once, and so does deleting that file; and a cookie forged from a
  guess at the token is refused whether or not the guess was right — it is not a way to try
  tokens. Where there is no directory to keep the secret in, the server makes one for the run
  and says that sign-ins will end with it.

Ten wrong tokens from one caller in a minute, by either route, and that caller is answered `429`
until the minute rolls over — without the token even being compared. A caller is the address a
connection comes from; an IPv6 caller is its `/64`, since that is what it holds.

**Behind a proxy** every connection comes from the proxy, so "the address it comes from" would
name everyone at once — and ten wrong tokens from anywhere would shut out every caller, the
agents on the machine itself included. Two things prevent that:

- What a proxy relayed is counted apart from what reached the port directly. A request with a
  forwarding header (`Forwarded`, `X-Forwarded-*`, `X-Real-IP`, `Via`, `CF-Connecting-IP`) is
  one of the relayed; nothing that arrives through the proxy can spend the allowance of the
  programs that call `asq` on its own machine. This needs no setting.
- To count the callers behind the proxy one by one, say which header names them:
  `--proxy-header CF-Connecting-IP` behind Cloudflare, `X-Real-IP` behind the nginx below,
  `X-Forwarded-For` behind anything that appends to it (the last address in it is used — the
  one your proxy wrote). Without it they share one allowance, and one of them guessing shuts
  out the rest of them for the minute.

The header is believed only on a connection from the machine itself or a private network, which
is where a proxy of yours connects from. Name one your proxy *sets*: a header it merely passes
on is the caller's own word, and a caller who may choose its address has no limit at all. A
tunnel that passes bytes through unchanged (`ssh -R`, a TCP forward) marks nothing, and what
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
  <key>ProcessType</key><string>Background</string>
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
