package baseline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/syednabilashraf/dibs/internal/config"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
	os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", file)
}

type repos struct{ remote, base, upstream string }

func newRepos(t *testing.T) repos {
	t.Helper()
	root := t.TempDir()
	r := repos{remote: filepath.Join(root, "remote.git"), base: filepath.Join(root, "base"), upstream: filepath.Join(root, "upstream")}
	git(t, root, "init", "-q", "--bare", "-b", "main", r.remote)
	git(t, root, "clone", "-q", r.remote, r.upstream)
	commit(t, r.upstream, "app/main.txt", "v1")
	git(t, r.upstream, "push", "-q", "origin", "main")
	git(t, root, "clone", "-q", r.remote, r.base)
	return r
}

func TestInspectAndFastForward(t *testing.T) {
	r := newRepos(t)
	ctx := context.Background()
	commit(t, r.upstream, "migrations/002.sql", "alter")
	git(t, r.upstream, "push", "-q", "origin", "main")

	st, err := Inspect(ctx, r.base, config.Baseline{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" || st.TargetRef() != "origin/main" || st.Behind != 0 {
		t.Fatalf("before fetching: %+v", st)
	}
	if err := Fetch(ctx, r.base, st.Remote, st.Target); err != nil {
		t.Fatal(err)
	}
	if st, _ = Inspect(ctx, r.base, config.Baseline{}); st.Behind != 1 || st.Ahead != 0 || st.Fetched.IsZero() {
		t.Fatalf("after fetching: %+v", st)
	}
	if obstacles := st.Obstacles(false); len(obstacles) != 0 {
		t.Fatalf("a clean checkout on main can fast-forward: %v", obstacles)
	}

	os.WriteFile(filepath.Join(r.base, "app", "main.txt"), []byte("local edit"), 0o644)
	st, _ = Inspect(ctx, r.base, config.Baseline{})
	if !reflect.DeepEqual(st.Dirty, []string{"app/main.txt"}) || len(st.Obstacles(false)) != 1 || len(st.Obstacles(true)) != 0 {
		t.Fatalf("local changes block unless stashing: %+v %v", st.Dirty, st.Obstacles(false))
	}
	if err := Stash(ctx, r.base, "dibs test"); err != nil {
		t.Fatal(err)
	}
	if err := FastForward(ctx, r.base, st.TargetRef()); err != nil {
		t.Fatal(err)
	}
	if st, _ = Inspect(ctx, r.base, config.Baseline{}); st.Behind != 0 || len(st.Dirty) != 0 {
		t.Fatalf("after the fast-forward: %+v", st)
	}
	if !strings.Contains(git(t, r.base, "stash", "list"), "dibs test") {
		t.Fatal("stashed changes are kept")
	}
}

func TestObstacles(t *testing.T) {
	r := newRepos(t)
	ctx := context.Background()
	commit(t, r.base, "app/local.txt", "mine")
	git(t, r.upstream, "commit", "-q", "--allow-empty", "-m", "upstream")
	git(t, r.upstream, "push", "-q", "origin", "main")
	Fetch(ctx, r.base, "origin", "main")
	st, _ := Inspect(ctx, r.base, config.Baseline{})
	if got := st.Obstacles(false); len(got) != 1 || !strings.Contains(got[0], "1 commit(s) that are not on origin/main") {
		t.Fatalf("local commits: %v", got)
	}

	git(t, r.base, "switch", "-q", "--detach", "HEAD")
	st, _ = Inspect(ctx, r.base, config.Baseline{})
	if got := st.Obstacles(false); !strings.Contains(strings.Join(got, ";"), "detached") {
		t.Fatalf("detached: %v", got)
	}
	git(t, r.base, "switch", "-q", "-c", "feature")
	st, _ = Inspect(ctx, r.base, config.Baseline{Branch: "main"})
	if got := st.Obstacles(false); !strings.Contains(strings.Join(got, ";"), "on branch feature, not main") {
		t.Fatalf("other branch: %v", got)
	}
}

func TestExportWritesOnlyTheRequestedPaths(t *testing.T) {
	r := newRepos(t)
	ctx := context.Background()
	commit(t, r.upstream, "migrations/002.sql", "alter")
	git(t, r.upstream, "push", "-q", "origin", "main")
	Fetch(ctx, r.base, "origin", "main")
	rev, err := Resolve(ctx, r.base, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Export(ctx, r.base, rev, []string{"migrations"}, dest); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "migrations", "002.sql")); err != nil || string(data) != "alter" {
		t.Fatalf("exported file: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "app")); !os.IsNotExist(err) {
		t.Fatal("paths that were not asked for stay out")
	}
	if _, err := os.Stat(filepath.Join(r.base, "migrations")); !os.IsNotExist(err) {
		t.Fatal("exporting must not touch the checkout")
	}
	if err := Export(ctx, r.base, rev, []string{"missing"}, t.TempDir()); err == nil {
		t.Fatal("a path the revision lacks is an error")
	}
}
