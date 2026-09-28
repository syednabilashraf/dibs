package tree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	main     string
	worktree string
	foreign  string
}

func setup(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "app")
	write(t, filepath.Join(main, "web", "index.js"), "main")
	write(t, filepath.Join(main, "config.json"), "{}")
	git(t, root, "init", "-q", "-b", "main", main)
	git(t, main, "add", ".")
	git(t, main, "commit", "-q", "-m", "init")
	write(t, filepath.Join(main, ".data", "db"), "local only")

	worktree := filepath.Join(root, "app-feature")
	git(t, main, "worktree", "add", "-q", "-b", "feature", worktree)

	foreign := filepath.Join(root, "other")
	write(t, filepath.Join(foreign, "x"), "x")
	git(t, root, "init", "-q", foreign)
	return fixture{main: main, worktree: worktree, foreign: foreign}
}

func TestResolveMainAndWorktree(t *testing.T) {
	f := setup(t)
	main, err := Resolve(filepath.Join(f.main, "web"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := Resolve(f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !main.Main || wt.Main {
		t.Fatalf("main flags wrong: main=%v wt=%v", main.Main, wt.Main)
	}
	if main.Path != Clean(f.main) || wt.Path != Clean(f.worktree) {
		t.Fatalf("paths: %s %s", main.Path, wt.Path)
	}
	if !main.SameRepo(wt) {
		t.Fatalf("worktrees of one repo must share a common dir: %s vs %s", main.Common, wt.Common)
	}
	if main.Branch != "main" || wt.Branch != "feature" || wt.Label != "feature" {
		t.Fatalf("branches: %q %q", main.Branch, wt.Branch)
	}
	foreign, err := Resolve(f.foreign)
	if err != nil {
		t.Fatal(err)
	}
	if foreign.SameRepo(main) {
		t.Fatal("unrelated repositories must not match")
	}
}

func TestResolveOutsideGit(t *testing.T) {
	if _, err := Resolve(t.TempDir()); err == nil {
		t.Fatal("a plain directory is not a worktree")
	}
}

func TestMap(t *testing.T) {
	f := setup(t)
	target, err := Resolve(f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMapper()

	dir := m.Map(filepath.Join(f.main, "web"), target)
	if !dir.Changed() || dir.Result != filepath.Join(target.Path, "web") {
		t.Fatalf("directory mount not mapped: %+v", dir)
	}
	if dir.Root != Clean(f.main) {
		t.Fatalf("root = %s", dir.Root)
	}

	file := m.Map(filepath.Join(f.main, "config.json"), target)
	if !file.Changed() || file.Result != filepath.Join(target.Path, "config.json") {
		t.Fatalf("file mount not mapped: %+v", file)
	}

	missing := m.Map(filepath.Join(f.main, ".data"), target)
	if missing.Mapped || missing.Result != filepath.Join(f.main, ".data") || missing.Reason == "" {
		t.Fatalf("untracked dir absent from the worktree must be kept: %+v", missing)
	}

	foreign := m.Map(filepath.Join(f.foreign, "x"), target)
	if foreign.Mapped || foreign.Result != filepath.Join(f.foreign, "x") {
		t.Fatalf("foreign repo must be untouched: %+v", foreign)
	}

	nowhere := m.Map("/definitely/not/here", target)
	if nowhere.Mapped {
		t.Fatalf("nonexistent source must be untouched: %+v", nowhere)
	}
}

func TestMapBackToMainIsUnchanged(t *testing.T) {
	f := setup(t)
	main, _ := Resolve(f.main)
	m := NewMapper()
	source := filepath.Join(f.main, "web")
	change := m.Map(source, main)
	if !change.Mapped || change.Changed() || change.Result != source {
		t.Fatalf("mapping onto the source's own tree should be a no-op: %+v", change)
	}
}

func TestMapFromWorktreeToWorktree(t *testing.T) {
	f := setup(t)
	main, _ := Resolve(f.main)
	m := NewMapper()
	change := m.Map(filepath.Join(f.worktree, "web"), main)
	if !change.Changed() || change.Result != filepath.Join(main.Path, "web") {
		t.Fatalf("a baseline already on a worktree should still map: %+v", change)
	}
}
