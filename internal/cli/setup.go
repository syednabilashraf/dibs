package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/syednabilashraf/dibs/skill"
)

func runClaudeSetup(e *env, args []string) int {
	fs := newFlags(e, "claude-setup", "[--skills-dir DIR]")
	home, _ := os.UserHomeDir()
	dir := fs.String("skills-dir", filepath.Join(home, ".claude", "skills"), "where Claude Code looks for user skills")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	target := filepath.Join(*dir, "dibs", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return e.errorf("%v", err)
	}
	if err := os.WriteFile(target, []byte(skill.Markdown), 0o644); err != nil {
		return e.errorf("%v", err)
	}
	e.printf("dibs: wrote %s\n\n", target)

	binary, err := os.Executable()
	if err != nil {
		binary = "dibs"
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	hooks := map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "^Bash$|^mcp__.*",
			"hooks":   []any{map[string]any{"type": "command", "command": binary + " guard"}},
		}},
		"PostToolUse": []any{map[string]any{
			"matcher": "^Bash$",
			"hooks":   []any{map[string]any{"type": "command", "command": binary + " guard --post"}},
		}},
		"SessionStart": []any{map[string]any{
			"matcher": "startup|resume|clear|compact|fork",
			"hooks":   []any{map[string]any{"type": "command", "command": binary + " context"}},
		}},
	}}
	data, _ := json.MarshalIndent(hooks, "", "  ")
	e.printf("Merge these hooks into ~/.claude/settings.json (keep any hooks you already have):\n\n%s\n\n", data)
	e.printf("For a browser shared by every session, register chrome-devtools-mcp through dibs:\n\n")
	e.printf("  claude mcp add chrome-devtools -s user -- %s browser-mcp --no-usage-statistics\n\n", binary)
	e.printf("Hooks and MCP servers load at session start; restart running sessions afterwards.\n")
	return exitOK
}
