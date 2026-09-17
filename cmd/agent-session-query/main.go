// agent-session-query: entry point for the local agent session query API.
// The implementation lives in internal/app (flag parsing, startup banner,
// data source wiring).
package main

import (
	"os"

	"github.com/itswl/agent-session-query/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
