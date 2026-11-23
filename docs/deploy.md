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
not warned about — and what protects it is that only this machine can connect. Two things on
this machine can undo that, and what comes by either is refused with `403` before anything is
read:

- **The browser.** Any web page you visit can make your browser connect to `localhost`. So a
  request must name a loopback `Host` (a DNS name rebound to `127.0.0.1` does not), and must not
  come from another site: a foreign `Origin`, or a browser's `Sec-Fetch-Site: cross-site`.
- **A proxy or a tunnel running here** — nginx, Caddy, `cloudflared`, ngrok, Tailscale Serve. It
  connects from loopback, passes on what the whole network sends, and can be told to write
  `Host: localhost`. So a request that bears a proxy's mark — a forwarding header: `Forwarded`,
  `X-Forwarded-*`, `X-Real-IP`, `Via`, `CF-Connecting-IP` — is refused, with an answer that
  says the server has no token and is being reached through a proxy; and `--proxy-header`, or
  `ASQ_PROXY_HEADER`, without a token is refused at start. To serve through a proxy or a tunnel,
  set `ASQ_TOKEN`.

A proxy that adds no forwarding header cannot be told from this machine's own programs: nginx's
`proxy_pass` adds none unless told to, a proxy may be set to remove them, and a tunnel that
passes bytes through unchanged (`ssh -R`, a TCP forward) has none to add. Put none of them in
front of a server without a token.

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

**What is written down.** Each wrong token, each caller refused for too many, and each sign-in
is a line in the server's log — its standard error: `journalctl --user -u asq` under systemd, the
`StandardErrorPath` file under launchd, `docker logs` in a container — with the time, and the
caller as the limit above counts it: `local`, `local browser`, `203.0.113.9`,
`198.51.100.7 relayed`, `named 203.0.113.9 via local`. Never a token, right or wrong, nor the
cookie. Without a token, each request refused for coming through a proxy is a line too.

```
2026-11-18T15:04:05+01:00 a wrong token from "named 203.0.113.9 via local", in an Authorization header
2026-11-18T15:04:09+01:00 refused "named 203.0.113.9 via local": too many wrong tokens from this caller
2026-11-18T15:05:07+01:00 the minute from 15:04:05 held more than was written: 9 wrong tokens, 37 refusals
```

