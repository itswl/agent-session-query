package cli

import (
	"context"
	"fmt"
	"runtime"
	"strings"

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
		fmt.Fprint(io.err, saveLines(runtime.GOOS))
	}
	return nil
}

// saveLines says where to save the skill, and the lines that do it in the system's shell:
// on Windows cmd's, whose > writes what asq prints as it is, where Windows PowerShell's
// writes it again as UTF-16, which a CLI does not read as a skill.
func saveLines(goos string) string {
	claude, agents := "~/.claude/skills/"+skill.Name, "~/.agents/skills/"+skill.Name
	lines := []string{"mkdir -p " + claude + " " + agents, "asq skill > " + claude + "/SKILL.md", "asq skill > " + agents + "/SKILL.md"}
	shell := ""
	if goos == "windows" {
		claude, agents = `%USERPROFILE%\.claude\skills\`+skill.Name, `%USERPROFILE%\.agents\skills\`+skill.Name
		lines = []string{`mkdir "` + claude + `" "` + agents + `"`, `asq skill > "` + claude + `\SKILL.md"`, `asq skill > "` + agents + `\SKILL.md"`}
		shell = ", in cmd"
	}
	return fmt.Sprintf("\nasq: save it as SKILL.md in %s for Claude Code, and in %s for Codex, Gemini CLI, OpenCode and Pi%s:\n  %s\n",
		claude, agents, shell, strings.Join(lines, "\n  "))
}
