package cli

import (
	"context"
	"fmt"

	"github.com/itswl/agent-session-query/internal/skill"
)

// cmdSkill prints the agent-sessions skill, to be saved where an agent's CLI reads skills.
// It writes it nowhere itself: where a CLI keeps its files is the CLI's, and asq only reads
// them. On a terminal it says where that is.
func cmdSkill(ctx context.Context, args []string, io *streams) error {
	c := newCommand("skill", io.err)
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("asq skill takes no arguments: it prints the skill, to save where a CLI reads skills")
	}
	if c.json {
		return printJSON(io.out, map[string]any{"name": skill.Name, "skill": skill.Text})
	}
	fmt.Fprint(io.out, skill.Text)
	if isTerminal(io.out) {
		fmt.Fprintf(io.err, "\nasq: save it as SKILL.md in ~/.claude/skills/%[1]s for Claude Code, and in ~/.agents/skills/%[1]s for Codex, Gemini CLI, OpenCode and Pi:\n"+
			"  mkdir -p ~/.claude/skills/%[1]s ~/.agents/skills/%[1]s\n"+
			"  asq skill > ~/.claude/skills/%[1]s/SKILL.md\n"+
			"  asq skill > ~/.agents/skills/%[1]s/SKILL.md\n", skill.Name)
	}
	return nil
}
