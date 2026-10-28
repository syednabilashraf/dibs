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

func additionalContext(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var decoded struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	return decoded.HookSpecificOutput.AdditionalContext
}

func logSwap(t *testing.T, w hookWorld, actor *tree.Tree) {
	t.Helper()
	store, _ := state.Open(w.home)
	store.Log(state.Event{Kind: "swap", Resource: "web", Tree: actor.Path, Label: actor.Label, Actor: actor.Path})
}

func bashHook(t *testing.T, event, session, toolUse, cwd, command string) string {
	t.Helper()
	return hookInput(t, map[string]any{
		"hook_event_name": event, "session_id": session, "tool_use_id": toolUse, "tool_name": "Bash", "cwd": cwd,
		"tool_input": map[string]any{"command": command, "timeout": 120000},
	})
}

func callPost(t *testing.T, stdin string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"guard", "--post"}, strings.NewReader(stdin), &out, &errOut); code != exitOK {
		t.Fatalf("post hook exit %d: %s", code, errOut.String())
	}
	return additionalContext(t, out.String())
}

func TestPostHookExplainsSwapDuringCommand(t *testing.T) {
	w := newHookWorld(t)
	command := "docker exec web sh -c 'cd /tmp/dibs-app-b && pytest'"
	if _, out := callHook(t, "guard", bashHook(t, "PreToolUse", "s-b", "toolu_1", w.b.Path, command)); out != "" {
		t.Fatalf("docker exec into a held container is allowed: %s", out)
	}
	time.Sleep(10 * time.Millisecond)
	logSwap(t, w, w.a)

	text := callPost(t, bashHook(t, "PostToolUse", "s-b", "toolu_1", w.b.Path, command))
	if !strings.Contains(text, "web was recreated") || !strings.Contains(text, "copy your code again") {
		t.Fatalf("b should be told its container was recreated under it:\n%s", text)
	}
	if !strings.Contains(text, "web is held by ticket-a") {
		t.Fatalf("b should also learn who holds web:\n%s", text)
	}

	again := callPost(t, bashHook(t, "PostToolUse", "s-b", "toolu_2", w.b.Path, "docker exec web ls"))
	if strings.Contains(again, "held by ticket-a") {
		t.Fatalf("the held note is said once per session:\n%s", again)
	}
}

func TestPostHookExplainsSwapBetweenCommands(t *testing.T) {
	w := newHookWorld(t)
	callHook(t, "guard", bashHook(t, "PreToolUse", "s-b", "toolu_1", w.b.Path, "docker cp ./web/. web:/tmp/dibs-app-b/"))
	callPost(t, bashHook(t, "PostToolUse", "s-b", "toolu_1", w.b.Path, "docker cp ./web/. web:/tmp/dibs-app-b/"))

	time.Sleep(10 * time.Millisecond)
	logSwap(t, w, w.a)
	time.Sleep(10 * time.Millisecond)

	callHook(t, "guard", bashHook(t, "PreToolUse", "s-b", "toolu_2", w.b.Path, "docker exec web pytest /tmp/dibs-app-b"))
	text := callPost(t, bashHook(t, "PostToolUse", "s-b", "toolu_2", w.b.Path, "docker exec web pytest /tmp/dibs-app-b"))
	if !strings.Contains(text, "web was recreated") {
		t.Fatalf("a swap between the copy and the test run must be reported:\n%s", text)
	}
}

func TestPostHookQuietForOwnSwap(t *testing.T) {
	w := newHookWorld(t)
	callHook(t, "guard", bashHook(t, "PreToolUse", "s-a", "toolu_1", w.a.Path, "docker exec web ls"))
	logSwap(t, w, w.a)
	if text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_1", w.a.Path, "docker exec web ls")); text != "" {
		t.Fatalf("a caused the swap, so there is nothing to explain:\n%s", text)
	}
}

func TestPostHookWarnsBeforeLeaseEnds(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		for _, name := range []string{"browser", "web"} {
			st.Resources[name].Holder.Expires = time.Now().Add(3 * time.Minute)
		}
		return nil
	})
	text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_9", w.a.Path, "ls"))
	if !strings.Contains(text, "dibs renew") {
		t.Fatalf("a should be warned before its lease ends:\n%s", text)
	}
	if text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_10", w.a.Path, "ls")); text != "" {
		t.Fatalf("the lease warning is given once:\n%s", text)
	}
}

func TestContextHook(t *testing.T) {
	w := newHookWorld(t)
	start := func(cwd string) string {
		var out bytes.Buffer
		Run([]string{"context"}, strings.NewReader(hookInput(t, map[string]any{"hook_event_name": "SessionStart", "source": "startup", "cwd": cwd})), &out, &out)
		return additionalContext(t, out.String())
	}
	text := start(w.b.Path)
	for _, want := range []string{"dibs take", "web: held by ticket-a", TmpDir(w.b.Path)} {
		if !strings.Contains(text, want) {
			t.Fatalf("primer should mention %q:\n%s", want, text)
		}
	}
	if text := start(t.TempDir()); text != "" {
		t.Fatalf("outside a managed repo the hook must stay silent:\n%s", text)
	}
}

