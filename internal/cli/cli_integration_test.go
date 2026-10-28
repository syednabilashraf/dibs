package cli

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/docker"
)

type stack struct {
	container string
	main      string
	a         string
	b         string
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newStack(t *testing.T) *stack {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := docker.New(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	if exec.Command("docker", "image", "inspect", "busybox:latest").Run() != nil {
		t.Skip("busybox:latest is not present locally")
	}
	root := t.TempDir()
	s := &stack{
		container: fmt.Sprintf("dibs-cli-%d", rand.Int63()),
		main:      filepath.Join(root, "app"),
		a:         filepath.Join(root, "app-a"),
		b:         filepath.Join(root, "app-b"),
	}
	os.MkdirAll(filepath.Join(s.main, "web"), 0o755)
	os.WriteFile(filepath.Join(s.main, "web", "who"), []byte("main\n"), 0o644)
	sh(t, "git", "init", "-q", "-b", "main", s.main)
	sh(t, "git", "-C", s.main, "add", ".")
	sh(t, "git", "-C", s.main, "commit", "-q", "-m", "init")
	for _, wt := range []struct{ dir, branch string }{{s.a, "ticket-a"}, {s.b, "ticket-b"}} {
		sh(t, "git", "-C", s.main, "worktree", "add", "-q", "-b", wt.branch, wt.dir)
		os.WriteFile(filepath.Join(wt.dir, "web", "who"), []byte(wt.branch+"\n"), 0o644)
	}
	sh(t, "docker", "run", "-d", "--name", s.container, "-v", filepath.Join(s.main, "web")+":/app:ro",
		"busybox:latest", "sh", "-c", "sleep 3600")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", s.container).Run() })

	home := t.TempDir()
	t.Setenv("DIBS_HOME", home)
	config := fmt.Sprintf(`
stop_timeout: 1s
exec_grace: 0s
groups:
  ui: [browser, %s]
containers:
  %s:
    ready:
      settle: 500ms
`, s.container, s.container)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o644)
	return s
}

func dibs(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &out)
	return code, out.String()
}

func (s *stack) serves(t *testing.T) string {
	t.Helper()
	return sh(t, "docker", "exec", s.container, "cat", "/app/who")
}

