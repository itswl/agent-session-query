// asq reads the session history of every agent CLI on a machine as one record.
package main

import (
	"os"

	// The zone database travels with the binary: a day in a report is the local
	// calendar's, and a container built from scratch has no /usr/share/zoneinfo for TZ
	// to name a zone in.
	_ "time/tzdata"

	"github.com/itswl/agent-session-query/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