func TestClaudeSetupWritesSkill(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if code := Run([]string{"claude-setup", "--skills-dir", dir}, strings.NewReader(""), &out, &out); code != exitOK {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "dibs", "SKILL.md"))
	if err != nil || !strings.HasPrefix(string(data), "---\nname: dibs\n") {
		t.Fatalf("skill not written: %v", err)
	}
	for _, want := range []string{`"PreToolUse"`, "NotebookEdit", " guard --post", " context", `"SubagentStart"`, "browser-mcp"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("setup output should include %q:\n%s", want, out.String())
		}
	}
}

func TestPostHookCatchesSwapDuringBackgroundCommand(t *testing.T) {
	w := newHookWorld(t)
	command := "docker exec web sh /tmp/dibs-app-b/test.sh"
	launch := hookInput(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s-bg", "tool_use_id": "toolu_bg", "tool_name": "Bash", "cwd": w.b.Path,
		"tool_input": map[string]any{"command": command, "run_in_background": true},
	})
	callPost(t, launch)
	time.Sleep(10 * time.Millisecond)
	logSwap(t, w, w.a)

	read := hookInput(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s-bg", "tool_use_id": "toolu_read", "tool_name": "Read", "cwd": w.b.Path,
		"tool_input": map[string]any{"file_path": "/tmp/tasks/bg.output"},
	})
	text := callPost(t, read)
	if !strings.Contains(text, "web was recreated") {
		t.Fatalf("reading a background command's output after a swap must explain it:\n%s", text)
	}
	if again := callPost(t, read); strings.Contains(again, "recreated") {
		t.Fatalf("each swap is explained once:\n%s", again)
	}
}

func TestPostHookTracksContainersBeforeDibsManagesThem(t *testing.T) {
	w := newHookWorld(t)
	command := "docker exec -e SLOW=60 qa-unmanaged sh /tmp/dibs-app-b/test.sh"
	launch := hookInput(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s-early", "tool_use_id": "toolu_early", "tool_name": "Bash", "cwd": w.b.Path,
		"tool_input": map[string]any{"command": command, "run_in_background": true},
	})
	callPost(t, launch)
	time.Sleep(10 * time.Millisecond)
	store, _ := state.Open(w.home)
	store.Log(state.Event{Kind: "swap", Resource: "qa-unmanaged", Tree: w.a.Path, Label: w.a.Label, Actor: w.a.Path})

	read := hookInput(t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s-early", "tool_use_id": "toolu_read2", "tool_name": "Read", "cwd": w.b.Path,
		"tool_input": map[string]any{"file_path": "/tmp/tasks/bg.output"},
	})
	if text := callPost(t, read); !strings.Contains(text, "qa-unmanaged was recreated") {
		t.Fatalf("a container first used before dibs managed it must still be tracked:\n%s", text)
	}
}

func TestContextHookPrimesSubagents(t *testing.T) {
	w := newHookWorld(t)
	var out bytes.Buffer
	input := hookInput(t, map[string]any{"hook_event_name": "SubagentStart", "agent_id": "a1", "agent_type": "general-purpose", "cwd": w.a.Path})
	Run([]string{"context"}, strings.NewReader(input), &out, &out)
	var decoded struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("not JSON: %q", out.String())
	}
	if decoded.HookSpecificOutput.HookEventName != "SubagentStart" {
		t.Fatalf("the event name must match the hook that fired: %q", decoded.HookSpecificOutput.HookEventName)
	}
	text := decoded.HookSpecificOutput.AdditionalContext
	if !strings.Contains(text, "You are a subagent") || !strings.Contains(text, "web: held by you") {
		t.Fatalf("a subagent in the holder's worktree should learn it shares the lease:\n%s", text)
	}
}

func queueWaiter(t *testing.T, w hookWorld, tr *tree.Tree, waited time.Duration, resources ...string) {
	t.Helper()
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		st.Queue = append(st.Queue, state.Waiter{ID: "w-" + tr.Label, Tree: tr.Path, Label: tr.Label, Resources: resources, PID: os.Getpid(), Since: time.Now().Add(-waited)})
		return nil
	})
}

