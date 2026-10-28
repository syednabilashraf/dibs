package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/swap"
)

func runCancel(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "cancel", "[--timeout 3m] [--tree DIR]")
	patience := fs.Duration("timeout", 3*time.Minute, "how long to wait for a running take to stop safely")
	treeFlag := fs.String("tree", "", "worktree to act for (default: current directory)")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		return e.errorf("%v", err)
	}
	takers := []int{}
	for _, pid := range queue.Takers(st, t.Path, time.Now()) {
		if pid != os.Getpid() && state.ProcessAlive(pid) && isTakeProcess(pid) {
			takers = append(takers, pid)
		}
	}
	if len(takers) == 0 {
		queued := queue.Position(st, t.Path) > 0
		a.leave(t.Path)
		if len(st.HeldBy(t.Path, time.Now())) == 0 {
			if queued {
				e.printf("dibs: removed %s's place in line\n", t.Label)
				return exitOK
			}
			e.printf("dibs: %s has no take in progress and holds nothing\n", t.Label)
			return exitNo
		}
		e.printf("dibs: no take is in progress; passing what %s holds\n", t.Label)
		return runPass(e, []string{"--tree", t.Path})
	}

	for _, pid := range takers {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	e.printf("dibs: asked the take (pid %s) to stop; it finishes the step it is in, so no container is left half-recreated\n", joinInts(takers))
	deadline := time.Now().Add(*patience)
	for anyAlive(takers) {
		if time.Now().After(deadline) {
			return e.errorf("the take (pid %s) is still finishing its current step; run `dibs status` in a minute", joinInts(takers))
		}
		time.Sleep(200 * time.Millisecond)
	}

	var returned []string
	a.store.Update(func(st *state.State) error {
		queue.Leave(st, t.Path)
		mine := []string{}
		for _, name := range st.HeldBy(t.Path, time.Now()) {
			for _, pid := range takers {
				if st.Resources[name].Holder.Taker == pid {
					mine = append(mine, name)
				}
			}
		}
		returned = queue.Pass(st, t.Path, mine, time.Now())
		return nil
	})
	if len(returned) > 0 {
		a.store.Log(state.Event{Kind: "pass", Resource: strings.Join(returned, ","), Tree: t.Path, Label: t.Label})
		e.printf("dibs: gave back %s, which the stopped take still held\n", strings.Join(returned, ", "))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a.repairStalled(ctx, takers)
	a.describeLeftovers(t.Path)
	return exitOK
}

func (a *app) repairStalled(ctx context.Context, takers []int) {
	st, err := a.store.Read()
	if err != nil {
		return
	}
	var client *docker.Client
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Virtual || r.Status != state.StatusSwapping || r.Intent == nil || state.ProcessAlive(r.Intent.PID) || !containsPID(takers, r.Intent.PID) {
			continue
		}
		if client == nil {
			if client, err = docker.New(ctx); err != nil {
				return
			}
		}
		raw, err := client.Inspect(ctx, name)
		if err != nil {
			a.printf("dibs: %s was left half-recreated by the stopped take; `dibs restore %s` puts it back on the baseline\n", name, name)
			continue
		}
		live := docker.Summarize(raw)
		if !live.Running {
			if err := client.Start(ctx, live.ID); err != nil {
				a.printf("dibs: %s was left stopped by the stopped take and did not start again (%v); `dibs restore %s` recreates it\n", name, err, name)
				continue
			}
		}
		a.store.Update(func(st *state.State) error {
			current := st.Get(name)
			current.Status, current.Intent, current.ContainerID = state.StatusStarting, nil, live.ID
			current.Serving, current.ServingLabel = live.Label(swap.LabelTree), live.Label(swap.LabelBranch)
			return nil
		})
	}
}

func (a *app) describeLeftovers(treePath string) {
	st, err := a.store.Read()
	if err != nil {
		return
	}
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Virtual || r.Serving != treePath || r.ActiveHolder(time.Now()) != nil {
			continue
		}
		a.printf("dibs: %s keeps running your worktree (%s) until the next holder takes it\n", name, statusText(r))
	}
}

func isTakeProcess(pid int) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(out)) {
		if field == "take" {
			return true
		}
	}
	return false
}

func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if state.ProcessAlive(pid) {
			return true
		}
	}
	return false
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}
