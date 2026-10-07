package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func alive(pid int) bool { return pid == 4242 }

type world struct {
	main, a, b *tree.Tree
	cfg        *config.Config
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newWorld(t *testing.T) world {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "app")
	os.MkdirAll(filepath.Join(main, "web"), 0o755)
	os.WriteFile(filepath.Join(main, "web", "x"), []byte("x"), 0o644)
	gitRun(t, root, "init", "-q", "-b", "main", main)
	gitRun(t, main, "add", ".")
	gitRun(t, main, "commit", "-q", "-m", "init")
	gitRun(t, main, "worktree", "add", "-q", "-b", "ticket-a", filepath.Join(root, "app-a"))
	gitRun(t, main, "worktree", "add", "-q", "-b", "ticket-b", filepath.Join(root, "app-b"))
	w := world{cfg: config.Default()}
	w.main, _ = tree.Resolve(main)
	w.a, _ = tree.Resolve(filepath.Join(root, "app-a"))
	w.b, _ = tree.Resolve(filepath.Join(root, "app-b"))
	w.cfg.Guard.MutatePatterns = []string{`\bmycli\s+(restart|rebuild)\b.*\b{service}\b`, `\bmycli\s+mount\b`, `\bmycli\s+down(\s+-{1,2}\w+)*\s*$`}
	return w
}

func (w world) session(tr *tree.Tree) *state.Holder {
	return &state.Holder{Tree: tr.Path, Label: tr.Label, Kind: state.KindSession, Since: now.Add(-5 * time.Minute), Expires: now.Add(25 * time.Minute)}
}

func (w world) aHoldsEverything() *state.State {
	st := state.New()
	st.Resources["browser"] = &state.Resource{Virtual: true, Holder: w.session(w.a)}
	st.Resources["web"] = &state.Resource{
		Holder: w.session(w.a), Serving: w.a.Path, ServingLabel: w.a.Label, Status: state.StatusReady,
		Service: "web-svc", Project: "stack", Ports: []int{3000}, BaselineRoots: []string{w.main.Path},
	}
	st.Resources["api"] = &state.Resource{Serving: "", Status: state.StatusReady, Service: "api", Project: "stack", Ports: []int{8080}, BaselineRoots: []string{w.main.Path}}
	return st
}

func bash(command, cwd string) Input {
	in := Input{ToolName: "Bash", CWD: cwd}
	in.ToolInput.Command = command
	return in
}

func browserCall() Input { return Input{ToolName: "mcp__chrome-devtools__navigate_page"} }

func expect(t *testing.T, d Decision, wantDeny bool, wantIn string) {
	t.Helper()
	if d.Deny != wantDeny {
		t.Fatalf("deny = %v, want %v (reason %q)", d.Deny, wantDeny, d.Reason)
	}
	if wantIn != "" && !strings.Contains(d.Reason, wantIn) {
		t.Fatalf("reason %q should mention %q", d.Reason, wantIn)
	}
}

func TestBrowserRules(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()

	expect(t, Decide(w.cfg, st, w.a, browserCall(), now, alive), false, "")
	expect(t, Decide(w.cfg, st, w.b, browserCall(), now, alive), true, "browser is in use by ticket-a")
	expect(t, Decide(w.cfg, st, nil, browserCall(), now, alive), true, "browser is in use")

	st.Resources["browser"].Holder = nil
	expect(t, Decide(w.cfg, st, w.b, browserCall(), now, alive), true, "web is held by ticket-a")

	st.Resources["web"].Holder = nil
	expect(t, Decide(w.cfg, st, w.b, browserCall(), now, alive), true, "still running ticket-a")
	expect(t, Decide(w.cfg, st, w.a, browserCall(), now, alive), false, "")

	st.Resources["web"].Serving = ""
	expect(t, Decide(w.cfg, st, w.b, browserCall(), now, alive), false, "")

	notBrowser := Input{ToolName: "mcp__notes__search"}
	expect(t, Decide(w.cfg, w.aHoldsEverything(), w.b, notBrowser, now, alive), false, "")
}

func TestBrowserWhileMySwapIsInFlight(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	web := st.Resources["web"]

	web.Status, web.Intent = state.StatusSwapping, &state.Intent{PID: 4242}
	expect(t, Decide(w.cfg, st, w.a, browserCall(), now, alive), true, "still being switched")

	web.Intent.PID = 1
	expect(t, Decide(w.cfg, st, w.a, browserCall(), now, alive), true, "was interrupted")

	web.Status, web.Intent = state.StatusFailed, nil
	expect(t, Decide(w.cfg, st, w.a, browserCall(), now, alive), true, "failed to start")

	web.Status = state.StatusReady
	web.Serving = w.main.Path
	web.Holder.NoSwap = true
	expect(t, Decide(w.cfg, st, w.a, browserCall(), now, alive), false, "")
}