func editSession(t *testing.T, w hookWorld, session string, edit func(map[string]any)) {
	t.Helper()
	path := filepath.Join(w.home, "sessions", session+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var mem map[string]any
	json.Unmarshal(data, &mem)
	edit(mem)
	data, _ = json.Marshal(mem)
	os.WriteFile(path, data, 0o644)
}

func TestPostHookTellsAnActiveHolderWhoIsWaiting(t *testing.T) {
	w := newHookWorld(t)
	queueWaiter(t, w, w.b, 25*time.Minute, "web", "browser")
	text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_1", w.a.Path, "ls"))
	for _, want := range []string{"1 session is waiting", "ticket-b (worktree app-b) for browser, web, waiting 25m", "dibs pass", "dibs hold", "no tool calls for 20m"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if again := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_2", w.a.Path, "ls")); again != "" {
		t.Fatalf("the same waiters are not announced on every call:\n%s", again)
	}
	editSession(t, w, "s-a", func(mem map[string]any) { mem["reminded"] = time.Now().Add(-11 * time.Minute) })
	if again := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_3", w.a.Path, "ls")); !strings.Contains(again, "waiting for what this worktree holds") {
		t.Fatalf("a reminder is due after ten minutes:\n%s", again)
	}
	if other := callPost(t, bashHook(t, "PostToolUse", "s-b", "toolu_4", w.b.Path, "ls")); strings.Contains(other, "waiting for what") {
		t.Fatalf("only the holder is told:\n%s", other)
	}
}

func TestPostHookRemindsAHolderOnHold(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		for _, name := range []string{"browser", "web"} {
			h := st.Resources[name].Holder
			h.HoldSince, h.Note, h.Expires = time.Now(), "the user is testing the toolbar", time.Now().Add(time.Hour)
		}
		return nil
	})
	queueWaiter(t, w, w.b, 5*time.Minute, "web")
	text := callPost(t, hookInput(t, map[string]any{"hook_event_name": "PostToolUse", "session_id": "s-a", "tool_name": "mcp__chrome-devtools__take_screenshot", "cwd": w.a.Path}))
	if !strings.Contains(text, "You put it on hold until") || !strings.Contains(text, "the user is testing the toolbar") {
		t.Fatalf("a holder on hold is reminded of its own hold, after any tool:\n%s", text)
	}
}

func TestPostHookExplainsALapsedLease(t *testing.T) {
	w := newHookWorld(t)
	if text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_1", w.a.Path, "ls")); text != "" {
		t.Fatalf("nothing to say yet:\n%s", text)
	}
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		st.Resources["web"].Holder.Expires = time.Now().Add(-time.Minute)
		st.Resources["browser"].Holder = &state.Holder{Tree: w.b.Path, Label: w.b.Label, Kind: state.KindSession, Since: time.Now(), Expires: time.Now().Add(30 * time.Minute)}
		return nil
	})
	store.Log(state.Event{Kind: "expire", Resource: "browser", Tree: w.a.Path, Label: w.a.Label})

	text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_2", w.a.Path, "ls"))
	for _, want := range []string{"no longer holds browser, web", "web: your lease on it expired at", "browser: your lease on it expired (noticed at", "ticket-b (worktree app-b) has held it since", "discard them", "dibs take browser web --wait"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if again := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_3", w.a.Path, "ls")); strings.Contains(again, "no longer holds") {
		t.Fatalf("a lapse is reported once:\n%s", again)
	}
}

func TestPostHookExplainsAnIdleRelease(t *testing.T) {
	w := newHookWorld(t)
	callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_1", w.a.Path, "ls"))
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		st.Resources["web"].Holder = nil
		return nil
	})
	store.Log(state.Event{Kind: "idle", Resource: "web", Tree: w.a.Path, Label: w.a.Label, Detail: "no tool calls since 04:54 while ticket-b waited"})
	text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_2", w.a.Path, "ls"))
	if !strings.Contains(text, "web: dibs released it at") || !strings.Contains(text, "made no tool calls since 04:54 while ticket-b waited") {
		t.Fatalf("an idle release is explained:\n%s", text)
	}
}

func TestPostHookQuietWhenTheWorktreePassedItself(t *testing.T) {
	w := newHookWorld(t)
	callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_1", w.a.Path, "ls"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"pass", "--tree", w.a.Path}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("pass: %d %s %s", code, out.String(), errOut.String())
	}
	if text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_2", w.a.Path, "ls")); text != "" {
		t.Fatalf("passing is not a lapse:\n%s", text)
	}
}

func TestGuardRecordsActivityForManagedRepos(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	callHook(t, "guard", bashHook(t, "PreToolUse", "s-b", "toolu_1", w.b.Path, "ls"))
	if store.LastActive(w.b.Path).IsZero() {
		t.Fatal("a tool call in a worktree of a managed repo is activity")
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", other).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	callHook(t, "guard", bashHook(t, "PreToolUse", "s-x", "toolu_2", other, "ls"))
	if entries, _ := os.ReadDir(filepath.Join(w.home, "activity")); len(entries) != 1 {
		t.Fatalf("repos dibs does not manage leave no trace: %d files", len(entries))
	}
}