func TestCLIHandoffBetweenWorktrees(t *testing.T) {
	s := newStack(t)

	code, out := dibs(t, "take", "ui", "--tree", s.a)
	if code != exitOK {
		t.Fatalf("a take: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "ticket-a" {
		t.Fatalf("container serves %q after a's take", got)
	}

	code, out = dibs(t, "take", "ui", "--tree", s.b)
	if code != exitNo || !strings.Contains(out, "held by ticket-a") {
		t.Fatalf("b's one-shot take should be refused with the holder named: %d\n%s", code, out)
	}

	done := make(chan string, 1)
	go func() {
		code, out := dibs(t, "take", "ui", "--wait", "--tree", s.b)
		done <- fmt.Sprintf("%d\n%s", code, out)
	}()
	time.Sleep(1500 * time.Millisecond)
	if code, out := dibs(t, "line"); code != exitOK || !strings.Contains(out, "ticket-b") {
		t.Fatalf("b should be queued: %s", out)
	}
	if code, out := dibs(t, "check", "--tree", s.a); code != exitOK {
		t.Fatalf("a still holds: %d\n%s", code, out)
	}

	if code, out := dibs(t, "pass", "--tree", s.a); code != exitOK {
		t.Fatalf("a pass: %d\n%s", code, out)
	}
	select {
	case result := <-done:
		if !strings.HasPrefix(result, "0\n") || !strings.Contains(result, "it was serving ticket-a;") {
			t.Fatalf("b's waiting take should succeed and name what it displaced:\n%s", result)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("b was never granted")
	}
	if got := s.serves(t); got != "ticket-b" {
		t.Fatalf("container serves %q after the handoff", got)
	}

	if code, out := dibs(t, "check", "--tree", s.a); code != exitNo {
		t.Fatalf("a's check must fail after the handoff: %d\n%s", code, out)
	}
	if code, out := dibs(t, "check", "--tree", s.b); code != exitOK {
		t.Fatalf("b's check: %d\n%s", code, out)
	}

	code, out = dibs(t, "status")
	if code != exitOK || !strings.Contains(out, "ticket-b (app-b)") || !strings.Contains(out, "recreated to serve ticket-b (was ticket-a)") {
		t.Fatalf("status:\n%s", out)
	}

	if code, out := dibs(t, "restore", s.container); code != exitNo || !strings.Contains(out, "skipped, held by ticket-b") {
		t.Fatalf("restore of a held container must be refused: %d\n%s", code, out)
	}

	if code, out := dibs(t, "pass", "--restore", "--tree", s.b); code != exitOK {
		t.Fatalf("b pass --restore: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "main" {
		t.Fatalf("container serves %q after restore", got)
	}
}

func TestCLICheckDetectsOutsideRecreate(t *testing.T) {
	s := newStack(t)
	if code, out := dibs(t, "take", s.container, "--tree", s.a); code != exitOK {
		t.Fatalf("take: %s", out)
	}
	sh(t, "docker", "rm", "-f", s.container)
	sh(t, "docker", "run", "-d", "--name", s.container, "-v", filepath.Join(s.main, "web")+":/app:ro", "busybox:latest", "sh", "-c", "sleep 3600")
	code, out := dibs(t, "check", "--tree", s.a)
	if code != exitNo || !strings.Contains(out, "recreated outside dibs") {
		t.Fatalf("check should catch a container recreated behind dibs' back: %d\n%s", code, out)
	}
}

func TestCLITakeFromBaselineWarns(t *testing.T) {
	s := newStack(t)
	code, out := dibs(t, "take", s.container, "--tree", s.main)
	if code != exitOK || !strings.Contains(out, "is the baseline checkout") {
		t.Fatalf("taking from the baseline checkout should say so: %d\n%s", code, out)
	}
	code, out = dibs(t, "take", s.container, "--tree", s.main)
	if !strings.Contains(out, "is the baseline checkout") {
		t.Fatalf("the note repeats on every take from the baseline:\n%s", out)
	}
	dibs(t, "pass", "--tree", s.main)
	if code, out := dibs(t, "take", s.container, "--tree", s.a); code != exitOK || strings.Contains(out, "baseline checkout") {
		t.Fatalf("a take from a worktree must not get the baseline note: %d\n%s", code, out)
	}
}

func (s *stack) rerun(t *testing.T, script string) {
	t.Helper()
	exec.Command("docker", "rm", "-f", s.container).Run()
	sh(t, "docker", "run", "-d", "--name", s.container, "-v", filepath.Join(s.main, "web")+":/app:ro", "busybox:latest", "sh", "-c", script)
}

func writeStackConfig(t *testing.T, body string) {
	t.Helper()
	os.WriteFile(filepath.Join(os.Getenv("DIBS_HOME"), "config.yaml"), []byte("stop_timeout: 1s\nexec_grace: 0s\n"+body), 0o644)
}

func TestCLIFailedStartKeepsTheLeaseAndPassRestores(t *testing.T) {
	s := newStack(t)
	s.rerun(t, "grep -q ticket-a /app/who && { echo 'cannot start on ticket-a'; exit 1; }; sleep 3600")
	writeStackConfig(t, fmt.Sprintf("containers:\n  %s:\n    ready:\n      settle: 3s\n", s.container))

	code, out := dibs(t, "take", s.container, "--tree", s.a)
	if code != exitError || !strings.Contains(out, "you still hold") || !strings.Contains(out, "exited during startup") || !strings.Contains(out, "cannot start on ticket-a") {
		t.Fatalf("a failed start must keep the lease and explain how to recover: %d\n%s", code, out)
	}
	if code, out := dibs(t, "take", s.container, "--tree", s.b); code != exitNo || !strings.Contains(out, "held by ticket-a") {
		t.Fatalf("others must queue rather than get the broken container: %d\n%s", code, out)
	}
	if code, out := dibs(t, "check", "--tree", s.a); code != exitNo || !strings.Contains(out, "failed to start") {
		t.Fatalf("check must fail while the container is broken: %d\n%s", code, out)
	}
	code, out = dibs(t, "pass", "--tree", s.a)
	if code != exitOK || !strings.Contains(out, "goes back on the baseline") {
		t.Fatalf("passing a failed container should restore it: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "main" {
		t.Fatalf("after pass the failed container must be back on the baseline, serves %q", got)
	}
}

func TestCLISlowStartKeepsTheLeaseAndIsRechecked(t *testing.T) {
	s := newStack(t)
	writeStackConfig(t, fmt.Sprintf("containers:\n  %s:\n    ready:\n      log: 'never printed'\n      timeout: 2s\n", s.container))
	code, out := dibs(t, "take", s.container, "--tree", s.a)
	if code != exitError || !strings.Contains(out, "is running your worktree but is not ready yet") || !strings.Contains(out, "still starting after 2s") {
		t.Fatalf("a slow start keeps the lease and says it is still starting: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "ticket-a" {
		t.Fatalf("it keeps the worktree, serves %q", got)
	}
	if code, out := dibs(t, "check", "--tree", s.a); code != exitNo || !strings.Contains(out, "starting, not ready yet") {
		t.Fatalf("check must fail while it is starting: %d\n%s", code, out)
	}
	writeStackConfig(t, fmt.Sprintf("containers:\n  %s:\n    ready:\n      settle: 500ms\n", s.container))
	if code, out := dibs(t, "check", "--tree", s.a); code != exitOK {
		t.Fatalf("once it is ready, check re-checks and passes without a recreate: %d\n%s", code, out)
	}
}

func TestCLIUnknownResource(t *testing.T) {
	s := newStack(t)
	code, out := dibs(t, "take", "no-such-thing", "--tree", s.a)
	if code != exitError || !strings.Contains(out, "not a container or a virtual resource") {
		t.Fatalf("typos must be rejected: %d\n%s", code, out)
	}
}

func TestCLINoSwapAndVirtualOnly(t *testing.T) {
	s := newStack(t)
	if code, out := dibs(t, "take", "browser", "--tree", s.a); code != exitOK {
		t.Fatalf("virtual take: %s", out)
	}
	if code, out := dibs(t, "take", s.container, "--no-swap", "--tree", s.a); code != exitOK {
		t.Fatalf("no-swap take: %s", out)
	}
	if got := s.serves(t); got != "main" {
		t.Fatalf("--no-swap must not recreate, serves %q", got)
	}
	if code, out := dibs(t, "check", "--tree", s.a); code != exitOK {
		t.Fatalf("check with no-swap: %d\n%s", code, out)
	}
}

func TestTmpDirIsPerWorktree(t *testing.T) {
	a, b := TmpDir("/work/app-a"), TmpDir("/work/app-b")
	if a == b || !strings.HasPrefix(a, "/tmp/dibs-app-a-") {
		t.Fatalf("tmpdirs: %s %s", a, b)
	}
	if TmpDir("/other/app-a") == a {
		t.Fatal("same basename in a different place must not collide")
	}
	if TmpDir("/work/app-a") != a {
		t.Fatal("tmpdir must be stable for a worktree")
	}
}
