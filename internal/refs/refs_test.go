package refs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syednabilashraf/dibs/internal/tree"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, body string) string {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "web", "who"), []byte(body+"\n"), 0o644)
	gitT(t, dir, "commit", "-qam", body)
	return gitT(t, dir, "rev-parse", "HEAD")
}

func newOwner(t *testing.T) (*tree.Tree, string, string) {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "app")
	os.MkdirAll(filepath.Join(main, "web"), 0o755)
	os.WriteFile(filepath.Join(main, "web", "who"), []byte("v1\n"), 0o644)
	gitT(t, root, "init", "-q", "-b", "main", main)
	gitT(t, main, "add", ".")
	gitT(t, main, "commit", "-qm", "v1")
	first := gitT(t, main, "rev-parse", "HEAD")
	second := commit(t, main, "v2")
	gitT(t, main, "worktree", "add", "-q", "-b", "ticket", filepath.Join(root, "app-ticket"))
	owner, _ := tree.Resolve(filepath.Join(root, "app-ticket"))
	return owner, first, second
}

func read(t *testing.T, dir string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "web", "who"))
	return strings.TrimSpace(string(data))
}

func TestPrepareChecksOutAndReusesOneDirectory(t *testing.T) {
	owner, first, second := newOwner(t)
	home := t.TempDir()
	ctx := context.Background()

	checkout, err := Prepare(ctx, home, owner, first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, checkout.Path) != "v1" || checkout.Common != owner.Common || !strings.HasPrefix(checkout.Label, first+" at ") {
		t.Fatalf("checkout %+v serves %q", checkout, read(t, checkout.Path))
	}
	os.WriteFile(filepath.Join(checkout.Path, "stray.txt"), []byte("x"), 0o644)

	again, err := Prepare(ctx, home, owner, second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Path != checkout.Path || read(t, again.Path) != "v2" {
		t.Fatalf("the same directory is reused at the new ref: %s %q", again.Path, read(t, again.Path))
	}
	if _, err := os.Stat(filepath.Join(again.Path, "stray.txt")); !os.IsNotExist(err) {
		t.Fatal("leftovers from the previous use are cleaned")
	}
	if _, err := Prepare(ctx, home, owner, "no-such-ref", nil); err == nil || !strings.Contains(err.Error(), "is not a commit") {
		t.Fatalf("a bad ref is reported: %v", err)
	}
}

func TestPrepareAppliesPatches(t *testing.T) {
	owner, first, _ := newOwner(t)
	patch := filepath.Join(t.TempDir(), "flags.patch")
	os.WriteFile(patch, []byte("--- a/web/who\n+++ b/web/who\n@@ -1 +1 @@\n-v1\n+v1 with the flag on\n"), 0o644)
	checkout, err := Prepare(context.Background(), t.TempDir(), owner, first, []string{patch})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, checkout.Path) != "v1 with the flag on" || !strings.Contains(checkout.Label, "with flags.patch") {
		t.Fatalf("patched checkout serves %q, label %q", read(t, checkout.Path), checkout.Label)
	}
	bad := filepath.Join(t.TempDir(), "bad.patch")
	os.WriteFile(bad, []byte("--- a/web/who\n+++ b/web/who\n@@ -1 +1 @@\n-nothing like this\n+x\n"), 0o644)
	if _, err := Prepare(context.Background(), t.TempDir(), owner, first, []string{bad}); err == nil {
		t.Fatal("a patch that does not apply is an error")
	}
}

func TestPruneRemovesCheckoutsOfWorktreesThatAreGone(t *testing.T) {
	owner, first, _ := newOwner(t)
	home := t.TempDir()
	checkout, err := Prepare(context.Background(), home, owner, first, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed := Prune(context.Background(), home, func(string) bool { return false }); len(removed) != 0 {
		t.Fatal("the owner still exists, so its checkout stays")
	}
	os.RemoveAll(owner.Path)
	if removed := Prune(context.Background(), home, func(path string) bool { return path == checkout.Path }); len(removed) != 0 {
		t.Fatal("a checkout a container still serves stays")
	}
	if removed := Prune(context.Background(), home, func(string) bool { return false }); len(removed) != 1 {
		t.Fatalf("removed = %v", removed)
	}
	if _, err := os.Stat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("the checkout directory is gone")
	}
}
