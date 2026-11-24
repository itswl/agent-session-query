// Package skill is the agent-sessions skill: what tells an agent when to reach for asq and
// how to move through it — the MCP tools, or the command line where there is no server. It
// is built into the binary, and asq skill prints it, so that any agent whose CLI reads
// skills can be given it wherever asq is installed. In this repository
// .agents/skills/agent-sessions/SKILL.md is the same text, for the agents that read a
// repository's .agents/skills; a test keeps the two one.
package skill

import _ "embed"

// Name is the skill's name: the directory a CLI reads it from, beneath its skills directory.
const Name = "agent-sessions"

// Text is SKILL.md.
//
//go:embed SKILL.md
var Text string
