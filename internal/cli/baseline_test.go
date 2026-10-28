package cli

import (
	"bytes"
	"context"
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

type syncWorld struct {
	home, base, upstream string
	store                *state.Store
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w syncWorld) push(t *testing.T, file, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(w.upstream, file)), 0o755)
	os.WriteFile(filepath.Join(w.upstream, file), []byte(body), 0o644)
	gitIn(t, w.upstream, "add", ".")
	gitIn(t, w.upstream, "commit", "-q", "-m", file)
	gitIn(t, w.upstream, "push", "-q", "origin", "main")
}

func newSyncWorld(t *testing.T, extraConfig string) syncWorld {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	w := syncWorld{home: t.TempDir(), base: filepath.Join(root, "app"), upstream: filepath.Join(root, "upstream")}
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", remote)
	gitIn(t, root, "clone", "-q", remote, w.upstream)
	w.push(t, "migrations/001.sql", "create")
	gitIn(t, root, "clone", "-q", remote, w.base)
	base, _ := tree.Resolve(w.base)
	w.base = base.Path

	t.Setenv("DIBS_HOME", w.home)
	t.Setenv("DIBS_CONFIG", filepath.Join(w.home, "config.yaml"))
	os.WriteFile(filepath.Join(w.home, "config.yaml"), []byte("repos: ["+w.base+"]\n"+extraConfig), 0o644)
	w.store, _ = state.Open(w.home)
	w.store.Update(func(st *state.State) error {
		st.Resources["web"] = &state.Resource{Status: state.StatusReady, Repo: base.Common, BaselineRoots: []string{w.base}}
		return nil
	})
	return w
}

const webMigrationCheck = `containers:
  web:
    migrations:
      check: 'if [ -e migrations/002.sql ]; then echo "would apply 002.sql"; exit 1; fi'
      paths: [migrations]
`

func TestBaselineStatusAndSync(t *testing.T) {
	w := newSyncWorld(t, "")
	w.push(t, "app/main.txt", "v2")
	_, out := invoke("baseline", "--fetch", "--repo", w.base)
	for _, want := range []string{"baseline " + w.base + ": main at", "1 commit(s) behind origin/main", "serving it: web", "dibs baseline sync"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status should show %q:\n%s", want, out)
		}
	}
	code, out := invoke("baseline", "sync", "--repo", w.base)
	if code != exitOK || !strings.Contains(out, "fast-forwarded from") || !strings.Contains(out, "(1 commits)") {
		t.Fatalf("sync: %d %s", code, out)
	}
	if gitIn(t, w.base, "rev-parse", "HEAD") != gitIn(t, w.upstream, "rev-parse", "HEAD") {
		t.Fatal("the baseline should be at the remote branch")
	}
	if code, out := invoke("baseline", "sync", "--repo", w.base); code != exitOK || !strings.Contains(out, "up to date") {
		t.Fatalf("second sync: %d %s", code, out)
	}
	events, _ := w.store.Events(time.Now().Add(-time.Minute))
	if len(events) != 1 || events[0].Kind != "sync" || events[0].Tree != w.base {
		t.Fatalf("events = %+v", events)
	}
}

func TestBaselineSyncWaitsForLeasesOnTheBaseline(t *testing.T) {
	w := newSyncWorld(t, "")
	w.push(t, "app/main.txt", "v2")
	w.store.Update(func(st *state.State) error {
		st.Resources["web"].Holder = &state.Holder{Tree: "/w/a", Label: "ticket-a", Kind: state.KindSession, Since: time.Now(), Expires: time.Now().Add(time.Hour)}
		return nil
	})
	code, out := invoke("baseline", "sync", "--repo", w.base)
	if code != exitNo || !strings.Contains(out, "web runs the baseline under a lease held by ticket-a") {
		t.Fatalf("sync under a lease: %d %s", code, out)
	}
	if gitIn(t, w.base, "rev-parse", "HEAD") == gitIn(t, w.upstream, "rev-parse", "HEAD") {
		t.Fatal("a refused sync must leave the checkout alone")
	}
}