func TestExpiredLeaseIsNotAHolder(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	later := now.Add(time.Hour)
	st.Resources["web"].Serving = ""
	expect(t, Decide(w.cfg, st, w.b, browserCall(), later, alive), false, "")
}

func TestBashMutations(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	cases := []struct {
		command string
		deny    bool
	}{
		{"docker restart web", true},
		{"docker container stop web", true},
		{"docker rm -f web", true},
		{"docker restart webhook", false},
		{"docker restart api", false},
		{"docker exec web pytest -q", false},
		{"docker exec -w /tmp/dibs-x web python -m pytest", false},
		{"docker cp ./src/. web:/tmp/dibs-app-b-1234/", false},
		{"docker logs web --tail 50", false},
		{"docker compose restart web-svc", true},
		{"docker compose -f localdev/compose.yml up -d --force-recreate web-svc", true},
		{"docker compose up -d api", false},
		{"docker compose up -d", true},
		{"docker compose down", true},
		{"docker compose -p otherproject up -d", false},
		{"docker compose exec web-svc restart-worker", false},
		{"docker compose logs -f web-svc", false},
		{"docker-compose stop web-svc", true},
		{"mycli rebuild web-svc", true},
		{"mycli rebuild api", false},
		{"mycli mount ../app-b web-svc", true},
		{"mycli down --volumes", true},
		{"ls -la && docker restart web", true},
		{"dibs take ui --wait", false},
		{"dibs take web --wait && docker restart web", true},
		{"~/go/bin/dibs status", false},
	}
	for _, c := range cases {
		d := Decide(w.cfg, st, w.b, bash(c.command, w.b.Path), now, alive)
		if d.Deny != c.deny {
			t.Errorf("%q: deny = %v, want %v (%s)", c.command, d.Deny, c.deny, d.Reason)
		}
		if d.Deny && !strings.Contains(d.Reason, "dibs take") {
			t.Errorf("%q: deny reason should say how to proceed: %s", c.command, d.Reason)
		}
	}
	for _, c := range cases {
		if d := Decide(w.cfg, st, w.a, bash(c.command, w.a.Path), now, alive); d.Deny {
			t.Errorf("the holder must never be blocked: %q -> %s", c.command, d.Reason)
		}
	}
}

func TestRegexBracesAreNotPlaceholders(t *testing.T) {
	w := newWorld(t)
	d := Decide(w.cfg, w.aHoldsEverything(), w.b, bash("mycli down", w.b.Path), now, alive)
	if !d.Deny || !strings.Contains(d.Reason, "this command can restart or re-point shared containers") {
		t.Fatalf("a pattern with regex braces but no placeholder applies to the whole stack: %s", d.Reason)
	}
}

func TestBashUseOfSomeoneElsesCode(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	cases := []struct {
		command string
		deny    bool
	}{
		{"curl -s localhost:3000/health", true},
		{"curl -s http://127.0.0.1:3000/", true},
		{"curl -s localhost:30000/", false},
		{"curl -s localhost:8080/", false},
		{"npx playwright test tests/login.spec.ts", true},
		{"pnpm test:e2e", true},
		{"grep -r playwright package.json", false},
		{"go test ./...", false},
	}
	for _, c := range cases {
		d := Decide(w.cfg, st, w.b, bash(c.command, w.b.Path), now, alive)
		if d.Deny != c.deny {
			t.Errorf("%q: deny = %v, want %v (%s)", c.command, d.Deny, c.deny, d.Reason)
		}
	}
	if d := Decide(w.cfg, st, w.a, bash("curl localhost:3000", w.a.Path), now, alive); d.Deny {
		t.Fatalf("holder can hit its own port: %s", d.Reason)
	}
	st.Resources["web"].Holder = nil
	expect(t, Decide(w.cfg, st, w.b, bash("curl localhost:3000", w.b.Path), now, alive), true, "serving ticket-a")
	st.Resources["web"].Serving = ""
	expect(t, Decide(w.cfg, st, w.b, bash("curl localhost:3000", w.b.Path), now, alive), false, "")
}

func TestGitInOtherWorktrees(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	cases := []struct {
		command string
		cwd     string
		deny    bool
		reason  string
	}{
		{"git -C " + w.main.Path + " checkout -b hotfix", w.b.Path, true, "baseline code"},
		{"cd " + w.main.Path + " && git pull", w.b.Path, true, "baseline code"},
		{"git -C " + w.a.Path + " stash", w.b.Path, true, "serving"},
		{"git -C ../app-a reset --hard", w.b.Path, true, "serving"},
		{"git checkout -b mine", w.b.Path, false, ""},
		{"git -C " + w.b.Path + " switch main", w.b.Path, false, ""},
		{"git -C " + w.main.Path + " log --oneline", w.b.Path, false, ""},
		{"git -C " + w.main.Path + " checkout main", w.main.Path, true, "is the baseline checkout"},
	}
	for _, c := range cases {
		caller, _ := tree.Resolve(c.cwd)
		d := Decide(w.cfg, st, caller, bash(c.command, c.cwd), now, alive)
		if d.Deny != c.deny || (c.reason != "" && !strings.Contains(d.Reason, c.reason)) {
			t.Errorf("%q from %s: deny = %v (%s)", c.command, filepath.Base(c.cwd), d.Deny, d.Reason)
		}
	}
}

