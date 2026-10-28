package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

type background struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func startDibs(t *testing.T, args ...string) *background {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "DIBS_TEST_AS_CLI=1")
	b := &background{cmd: cmd, out: &syncBuffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = b.out, b.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		cmd.Wait()
		close(b.done)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-b.done
	})
	return b
}

func (b *background) wait(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case <-b.done:
		return b.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		t.Fatalf("still running after %s:\n%s", within, b.out.String())
	}
	return -1
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readState(t *testing.T, home string) *state.State {
	t.Helper()
	store, _ := state.Open(home)
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCancelStopsAQueuedTake(t *testing.T) {
	w := newHookWorld(t)
	take := startDibs(t, "take", "browser", "--wait", "--tree", w.b.Path)
	waitFor(t, "b to queue", func() bool { return queue.Position(readState(t, w.home), w.b.Path) == 1 })

	code, out := dibs(t, "cancel", "--tree", w.b.Path)
	if code != exitOK || !strings.Contains(out, "asked the take (pid") {
		t.Fatalf("cancel: %d %s", code, out)
	}
	if code := take.wait(t, 10*time.Second); code != exitNo || !strings.Contains(take.out.String(), "stopped waiting") {
		t.Fatalf("the take should stop: %d %s", code, take.out.String())
	}
	st := readState(t, w.home)
	if queue.Position(st, w.b.Path) != 0 || len(st.HeldBy(w.b.Path, time.Now())) != 0 {
		t.Fatal("after a cancel, b neither waits nor holds anything")
	}
}

func TestCancelWithoutATakePassesWhatIsHeld(t *testing.T) {
	w := newHookWorld(t)
	code, out := dibs(t, "cancel", "--tree", w.a.Path)
	if code != exitOK || !strings.Contains(out, "no take is in progress") || !strings.Contains(out, "passed browser, web") {
		t.Fatalf("cancel: %d %s", code, out)
	}
	if code, out := dibs(t, "cancel", "--tree", w.a.Path); code != exitNo || !strings.Contains(out, "holds nothing") {
		t.Fatalf("nothing left to cancel: %d %s", code, out)
	}
}

func TestStoppingAWaitingTakeGivesBackALateGrant(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		st.Resources["browser"].Holder = &state.Holder{Tree: w.b.Path, Label: w.b.Label, Kind: state.KindSession, Since: time.Now(), Expires: time.Now().Add(time.Hour)}
		return nil
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{env: &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, cfg: cfg, store: store}
	b, _ := tree.Resolve(w.b.Path)
	returned := a.giveBack(b, []string{"browser", "web"}, map[string]bool{"web": true})
	if len(returned) != 1 || returned[0] != "browser" {
		t.Fatalf("only what this take acquired is given back: %v", returned)
	}
}

func TestCLICancelWhileAContainerStartsLeavesItConsistent(t *testing.T) {
	s := newStack(t)
	home := os.Getenv("DIBS_HOME")
	config := "stop_timeout: 1s\nexec_grace: 0s\ncontainers:\n  " + s.container + ":\n    ready:\n      log: 'never printed'\n"
	os.WriteFile(home+"/config.yaml", []byte(config), 0o644)
	t.Setenv("DIBS_CONFIG", home+"/config.yaml")

	take := startDibs(t, "take", s.container, "--wait", "--tree", s.b)
	waitFor(t, "the take to wait for readiness", func() bool { return strings.Contains(take.out.String(), "waiting for it to become ready") })

	code, out := dibs(t, "cancel", "--tree", s.b)
	if code != exitOK {
		t.Fatalf("cancel: %d %s", code, out)
	}
	if code := take.wait(t, 20*time.Second); code != exitNo || !strings.Contains(take.out.String(), "keeps starting on its own") {
		t.Fatalf("take: %d %s", code, take.out.String())
	}
	r := readState(t, home).Lookup(s.container)
	if r.Holder != nil && r.ActiveHolder(time.Now()) != nil {
		t.Fatalf("nothing is held after a cancel: %+v", r.Holder)
	}
	if r.Status != state.StatusStarting || r.Serving != tree.Clean(s.b) {
		t.Fatalf("the container serves b and is starting, not failed: %s %s", r.Status, r.Serving)
	}
	if !strings.Contains(out, s.container+" keeps running your worktree (starting)") {
		t.Fatalf("cancel should say what the container is left doing:\n%s", out)
	}
	if got := s.serves(t); got != "ticket-b" {
		t.Fatalf("the recreated container keeps b's code: %q", got)
	}
}
