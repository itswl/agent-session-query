# 部署

`-d` 只是把进程丢到后台，挂了不会自己起来。要真正常驻，用下面三种之一。
参数说明见 [README 的配置表](../README.md#配置)。

## 常驻方式

**Linux（systemd user service）**——单二进制，不需要 Docker：

```ini
# ~/.config/systemd/user/agent-session-query.service
[Unit]
Description=本地 Agent 会话查询（只读）
After=network.target

[Service]
ExecStart=%h/.local/bin/agent-session-query --host 127.0.0.1 --port 8787
EnvironmentFile=%h/.config/agent-session-query/env    # 里面一行 HOOK_TOKEN=...
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
sudo loginctl enable-linger "$USER"     # 不开的话，退出登录服务就停
```

**Windows**——没有 systemd，用计划任务在登录时拉起。令牌走环境变量，别写进命令行参数
（参数会出现在任务管理器里）：

```powershell
[Environment]::SetEnvironmentVariable('HOOK_TOKEN', '你的令牌', 'User')
schtasks /create /tn agent-session-query /sc onlogon `
  /tr "$HOME\bin\agent-session-query.exe --host 127.0.0.1 --port 8787"
```

这样会留一个控制台窗口；要完全后台跑或开机（而非登录）即启，用 [nssm](https://nssm.cc/) 注册成服务。

**Docker**——单二进制已经够省事，Docker 不是推荐方式，仓库保留 `Dockerfile` / `docker-compose.yml` 备用：

```bash
HOOK_TOKEN=mysecrettoken docker compose up -d   # 六个源目录的挂载见 docker-compose.yml
```

容器里数据源路径由 `$HOME` 推导（挂载点在 `/root/...`）；Hermes 要挂整个 `~/.hermes`（`state.db`
及其 `-wal`/`-shm` 都得跟着）。镜像的 `CMD` 显式传了 `--host 0.0.0.0`（否则端口映射不出来），
所以容器部署**一定要**配 `HOOK_TOKEN`。

## 通用注意事项

- **令牌走环境变量**：`HOOK_TOKEN` 会被自动读取。命令行参数会出现在 `ps` /
  任务管理器 / `docker inspect` 里，环境变量不会。
- **默认只绑 `127.0.0.1`**。要给别人用就显式 `--host 0.0.0.0`、配上 `--hook_token`，
  并挂在反向代理后面。理由见 [README 的安全一节](../README.md#安全)。
