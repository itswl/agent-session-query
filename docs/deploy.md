# Deployment

`-d` only puts the process in the background; if it dies, nothing brings it back. For a real
long-running service, pick one of the three below. Flags are described in the
[configuration table in the README](../README.md#configuration).

## Supervised options

**Linux (systemd user service)** — a single binary, no Docker required:

```ini
# ~/.config/systemd/user/agent-session-query.service
[Unit]
Description=Local agent session query (read-only)
After=network.target

[Service]
ExecStart=%h/.local/bin/agent-session-query --host 127.0.0.1 --port 8787
EnvironmentFile=%h/.config/agent-session-query/env    # one line: HOOK_TOKEN=...
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload && systemctl --user enable --now agent-session-query
sudo loginctl enable-linger "$USER"     # without this, the service stops when you log out
```

**macOS (launchd LaunchAgent)** — drop it in `~/Library/LaunchAgents/` and it starts at
login:

```xml
<!-- ~/Library/LaunchAgents/io.github.itswl.agent-session-query.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.itswl.agent-session-query</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/YOURNAME/.local/bin/agent-session-query</string>
    <string>--host</string><string>127.0.0.1</string>
    <string>--port</string><string>8787</string>
    <string>--mode</string><string>auto</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>HOOK_TOKEN</key><string>your-token</string></dict>
  <key>RunAtLoad</key><true/>
  <!-- Restart only on an abnormal exit, the equivalent of systemd's Restart=on-failure -->
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/Users/YOURNAME/Library/Logs/agent-session-query.log</string>
  <key>StandardErrorPath</key><string>/Users/YOURNAME/Library/Logs/agent-session-query.log</string>
</dict>
</plist>
```

```bash
chmod 600 ~/Library/LaunchAgents/io.github.itswl.agent-session-query.plist  # it holds a token
plutil -lint ~/Library/LaunchAgents/io.github.itswl.agent-session-query.plist

launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.github.itswl.agent-session-query.plist
launchctl print "gui/$(id -u)/io.github.itswl.agent-session-query"          # state and pid
launchctl kickstart -k "gui/$(id -u)/io.github.itswl.agent-session-query"   # restart after an edit
launchctl bootout   "gui/$(id -u)/io.github.itswl.agent-session-query"      # stop and unload
```

Things that catch people out:

- **A plist does not expand `~`, so every path must be absolute** — both in
  `ProgramArguments` and for the logs. This is the most common way launchd setups fail, and
  it fails quietly
- **Use a LaunchAgent, not a LaunchDaemon.** Services in `/Library/LaunchDaemons/` run as
  root at boot, where `$HOME` is root's, so they read `/var/root/.claude` rather than your
  sessions
- The older `launchctl load -w` / `unload` spelling still works but is deprecated; on a
  current machine use the `bootstrap` / `bootout` commands above
- launchd does not rotate logs, so clean them up yourself or hand that to `newsyslog`

**Windows** — no systemd, so use a scheduled task that starts at logon. Keep the token in
the environment rather than on the command line (arguments are visible in Task Manager):

```powershell
[Environment]::SetEnvironmentVariable('HOOK_TOKEN', 'your-token', 'User')
schtasks /create /tn agent-session-query /sc onlogon `
  /tr "$HOME\bin\agent-session-query.exe --host 127.0.0.1 --port 8787"
```

That leaves a console window open. To run fully in the background, or to start at boot
rather than at logon, register it as a service with something like [nssm](https://nssm.cc/).

**Docker** — a single binary is already easy enough, so Docker is not the recommended route;
the repository keeps `Dockerfile` / `docker-compose.yml` around for those who want it:

```bash
HOOK_TOKEN=mysecrettoken docker compose up -d   # see docker-compose.yml for the six mounts
```

Inside the container, source paths derive from `$HOME` (the mount points sit under
`/root/...`). Mount the whole of `~/.hermes` for Hermes — `state.db` needs its `-wal`/`-shm`
companions alongside it. The image's `CMD` passes `--host 0.0.0.0` explicitly (otherwise the
port mapping cannot reach it), so a container deployment **must** set `HOOK_TOKEN`.

## Sessions outside the default home

Sources derive from `$HOME`, so sessions on another disk, in a backup, or inside agent
containers whose sessions land on a bind mount are invisible by default. `--path` reads
them, relocating a source or adding labeled instances of it:

```
./agent-session-query --path claude:build-box=/mnt/containers/build-box/home/.claude/projects
```

The flags, their semantics and the label rules: [sources.md](sources.md).

## Applies to all of them

- **Keep the token in the environment**: `HOOK_TOKEN` is read automatically. Command-line
  arguments show up in `ps`, Task Manager and `docker inspect`; environment variables do not.
- **The default bind is `127.0.0.1`.** To share the service, pass `--host 0.0.0.0`
  explicitly, set `--hook_token`, and put it behind a reverse proxy. The reasoning is in the
  [security section of the README](../README.md#security).
