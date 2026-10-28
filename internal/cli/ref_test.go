package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLITakeARefForBeforeShots(t *testing.T) {
	s := newStack(t)
	code, out := dibs(t, "take", s.container, "--ref", "main", "--tree", s.a)
	if code != exitOK || !strings.Contains(out, "will serve main at ") || !strings.Contains(out, "without --ref to switch them back") {
		t.Fatalf("take --ref: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "main" {
		t.Fatalf("the container should run the ref, not the worktree's changes: %q", got)
	}
	if code, out := dibs(t, "check", "--tree", s.a); code != exitOK {
		t.Fatalf("serving the checkout asked for is fine for check: %d\n%s", code, out)
	}

	patch := filepath.Join(t.TempDir(), "flag.patch")
	os.WriteFile(patch, []byte("--- a/web/who\n+++ b/web/who\n@@ -1 +1 @@\n-main\n+main with the flag on\n"), 0o644)
	if code, out := dibs(t, "take", s.container, "--ref", "main", "--patch", patch, "--tree", s.a); code != exitOK {
		t.Fatalf("take --ref --patch: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "main with the flag on" {
		t.Fatalf("the patch should be applied to the checkout: %q", got)
	}

	if code, out := dibs(t, "take", s.container, "--tree", s.a); code != exitOK {
		t.Fatalf("switching back: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "ticket-a" {
		t.Fatalf("without --ref the container serves the worktree again: %q", got)
	}
	if code, out := dibs(t, "take", s.container, "--patch", patch, "--tree", s.a); code != exitError || !strings.Contains(out, "only applies with --ref") {
		t.Fatalf("--patch needs --ref: %d\n%s", code, out)
	}
}
