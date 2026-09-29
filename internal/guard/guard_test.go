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
	w.cfg.Guard.MutatePatterns = []string{`\bmycli\s+(restart|rebuild)\b.*\b{service}\b`, `\bmycli\s+mount\b`}
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
		{"git -C " + w.main.Path + " checkout main", w.main.Path, false, ""},
	}
	for _, c := range cases {
		caller, _ := tree.Resolve(c.cwd)
		d := Decide(w.cfg, st, caller, bash(c.command, c.cwd), now, alive)
		if d.Deny != c.deny || (c.reason != "" && !strings.Contains(d.Reason, c.reason)) {
			t.Errorf("%q from %s: deny = %v (%s)", c.command, filepath.Base(c.cwd), d.Deny, d.Reason)
		}
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
