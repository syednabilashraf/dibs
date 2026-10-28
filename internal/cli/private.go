package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/pressure"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/swap"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runTest(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "test", "[--workdir DIR] [--env KEY=VALUE]... [--memory 8g] [--tree DIR] <container> [--] <command>...")
	workdir := fs.String("workdir", "", "working directory inside the private container (default: the container's)")
	memory := fs.String("memory", a.cfg.Test.Memory, "memory limit for the private container, such as 8g")
	treeFlag := fs.String("tree", "", "worktree to mount (default: current directory)")
	var envs multiFlag
	fs.Var(&envs, "env", "extra environment variable, KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	rest := fs.Args()
	if len(rest) < 2 || (len(rest) == 2 && rest[1] == "--") {
		fs.Usage()
		return exitError
	}
	name, command := rest[0], rest[1:]
	if command[0] == "--" {
		command = command[1:]
	}
	limit, err := swap.ParseMemory(*memory)
	if err != nil {
		return e.errorf("%v", err)
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	client, err := docker.New(ctx)
	if err != nil {
		return e.errorf("%v", err)
	}
	sw := &swap.Swapper{Docker: client, Store: a.store, Config: a.cfg}
	live, baseline, err := sw.Baseline(ctx, name)
	if err != nil {
		return e.errorf("%v", err)
	}
	source := live
	if source == nil {
		source = baseline
	}
	spec, err := swap.PrivateSpec(source, swap.Worktree(t), swap.Private{
		Name: privateName(name, t.Path), Tree: t.Path, Command: command, WorkDir: *workdir, Env: envs, Memory: limit, PID: os.Getpid(),
	})
	if err != nil {
		return e.errorf("%s: %v", name, err)
	}

	if err := a.waitForMemory(ctx); err != nil {
		return e.errorf("%v", err)
	}
	release, err := a.testSlot(ctx)
	if err != nil {
		return e.errorf("%v", err)
	}
	defer release()
	sweepPrivate(ctx, client)

	id, err := client.Create(ctx, spec.Name, spec.Body)
	if err != nil {
		return e.errorf("create %s: %v", spec.Name, err)
	}
	cleanup := func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), time.Minute)
		defer removeCancel()
		client.Remove(removeCtx, id)
	}
	defer cleanup()
	for network, endpoint := range spec.Networks {
		if err := client.Connect(ctx, network, id, endpoint); err != nil {
			return e.errorf("connect %s: %v", network, err)
		}
	}
	e.printf("dibs: running in a private copy of %s with %s mounted; shared containers are not touched\n", name, t.Label)
	if err := client.Start(ctx, id); err != nil {
		return e.errorf("start %s: %v", spec.Name, err)
	}
	streamed := make(chan struct{})
	go func() {
		client.Follow(context.WithoutCancel(ctx), id, false, e.stdout)
		close(streamed)
	}()
	code, err := client.Wait(ctx, id)
	if ctx.Err() != nil {
		e.printf("\ndibs: stopped; removing the private container\n")
		return 130
	}
	if err != nil {
		return e.errorf("wait for %s: %v", spec.Name, err)
	}
	select {
	case <-streamed:
	case <-time.After(5 * time.Second):
	}
	e.printf("dibs: %s exited with code %d\n", strings.Join(command, " "), code)
	return code
}

func privateName(container, treePath string) string {
	return fmt.Sprintf("dibs-test-%s-%s-%04d", container, strings.TrimPrefix(filepath.Base(TmpDir(treePath)), "dibs-"), rand.Intn(10000))
}

func (a *app) testSlot(ctx context.Context) (func(), error) {
	dir := filepath.Join(a.store.Dir, "test-slots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	announced := false
	for {
		for i := 0; i < a.cfg.Test.MaxParallel; i++ {
			file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("slot-%d.lock", i)), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return nil, err
			}
			if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				return func() {
					syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
					file.Close()
				}, nil
			}
			file.Close()
		}
		if !announced {
			a.printf("dibs: %d private test run(s) are already going, the most at once (test.max_parallel); waiting for one to finish\n", a.cfg.Test.MaxParallel)
			announced = true
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("stopped while waiting for a test slot")
		case <-time.After(time.Second):
		}
	}
}

var memoryRecheck = 5 * time.Second

func (a *app) waitForMemory(ctx context.Context) error {
	announced := false
	for pressure.Current(ctx) == pressure.Critical {
		if !announced {
			a.printf("dibs: this machine is out of memory (memory pressure is critical); waiting for it to ease before starting another container\n")
			announced = true
		}
		select {
		case <-ctx.Done():
			return errors.New("stopped while waiting for memory pressure to ease")
		case <-time.After(memoryRecheck):
		}
	}
	return nil
}

func sweepPrivate(ctx context.Context, client *docker.Client) {
	listed, err := client.ListLabeled(ctx, swap.LabelTest+"=1")
	if err != nil {
		return
	}
	for _, c := range listed {
		if pid, err := strconv.Atoi(c.Labels[swap.LabelTestPID]); err == nil && !state.ProcessAlive(pid) {
			client.Remove(ctx, c.ID)
		}
	}
}