func editCall(tool, path, cwd string) Input {
	in := Input{ToolName: tool, CWD: cwd}
	if tool == "NotebookEdit" {
		in.ToolInput.NotebookPath = path
	} else {
		in.ToolInput.FilePath = path
	}
	return in
}

func TestBaselineEditsAreBlocked(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	nested := filepath.Join(w.main.Path, ".claude", "worktrees", "nested")
	gitRun(t, w.main.Path, "worktree", "add", "-q", "-b", "nested", nested)

	cases := []struct {
		name   string
		in     Input
		caller *tree.Tree
		deny   bool
	}{
		{"edit in baseline from baseline", editCall("Edit", filepath.Join(w.main.Path, "web", "x"), w.main.Path), w.main, true},
		{"new file in baseline", editCall("Write", filepath.Join(w.main.Path, "web", "new", "deep.ts"), w.main.Path), w.main, true},
		{"relative path in baseline", editCall("Write", "web/x", w.main.Path), w.main, true},
		{"edit in baseline from another worktree", editCall("MultiEdit", filepath.Join(w.main.Path, "web", "x"), w.b.Path), w.b, true},
		{"notebook in baseline", editCall("NotebookEdit", filepath.Join(w.main.Path, "nb.ipynb"), w.main.Path), w.main, true},
		{"edit in own worktree", editCall("Edit", filepath.Join(w.b.Path, "web", "x"), w.b.Path), w.b, false},
		{"edit in nested worktree under baseline", editCall("Write", filepath.Join(nested, "web", "x"), w.main.Path), w.main, false},
		{"edit outside any repo", editCall("Write", filepath.Join(t.TempDir(), "notes.md"), w.main.Path), w.main, false},
	}
	for _, c := range cases {
		d := Decide(w.cfg, st, c.caller, c.in, now, alive)
		if d.Deny != c.deny {
			t.Errorf("%s: deny = %v (%s)", c.name, d.Deny, d.Reason)
		}
		if d.Deny && !strings.Contains(d.Reason, "EnterWorktree") {
			t.Errorf("%s: the reason should say how to get a worktree: %s", c.name, d.Reason)
		}
	}
}

func TestBaselineGitFromInside(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	cases := []struct {
		command string
		deny    bool
	}{
		{"git checkout -b feat/pins origin/main", true},
		{"git pull", true},
		{"git stash", true},
		{"git -c core.pager=cat reset --hard HEAD~1", true},
		{"git status && git switch main", true},
		{"git stash list", false},
		{"git status", false},
		{"git fetch origin main", false},
		{"git log --oneline -5", false},
		{"git worktree add ../app-pins -b feat/pins origin/main", false},
		{"git worktree add ../app-pins -b feat/pins && cd ../app-pins && git checkout -b other", false},
		{"cd " + w.b.Path + " && git checkout -b feat", false},
	}
	for _, c := range cases {
		d := Decide(w.cfg, st, w.main, bash(c.command, w.main.Path), now, alive)
		if d.Deny != c.deny {
			t.Errorf("%q from the baseline: deny = %v (%s)", c.command, d.Deny, d.Reason)
		}
	}
	if d := Decide(w.cfg, st, w.b, bash("(cd "+w.main.Path+" && git pull)", w.b.Path), now, alive); !d.Deny {
		t.Fatal("pulling in the baseline from another worktree must still be denied")
	}
}

func TestBaselineProtectionCanBeDisabled(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	off := false
	w.cfg.Guard.ProtectBaseline = &off
	if d := Decide(w.cfg, st, w.main, editCall("Edit", filepath.Join(w.main.Path, "web", "x"), w.main.Path), now, alive); d.Deny {
		t.Fatalf("edits are allowed when protection is off: %s", d.Reason)
	}
	if d := Decide(w.cfg, st, w.main, bash("git checkout -b feat", w.main.Path), now, alive); d.Deny {
		t.Fatalf("own-checkout git is allowed when protection is off: %s", d.Reason)
	}
	if d := Decide(w.cfg, st, w.b, bash("git -C "+w.main.Path+" checkout x", w.b.Path), now, alive); !d.Deny {
		t.Fatal("changing the baseline from another worktree is always denied")
	}
}