The callers the log reports on cannot fill it: a caller's first line of each kind in a minute is
written and the rest of that minute only counted, no more than ten lines are written in a minute
in all, and once the minute is over one line says how many of each kind were not. `/api/health`
counts them over the last day, under `guard` ([HTTP API](api.md#status)) — for a caller the
guards let in, as the rest of it is.

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

```sh
asq service install                    # serve on 127.0.0.1:7717, started at login and kept running
asq service install --port 7720        # serve's own flags: --host, --port, --proxy-header, --cors-origin
asq service status                     # what it runs, whether it answers, where its log is
asq service uninstall                  # stopped, and its definition removed
```

`asq service install` sets up what the sections below show — a LaunchAgent on macOS, a systemd
user unit on Linux, a task at logon on Windows — with the full path of the `asq` that ran it,
has the system's service manager start it, and waits for it to answer. What it runs is what
`asq serve` would run in the shell it was typed in: the flags given (and `--index`, `--only`,
`--path`), and the variables `asq` reads as that shell has them — `ASQ_TOKEN` and the other
`ASQ_*`, the `XDG_*` directories, `TZ`, and each CLI's own for moving its store
(`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, …), which a service does not get from a shell. To serve
beyond this machine, set `ASQ_TOKEN` first: a definition that holds it is its owner's alone.
`--dry-run` prints what would be written and run, the token's value left out.

- Install it again to change it: the definition is written anew and the server started again.
  The same after an upgrade, which replaces the binary but not the server already running it.
- A definition made by hand, from below, is neither replaced nor removed without `--force`:
  status says whose it is, and how to take it down by hand.
- Something else answering on the port — an `asq serve` started in a terminal — is refused: the
  service would never get the port.
- If the server does not answer, install says so and exits 1 — under launchd, quoting what the
  server wrote to its log meanwhile.

**Installed with Homebrew**, `brew services start asq` is the other way: Homebrew's own
LaunchAgent or systemd unit, which runs `asq serve` with no settings — loopback, port 7717, no
token, none of the shell's variables. For any of those, use `asq service install`, which runs
Homebrew's `asq` by the path that outlives an upgrade (`…/opt/asq/bin/asq`). Not both: install
refuses to set up a second service while Homebrew's is there, and status names it. Scoop has no
services; `asq service install` runs Scoop's `asq` by its `current` directory, and the task is
stopped for an upgrade as below.

What it writes for each system, and how to set it up by hand:

**Linux — a systemd user service**

```ini
# ~/.config/systemd/user/asq.service
[Unit]
Description=asq — agent session history
After=network.target

[Service]
ExecStart=%h/.local/bin/asq serve
# For anything but loopback, or behind a proxy or a tunnel: ASQ_TOKEN=… in this file, mode 600
# (and ASQ_HOST=0.0.0.0 to listen on every address)
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
its index and its state. A folder named with `asq keep --dir` goes in `ReadWritePaths` too.
Variables that move a CLI's store (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, …) go in the environment
file too: they are read from the service's environment, not from your shell's. `asq service
install` writes them into the unit instead, as `Environment=` lines, and the directories the
service writes as they are on this machine — the index's, the state's, and the folder
`asq keep --dir` named when there is one, so install again after naming another — and says when
lingering is off. A server that finds no sessions says on the page where it looked.

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
    <string>/Users/YOU/.local/bin/asq</string>
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
  setup fails, and it fails quietly; `asq service install` writes them out.
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
  the first one went, as the installers do when run again. Homebrew does not: it keeps each
  version in a directory of its own (`Cellar/asq/<version>`), and the path that counts is the
  file's, not that of a link to it, so under Homebrew a folder macOS guards is asked about again
  after each `brew upgrade`. A binary built from source is not signed so, and is asked again
  after each build. macOS asks about `asq` itself only when launchd runs it, as here; run from a
  terminal or by an agent's app, it asks about that app, and remembers it for that app.

**Windows — a scheduled task at logon**

```powershell
$action   = New-ScheduledTaskAction -Execute "$env:LOCALAPPDATA\Programs\asq\asq.exe" -Argument serve
$trigger  = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -Priority 5 `
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName asq -Action $action -Trigger $trigger -Settings $settings
Start-ScheduledTask -TaskName asq
```

Task Scheduler's defaults are for work that ends: a task is stopped after three days, is not
started on battery and is stopped when the machine goes on battery, is not tried again when it
fails to start, and runs below normal priority. The settings undo each — no time limit, battery
as mains, a start that fails tried again every minute, and priority 5, which is normal.
`asq service install` registers the same task, from Task Scheduler's XML.

- It runs in a console window of its own, which is the server: minimise it rather than close it.
  A server that has stopped starts again at the next logon, or with
  `Start-ScheduledTask -TaskName asq`; `asq service status` says whether it runs.
- An upgrade replaces `asq.exe`, which Windows does not allow while it runs:
  `Stop-ScheduledTask -TaskName asq` before, `Start-ScheduledTask -TaskName asq` after.
- A task has the user's environment as Windows keeps it, not a shell's. For a token, set it there
  (`[Environment]::SetEnvironmentVariable('ASQ_TOKEN', '…', 'User')`) rather than on the command
  line, which Task Manager shows; `asq service install` names the variables of its shell that the
  task gets only from there.

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
  --mount type=bind,src="$HOME/.codex/session_index.jsonl",dst=/root/.codex/session_index.jsonl,readonly \
  ghcr.io/itswl/agent-session-query:latest
```

- **A token is required.** A container listens on all of its interfaces, and `asq` refuses to do
  that without one: the container exits at once with the reason in `docker logs`.
- **Mount stores read-only, and only the sessions.** Inside, the home directory is `/root`, so
  each store mounts where its CLI keeps it under the home directory. Mount `~/.codex/sessions`,
  not `~/.codex`: beside the sessions sit the CLI's credentials. Codex keeps the names threads
  were given beside them too, in `session_index.jsonl`, which comes as a file of its own; Docker
  makes a directory of a file that is missing — one Codex could then not write — so it is
  mounted so that a missing one stops the container instead: `touch` it first if Codex has not
  named a thread yet. Hermes is the exception — its
  `state.db` needs its `-wal` and `-shm` beside it, so the whole of `~/.hermes` comes along.
- **`/data` is the index, the export ledger, the secret sign-ins are signed with, and what
  `asq keep` keeps.** A volume
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
- Each release's image has a build attestation, pushed beside it, which names the workflow run
  and the commit that built it:
  `gh attestation verify oci://ghcr.io/itswl/agent-session-query:latest --repo itswl/agent-session-query`.

Stores outside the default home — another machine's, a fleet's bind mount — are mounted anywhere
and named with `ASQ_PATH` ([how to copy another machine's](sources.md#another-machines-sessions)):

```yaml
    environment:
      ASQ_PATH: "claude@box2=/mnt/box2"
    volumes:
      - /srv/box2/.claude/projects:/mnt/box2:ro
```

## Upgrading

A server goes on with the binary it was started from until it is restarted, whatever was
installed since. Replace the binary where it is — the installers do, run again, which is also
what keeps the answer macOS gave about a guarded folder; one Homebrew or Scoop installed they
leave to `brew upgrade asq` or `scoop update asq` — and restart the service: `asq service
install` again for the one it set up, or

```sh
systemctl --user restart asq                                    # Linux
launchctl kickstart -k "gui/$(id -u)/io.github.itswl.asq"     # macOS
brew services restart asq                                       # Homebrew's own service
```

On Windows a running `asq.exe` cannot be replaced: `schtasks /end /tn asq`, the installer or
`scoop update asq`, then `schtasks /run /tn asq`. A container built from this checkout is
`git pull`, then `docker compose up -d --build` with the same `ASQ_TOKEN`; one from the
published image is `docker pull` of it, then `docker rm -f asq` and the same `docker run` as
before. The volume at `/data` keeps the index and the sign-ins either way.

Until it is restarted, a server whose binary was replaced says so: once in its log, on the
page, and in `/api/health` (`replaced`, beside the `executable` it was started from).

```
asq: /home/you/.local/bin/asq was replaced since this server started, by an upgrade: it goes on as v1.4.0 until it is restarted
```

`asq doctor` asks the server on this machine which `asq` it runs — on `--port`, or `ASQ_PORT`,
and with neither on the port the service `asq service install` set up listens on, or `7717` —
and compares it with itself: one started before an upgrade, or from another install than the
one doctor is, is a warning that says which and what to do. A server with a token tells it only
with the token in `ASQ_TOKEN`. Nothing is asked of anywhere but this machine.

The index needs nothing: a release that reads a CLI's files differently reads them again by
itself, at its first look at the stores, and goes on answering meanwhile.

## Exporting continuously

`asq otel --every 10m` is a process of its own; it shares the index with a running server, and
either can be restarted without the other. Under systemd it is a second unit with
`ExecStart=%h/.local/bin/asq otel --every 10m` and the collector's address in its environment
file. See [Telemetry](telemetry.md).

## What it costs to leave running

An idle server looks at the stores every thirty seconds — a `stat` per transcript — and reads
nothing that did not change. While a page is open it looks every two seconds. Memory is the
SQLite page cache, tens of megabytes. The index is a fraction of the size of the transcripts it
caches, and `asq index rm` gets all of it back. What sessions leaving it free — Claude Code's
cleanup after 30 days, say — is given back to the disk as it goes.

Nothing about the index needs the server stopped: `asq index rebuild` and `asq index rm` make a
new file, and the server goes on with that one at its next look at the stores. One it finds
damaged it sets aside and reads anew, and says so in its log. Windows is the exception: it does
not remove a file a program holds open, so there the server is stopped for either command.
