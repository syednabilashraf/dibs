package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/pressure"
)

func privateContainers(t *testing.T) []string {
	t.Helper()
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=dibs.test=1").Output()
	return strings.Fields(string(out))
}

func TestCLITestRunsInAPrivateCopy(t *testing.T) {
	s := newStack(t)
	underPressure(t, pressure.Normal)
	before := len(privateContainers(t))
	code, out := dibs(t, "test", "--tree", s.a, s.container, "--", "cat", "/app/who")
	if code != exitOK || !strings.Contains(out, "ticket-a") || !strings.Contains(out, "private copy of "+s.container) {
		t.Fatalf("the worktree's code is mounted in the copy: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "main" {
		t.Fatalf("the shared container is untouched, serves %q", got)
	}
	if code, out := dibs(t, "test", "--tree", s.a, s.container, "sh -c 'exit 3'"); code != 3 || !strings.Contains(out, "exited with code 3") {
		t.Fatalf("the command's exit code comes back: %d\n%s", code, out)
	}
	if after := len(privateContainers(t)); after != before {
		t.Fatalf("private containers are removed afterwards: %d left", after-before)
	}
}

func TestCLITestLimitsRunsAcrossTheMachine(t *testing.T) {
	s := newStack(t)
	underPressure(t, pressure.Normal)
	os.WriteFile(filepath.Join(os.Getenv("DIBS_HOME"), "config.yaml"), []byte(fmt.Sprintf("test:\n  max_parallel: 1\ncontainers:\n  %s:\n    ready:\n      settle: 500ms\n", s.container)), 0o644)
	var wg sync.WaitGroup
	outputs := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 700 * time.Millisecond)
			var out bytes.Buffer
			cmd := exec.Command(os.Args[0], "test", "--tree", s.a, s.container, "--", "sh", "-c", "date +%s; sleep 3")
			cmd.Env = append(os.Environ(), "DIBS_TEST_AS_CLI=1")
			cmd.Stdout, cmd.Stderr = &out, &out
			cmd.Run()
			outputs[i] = out.String()
		}()
	}
	wg.Wait()
	waited, started := 0, []int{}
	for _, out := range outputs {
		if strings.Contains(out, "waiting for one to finish") {
			waited++
		}
		for _, line := range strings.Fields(out) {
			if n, err := strconv.Atoi(line); err == nil && n > 1_000_000_000 {
				started = append(started, n)
			}
		}
	}
	if waited != 1 || len(started) != 2 || absInt(started[0]-started[1]) < 3 {
		t.Fatalf("one run should wait for the other, so their commands never overlap:\n%s\n---\n%s", outputs[0], outputs[1])
	}
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestCLITestSweepsCopiesLeftByDeadRuns(t *testing.T) {
	s := newStack(t)
	underPressure(t, pressure.Normal)
	stale := fmt.Sprintf("dibs-test-stale-%d", time.Now().UnixNano())
	sh(t, "docker", "run", "-d", "--name", stale, "--label", "dibs.test=1", "--label", "dibs.test.pid=999999", "busybox:latest", "sleep", "3600")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", stale).Run() })
	if code, out := dibs(t, "test", "--tree", s.a, s.container, "--", "true"); code != exitOK {
		t.Fatalf("%d %s", code, out)
	}
	if exec.Command("docker", "inspect", stale).Run() == nil {
		t.Fatal("a copy whose dibs process is gone should be removed")
	}
}

func TestPrivateRunsWaitOutCriticalMemoryPressure(t *testing.T) {
	saved, savedRecheck := pressure.Current, memoryRecheck
	t.Cleanup(func() { pressure.Current, memoryRecheck = saved, savedRecheck })
	memoryRecheck = 10 * time.Millisecond
	calls := 0
	pressure.Current = func(context.Context) pressure.Level {
		calls++
		if calls < 3 {
			return pressure.Critical
		}
		return pressure.Warning
	}
	var out bytes.Buffer
	a := &app{env: &env{stdout: &out, stderr: &out}}
	if err := a.waitForMemory(context.Background()); err != nil || calls != 3 || !strings.Contains(out.String(), "waiting for it to ease") {
		t.Fatalf("err %v, calls %d, output %q", err, calls, out.String())
	}
	pressure.Current = func(context.Context) pressure.Level { return pressure.Critical }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.waitForMemory(ctx); err == nil {
		t.Fatal("a stopped wait is an error")
	}
}
