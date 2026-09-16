// agent-session-query：本地 Agent 会话查询 HTTP API 的入口。
// 实现在 internal/app（参数解析、启动横幅、数据源装配都在那里）。
package main

import (
	"os"

	"github.com/itswl/agent-session-query/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