func TestConfiguredRepoIsBaselineBeforeAnythingIsManaged(t *testing.T) {
	w := newWorld(t)
	w.cfg.Repos = []string{w.main.Path}
	d := Decide(w.cfg, state.New(), w.main, editCall("Edit", filepath.Join(w.main.Path, "web", "x"), w.main.Path), now, alive)
	if !d.Deny || !strings.Contains(d.Reason, "the shared containers") {
		t.Fatalf("a configured repo is the baseline even with nothing managed: %v %s", d.Deny, d.Reason)
	}
	if d := Decide(w.cfg, state.New(), w.b, editCall("Edit", filepath.Join(w.b.Path, "web", "x"), w.b.Path), now, alive); d.Deny {
		t.Fatalf("worktrees of a configured repo are not the baseline: %s", d.Reason)
	}
}

func TestTakeMustBeForTheSessionsOwnWorktree(t *testing.T) {
	w := newWorld(t)
	st := state.New()
	cases := []struct {
		command string
		deny    bool
	}{
		{"dibs take ui --wait", false},
		{"cd web && dibs take ui --wait", false},
		{"cd " + w.b.Path + " && dibs take web", false},
		{"cd ../app-a && dibs take ui --wait", true},
		{"cd " + w.a.Path + " && ~/go/bin/dibs take web --wait 2>&1 | tail -5", true},
		{"dibs take ui --tree ../app-a", true},
		{"dibs take --tree=" + w.a.Path + " web", true},
		{"dibs grab web --tree " + w.a.Path + " --note x", true},
		{"cd ../app-a && dibs status", false},
		{"cd ../app-a && dibs check", false},
	}
	for _, c := range cases {
		d := Decide(w.cfg, st, w.b, bash(c.command, w.b.Path), now, alive)
		if d.Deny != c.deny {
			t.Errorf("%q: deny = %v (%s)", c.command, d.Deny, d.Reason)
		}
		if d.Deny && (!strings.Contains(d.Reason, "EnterWorktree") || !strings.Contains(d.Reason, w.a.Path)) {
			t.Errorf("%q: the reason should name the worktree and how to move into it: %s", c.command, d.Reason)
		}
	}
	if d := Decide(w.cfg, st, nil, bash("cd "+w.a.Path+" && dibs take web", t.TempDir()), now, alive); !d.Deny || !strings.Contains(d.Reason, "outside any git worktree") {
		t.Fatalf("a session outside any worktree must not take for one: %s", d.Reason)
	}
}

func TestQuotedAndHeredocTextIsNotACommand(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	takeElsewhere := "cd ../app-a && " + "dibs take ui"
	body := "cat > /tmp/pr.md <<'EOF'\nThe session ran `" + takeElsewhere + " --wait` and then\ngit -C " + w.main.Path + " checkout main && docker restart web\nEOF\ngh pr create --body-file /tmp/pr.md"
	cases := []struct {
		command string
		deny    bool
	}{
		{body, false},
		{"git commit -m \"explain " + takeElsewhere + "\"", false},
		{"echo 'cd ../app-a; " + "dibs take ui --tree ../app-a'", false},
		{"cat <<-EOT\n\t" + "dibs take ui --tree ../app-a\n\tEOT", false},
		{takeElsewhere, true},
		{"cat <<'EOF' > x\nhello\nEOF\n" + takeElsewhere, true},
	}
	for _, c := range cases {
		d := Decide(w.cfg, st, w.b, bash(c.command, w.b.Path), now, alive)
		if d.Deny != c.deny {
			t.Errorf("%q: deny = %v (%s)", c.command, d.Deny, d.Reason)
		}
	}
	if got := StripDibs("cat <<EOF\ndocker restart web\nEOF\nls"); strings.Contains(got, "docker restart") {
		t.Fatalf("heredoc bodies are data, not commands: %q", got)
	}
}

func TestNothingManagedAllowsEverything(t *testing.T) {
	w := newWorld(t)
	st := state.New()
	for _, in := range []Input{browserCall(), bash("docker restart web", w.b.Path), bash("git -C "+w.main.Path+" checkout x", w.b.Path)} {
		if d := Decide(w.cfg, st, w.b, in, now, alive); d.Deny {
			t.Fatalf("with nothing managed the guard must stay out of the way: %s", d.Reason)
		}
	}
}

func TestStripDibs(t *testing.T) {
	cases := map[string]string{
		"dibs take ui --wait":               "",
		"dibs check && npx playwright test": " npx playwright test",
		"echo hi":                           "echo hi",
		"/usr/local/bin/dibs pass; ls":      " ls",
	}
	for in, want := range cases {
		if got := strings.TrimSpace(StripDibs(in)); got != strings.TrimSpace(want) {
			t.Errorf("StripDibs(%q) = %q, want %q", in, got, want)
		}
	}
}
