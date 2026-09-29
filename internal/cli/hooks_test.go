package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

type hookWorld struct {
	home string
	main *tree.Tree
	a    *tree.Tree
	b    *tree.Tree
}

func newHookWorld(t *testing.T) hookWorld {
	t.Helper()
	root := t.TempDir()
	mainDir := filepath.Join(root, "app")
	os.MkdirAll(filepath.Join(mainDir, "web"), 0o755)
	os.WriteFile(filepath.Join(mainDir, "web", "x"), []byte("x"), 0o644)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", mainDir},
		{"-C", mainDir, "add", "."},
		{"-C", mainDir, "commit", "-q", "-m", "init"},
		{"-C", mainDir, "worktree", "add", "-q", "-b", "ticket-a", filepath.Join(root, "app-a")},
		{"-C", mainDir, "worktree", "add", "-q", "-b", "ticket-b", filepath.Join(root, "app-b")},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	w := hookWorld{home: t.TempDir()}
	t.Setenv("DIBS_HOME", w.home)
	t.Setenv("DIBS_CONFIG", filepath.Join(w.home, "config.yaml"))
	w.main, _ = tree.Resolve(mainDir)
	w.a, _ = tree.Resolve(filepath.Join(root, "app-a"))
	w.b, _ = tree.Resolve(filepath.Join(root, "app-b"))

	store, _ := state.Open(w.home)
	now := time.Now()
	store.Update(func(st *state.State) error {
		holder := &state.Holder{Tree: w.a.Path, Label: w.a.Label, Kind: state.KindSession, Since: now, Expires: now.Add(time.Hour)}
		st.Resources["browser"] = &state.Resource{Virtual: true, Holder: holder}
		copied := *holder
		st.Resources["web"] = &state.Resource{
			Holder: &copied, Serving: w.a.Path, ServingLabel: w.a.Label, Status: state.StatusReady,
			ContainerID: "c1", Repo: w.main.Common, Ports: []int{3000}, BaselineRoots: []string{w.main.Path},
		}
		return nil
	})
	return w
}

func hookInput(t *testing.T, fields map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(fields)
	return string(data)
}

func callHook(t *testing.T, command string, stdin string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run([]string{command}, strings.NewReader(stdin), &out, &errOut)
	return code, out.String()
}

func TestGuardHookIsSilentWhenAllowing(t *testing.T) {
	w := newHookWorld(t)
	input := hookInput(t, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "mcp__chrome-devtools__click", "cwd": w.a.Path,
	})
	code, out := callHook(t, "guard", input)
	if code != exitOK || out != "" {
		t.Fatalf("an allowed call must produce no output at all (so normal permission prompts still apply): %d %q", code, out)
	}
}

func TestGuardHookDenies(t *testing.T) {
	w := newHookWorld(t)
	input := hookInput(t, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": w.b.Path,
		"tool_input": map[string]any{"command": "docker restart web"},
	})
	code, out := callHook(t, "guard", input)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var decoded struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not JSON: %q", out)
	}
	if decoded.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(decoded.HookSpecificOutput.PermissionDecisionReason, "ticket-a") {
		t.Fatalf("unexpected decision: %+v", decoded)
	}
}

func TestGuardHookFailsOpen(t *testing.T) {
	newHookWorld(t)
	for _, stdin := range []string{"", "not json", `{"tool_name": 7}`} {
		if code, out := callHook(t, "guard", stdin); code != exitOK || out != "" {
			t.Fatalf("bad input %q must be ignored silently: %d %q", stdin, code, out)
		}
	}
}