func TestBaselineSyncChecksMigrationsOnTheNewRevision(t *testing.T) {
	w := newSyncWorld(t, webMigrationCheck)
	w.push(t, "app/main.txt", "v2")
	if code, out := invoke("baseline", "sync", "--repo", w.base); code != exitOK {
		t.Fatalf("no new migrations, so the check passes: %d %s", code, out)
	}
	w.push(t, "migrations/002.sql", "alter")
	code, out := invoke("baseline", "sync", "--repo", w.base)
	if code != exitNo || !strings.Contains(out, "would apply 002.sql") || !strings.Contains(out, "--allow-migrations") {
		t.Fatalf("the check should stop the sync: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(w.base, "migrations", "002.sql")); !os.IsNotExist(err) {
		t.Fatal("the checkout must not move before the check passes")
	}
	if code, out := invoke("baseline", "sync", "--repo", w.base, "--allow-migrations"); code != exitOK {
		t.Fatalf("--allow-migrations overrides the check: %d %s", code, out)
	}
}

func TestBaselineSyncStashesOnlyWhenAsked(t *testing.T) {
	w := newSyncWorld(t, "")
	w.push(t, "app/main.txt", "v2")
	os.WriteFile(filepath.Join(w.base, "migrations", "001.sql"), []byte("local edit"), 0o644)
	code, out := invoke("baseline", "sync", "--repo", w.base)
	if code != exitNo || !strings.Contains(out, "local changes (migrations/001.sql)") || !strings.Contains(out, "--stash") {
		t.Fatalf("local changes: %d %s", code, out)
	}
	if code, out := invoke("baseline", "sync", "--repo", w.base, "--stash"); code != exitOK || !strings.Contains(out, "stashed local changes") {
		t.Fatalf("--stash: %d %s", code, out)
	}
	if !strings.Contains(gitIn(t, w.base, "stash", "list"), "dibs baseline sync") {
		t.Fatal("the stash should be named after dibs")
	}
}

func TestRestorePreflight(t *testing.T) {
	w := newSyncWorld(t, webMigrationCheck)
	w.push(t, "migrations/002.sql", "alter")
	gitIn(t, w.base, "fetch", "-q", "origin")
	os.WriteFile(filepath.Join(w.base, "migrations", "002.sql"), []byte("untracked, but present"), 0o644)

	cfg, _ := config.Load()
	var out bytes.Buffer
	a := &app{env: &env{stdout: &out, stderr: &out}, cfg: cfg, store: w.store}
	st, _ := w.store.Read()
	if a.restorePreflight(context.Background(), st, "web", w.base, map[string]bool{}) {
		t.Fatalf("the check fails on the baseline, so no restore:\n%s", out.String())
	}
	for _, want := range []string{"web: not restored", "would apply 002.sql", "1 commit(s) behind origin/main", "dibs baseline sync", "dibs restore web --allow-migrations"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}

	os.Remove(filepath.Join(w.base, "migrations", "002.sql"))
	out.Reset()
	if !a.restorePreflight(context.Background(), st, "web", w.base, map[string]bool{}) {
		t.Fatalf("the check passes now:\n%s", out.String())
	}

	delete(a.cfg.Containers, "web")
	out.Reset()
	told := map[string]bool{}
	for i := 0; i < 2; i++ {
		if !a.restorePreflight(context.Background(), st, "web", w.base, told) {
			t.Fatal("without a check a restore goes ahead")
		}
	}
	if strings.Count(out.String(), "behind origin/main") != 1 {
		t.Fatalf("without a check, being behind is a note, given once:\n%s", out.String())
	}
}

func invoke(args ...string) (int, string) {
	var out bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &out)
	return code, out.String()
}
