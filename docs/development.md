# 开发

```
.
├── cmd/agent-session-query/   # 入口（实现在 internal/app）
├── cmd/healthcheck/           # 容器探活小程序
├── internal/app/              # 全部实现：run.go（参数与启动）、http.go（路由/认证/限流）、
│                              #   api.go（多源合并/匹配/缓存）、record.go、source*.go（各数据源）、
│                              #   search.go（全文搜索）、export.go（Markdown 导出）、
│                              #   mcp.go（stdio 上的 MCP server）、
│                              #   filecache.go（按 mtime 记忆化的文件头缓存）、
│                              #   hermes_sqlite.go、ui.go + ui/（go:embed 三栏页面）、
│                              #   console_{windows,other}.go（Windows 控制台代码页）
│                              #   测试与源文件一一对应（source_*_test.go 等）
├── docs/internals.md          # 解析细节 / 性能
├── .github/workflows/test.yml     # push / PR：三平台跑测试 + gofmt/vet/六平台交叉编译自检
├── .github/workflows/release.yml  # 打 tag：三平台跑测试，过了再交叉编译六平台发 Release
├── Dockerfile / docker-compose.yml
└── go.mod / go.sum            # 唯一依赖：纯 Go 的 SQLite 驱动
```

```bash
go test -race ./...   # 全部单测（不碰网络、不依赖本机装了什么）
gofmt -l .            # 格式检查
go vet ./...
```

发版：tag 的注释会被 [release.yml](../.github/workflows/release.yml) 拿去当 Release 正文，
所以要带 `--cleanup=verbatim`——否则 Markdown 的 `##` 标题会被 git 当成注释行剥掉：

```bash
git tag -a v0.3.0 --cleanup=verbatim -F notes.md
git push origin v0.3.0
```

新增一种数据源：实现 `SessionSource` 接口（`Mode` / `Location` / `Exists` / `List` / `Messages` / `Final`），
在 `buildSources()` 的 `factories` 里注册，再把模式名加进 `knownModes`——多源合并、匹配排序、
全文搜索、项目聚合、`source` 标记都是框架层统一处理的。会话不落在文件里的源（比如全 SQLite 的
Hermes）可以另外实现 `searchableSource` 自己接管搜索。
