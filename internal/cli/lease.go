package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/swap"
	"github.com/syednabilashraf/dibs/internal/tree"
)

func contextWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

func runTake(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "take", "<resource|group>... [--wait] [--timeout 30m] [--lease 30m] [--no-swap] [--tree DIR]")
	wait := fs.Bool("wait", false, "queue until granted, then swap (run it in the background)")
	timeout := fs.Duration("timeout", a.cfg.WaitTimeout.D(), "give up waiting after this long")
	lease := fs.Duration("lease", a.cfg.Lease.D(), "lease length")
	noSwap := fs.Bool("no-swap", false, "hold the lock without recreating containers")
	treeFlag := fs.String("tree", "", "worktree to act for (default: current directory)")
	positional, err := parse(fs, args)
	if err != nil {
		return exitError
	}
	names := a.cfg.Expand(positional)
	if len(names) == 0 {
		fs.Usage()
		return exitError
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	if err := a.validate(ctx, names); err != nil {
		return e.errorf("%v", err)
	}

	deadline := time.Now().Add(*timeout)
	last := ""
	for {
		var result queue.Result
		var takeErr error
		err := a.store.Update(func(st *state.State) error {
			a.prune(st)
			for _, name := range names {
				st.Get(name).Virtual = a.cfg.IsVirtual(name)
			}
			result, takeErr = queue.Take(st, queue.Request{
				Tree: t.Path, Label: t.Label, Resources: names, Lease: *lease,
				PID: os.Getpid(), NoSwap: *noSwap, Enqueue: *wait,
			}, state.ProcessAlive)
			return nil
		})
		if err != nil {
			return e.errorf("%v", err)
		}
		if takeErr != nil {
			return e.errorf("%v", takeErr)
		}
		if result.Outcome == queue.Granted {
			break
		}
		message := describeWait(result)
		if !*wait {
			e.printf("dibs: not granted: %s\n", message)
			e.printf("dibs: run `dibs take %s --wait` in the background to queue\n", strings.Join(positional, " "))
			return exitNo
		}
		if message != last {
			e.printf("dibs: waiting: %s\n", message)
			last = message
		}
		if time.Now().After(deadline) {
			a.leave(t.Path)
			e.printf("dibs: gave up after %s\n", *timeout)
			return exitNo
		}
		select {
		case <-ctx.Done():
			a.leave(t.Path)
			e.printf("dibs: stopped waiting\n")
			return exitNo
		case <-time.After(time.Second):
		}
	}

	a.store.Log(state.Event{Kind: "take", Resource: strings.Join(names, ","), Tree: t.Path, Label: t.Label})
	expires := time.Now().Add(*lease)
	e.printf("dibs: %s holds %s until %s\n", t.Label, strings.Join(names, ", "), clock(expires))

	containers := a.split(names)
	if len(containers) == 0 {
		return exitOK
	}
	sw, err := a.swapper(ctx)
	if err != nil {
		a.release(t.Path, names)
		return e.errorf("%v (released %s)", err, strings.Join(names, ", "))
	}
	if *noSwap {
		for _, name := range containers {
			a.describe(ctx, sw, name)
		}
		return exitOK
	}
	for _, name := range containers {
		outcome, err := sw.Serve(ctx, name, t)
		if outcome != nil {
			a.report(outcome)
		}
		if err != nil {
			a.release(t.Path, names)
			if errors.Is(err, swap.ErrInterrupted) {
				return e.errorf("interrupted after %s was recreated; released %s", name, strings.Join(names, ", "))
			}
			return e.errorf("%v\ndibs: released %s", err, strings.Join(names, ", "))
		}
	}
	e.printf("dibs: ready. Run `dibs check` after each test batch and `dibs pass` when done testing.\n")
	return exitOK
}

func (a *app) report(o *swap.Outcome) {
	serving := o.ServingLabel
	if o.Serving == "" {
		serving = "the baseline"
	}
	if o.Waited >= time.Second {
		a.printf("dibs: %s: waited %s for running execs\n", o.Name, o.Waited.Round(time.Second))
	}
	if len(o.Interrupted) > 0 {
		a.printf("dibs: %s: recreated with %d exec(s) still running, which were killed: %s\n", o.Name, len(o.Interrupted), strings.Join(o.Interrupted, "; "))
	}
	if !o.Swapped {
		a.printf("dibs: %s: already serving %s\n", o.Name, serving)
	} else {
		a.printf("dibs: %s: recreated, now serving %s\n", o.Name, serving)
		if o.Previous != "" && o.Previous != o.Serving {
			a.printf("dibs: %s: it was serving %s; that worktree's running execs and files copied into the container are gone\n", o.Name, o.PreviousLabel)
		}
	}
	for _, kept := range o.Kept {
		a.printf("dibs: %s: kept %s from the baseline (%s)\n", o.Name, kept.Source, kept.Reason)
	}
}

func (a *app) describe(ctx context.Context, sw *swap.Swapper, name string) {
	meta, err := sw.Describe(ctx, name)
	if err != nil {
		return
	}
	a.store.Update(func(st *state.State) error {
		r := st.Get(name)
		r.Service, r.Project, r.Ports = meta.Service, meta.Project, meta.Ports
		r.Repo, r.BaselineRoots = meta.Repo, meta.BaselineRoots
		if r.ContainerID == "" {
			r.ContainerID = meta.ContainerID
		}
		return nil
	})
}

func (a *app) prune(st *state.State) {
	expired, dropped := queue.Prune(st, time.Now(), state.ProcessAlive)
	for _, x := range expired {
		a.store.Log(state.Event{Kind: "expire", Resource: x.Resource, Tree: x.Holder.Tree, Label: x.Holder.Label})
	}
	for _, w := range dropped {
		a.store.Log(state.Event{Kind: "abandon", Resource: strings.Join(w.Resources, ","), Tree: w.Tree, Label: w.Label})
	}
}

func (a *app) leave(treePath string) {
	a.store.Update(func(st *state.State) error {
		queue.Leave(st, treePath)
		return nil
	})
}

func (a *app) release(treePath string, names []string) {
	a.store.Update(func(st *state.State) error {
		queue.Pass(st, treePath, names, time.Now())
		return nil
	})
}

func describeWait(result queue.Result) string {
	parts := []string{}
	for _, b := range result.Blockers {
		part := fmt.Sprintf("%s held by %s since %s", b.Resource, holderText(&b.Holder), clock(b.Holder.Since))
		if !b.Holder.Pinned() && !b.Holder.Expires.IsZero() {
			part += fmt.Sprintf(" (lease ends %s)", clock(b.Holder.Expires))
		}
		parts = append(parts, part)
	}
	if len(result.Ahead) > 0 {
		parts = append(parts, "behind "+strings.Join(result.Ahead, ", "))
	}
	if result.Position > 0 {
		parts = append(parts, fmt.Sprintf("position %d in line", result.Position))
	}
	if result.Outcome == queue.Blocked {
		parts = append(parts, "a human pinned it; wait for `dibs drop`")
	}
	if len(parts) == 0 {
		return "someone ahead in line wants the same resources"
	}
	return strings.Join(parts, "; ")
}

func runPass(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "pass", "[resource|group]... [--restore] [--tree DIR]")
	restore := fs.Bool("restore", a.cfg.RestoreOnPass, "recreate released containers from their baseline")
	treeFlag := fs.String("tree", "", "worktree to act for (default: current directory)")
	positional, err := parse(fs, args)
	if err != nil {
		return exitError
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	names := a.cfg.Expand(positional)
	var released []string
	if err := a.store.Update(func(st *state.State) error {
		a.prune(st)
		released = queue.Pass(st, t.Path, names, time.Now())
		if len(names) == 0 {
			queue.Leave(st, t.Path)
		}
		return nil
	}); err != nil {
		return e.errorf("%v", err)
	}
	if len(released) == 0 {
		e.printf("dibs: %s holds nothing to pass\n", t.Label)
		return exitNo
	}
	a.store.Log(state.Event{Kind: "pass", Resource: strings.Join(released, ","), Tree: t.Path, Label: t.Label})
	e.printf("dibs: passed %s\n", strings.Join(released, ", "))
	if !*restore {
		return exitOK
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	return a.restore(ctx, nil, a.split(released))
}

func runCheck(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "check", "[resource|group]... [--tree DIR]")
	treeFlag := fs.String("tree", "", "worktree to act for (default: current directory)")
	positional, err := parse(fs, args)
	if err != nil {
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
	now := time.Now()
	names := a.cfg.Expand(positional)
	if len(names) == 0 {
		names = st.HeldBy(t.Path, now)
	}
	if len(names) == 0 {
		e.printf("dibs: %s holds nothing\n", t.Label)
		return exitNo
	}

	var client *docker.Client
	problems := []string{}
	for _, name := range names {
		r := st.Lookup(name)
		h := r.ActiveHolder(now)
		if h == nil {
			problems = append(problems, fmt.Sprintf("%s: not held by you any more (free)", name))
			continue
		}
		if h.Tree != t.Path {
			problems = append(problems, fmt.Sprintf("%s: held by %s, not you", name, holderText(h)))
			continue
		}
		if r.Virtual || h.NoSwap {
			continue
		}
		if r.Serving != "" && r.Serving != t.Path {
			problems = append(problems, fmt.Sprintf("%s: serving %s, not your worktree", name, servingText(r)))
			continue
		}
		if r.Status != state.StatusReady {
			problems = append(problems, fmt.Sprintf("%s: status %q, not ready", name, r.Status))
			continue
		}
		if client == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			client, err = docker.New(ctx)
			cancel()
			if err != nil {
				return e.errorf("%v", err)
			}
		}
		raw, err := client.Inspect(context.Background(), name)
		if errors.Is(err, docker.ErrNotFound) {
			problems = append(problems, fmt.Sprintf("%s: the container is gone", name))
			continue
		}
		if err != nil {
			return e.errorf("%v", err)
		}
		live := docker.Summarize(raw)
		switch {
		case live.ID != r.ContainerID:
			problems = append(problems, fmt.Sprintf("%s: recreated outside dibs since you took it; it no longer serves your worktree", name))
		case !live.Running:
			problems = append(problems, fmt.Sprintf("%s: not running (%s)", name, live.Status))
		case live.Label(swap.LabelTree) != r.Serving:
			problems = append(problems, fmt.Sprintf("%s: its labels say it serves %s", name, treeName(live.Label(swap.LabelTree))))
		}
	}
	if len(problems) > 0 {
		for _, p := range problems {
			e.printf("dibs: %s\n", p)
		}
		e.printf("dibs: results collected since then may not be from your code; discard them and take again\n")
		return exitNo
	}
	e.printf("dibs: ok: %s serve %s\n", strings.Join(names, ", "), t.Label)
	return exitOK
}

func runRenew(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "renew", "[--lease 30m] [--tree DIR]")
	lease := fs.Duration("lease", a.cfg.Lease.D(), "new lease length from now")
	treeFlag := fs.String("tree", "", "worktree to act for (default: current directory)")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	var renewed []string
	if err := a.store.Update(func(st *state.State) error {
		a.prune(st)
		renewed = queue.Renew(st, t.Path, *lease, time.Now())
		return nil
	}); err != nil {
		return e.errorf("%v", err)
	}
	if len(renewed) == 0 {
		e.printf("dibs: %s holds no leases\n", t.Label)
		return exitNo
	}
	e.printf("dibs: renewed %s until %s\n", strings.Join(renewed, ", "), clock(time.Now().Add(*lease)))
	return exitOK
}

func runRestore(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "restore", "<container|group>... | --all")
	all := fs.Bool("all", false, "every container dibs has managed")
	positional, err := parse(fs, args)
	if err != nil {
		return exitError
	}
	names := a.split(a.cfg.Expand(positional))
	if *all {
		st, err := a.store.Read()
		if err != nil {
			return e.errorf("%v", err)
		}
		names = nil
		for _, name := range st.Names() {
			if !st.Resources[name].Virtual {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		fs.Usage()
		return exitError
	}
	caller, _ := tree.Resolve("")
	ctx, cancel := contextWithSignals()
	defer cancel()
	return a.restore(ctx, caller, names)
}

func (a *app) restore(ctx context.Context, caller *tree.Tree, names []string) int {
	if len(names) == 0 {
		return exitOK
	}
	sw, err := a.swapper(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		return a.errorf("%v", err)
	}
	code := exitOK
	for _, name := range names {
		if h := st.Lookup(name).ActiveHolder(time.Now()); h != nil && (caller == nil || h.Tree != caller.Path) {
			a.printf("dibs: %s: skipped, held by %s\n", name, holderText(h))
			code = exitNo
			continue
		}
		outcome, err := sw.Serve(ctx, name, nil)
		if outcome != nil {
			a.report(outcome)
		}
		if err != nil {
			a.errorf("%v", err)
			code = exitError
			continue
		}
		if outcome.Swapped {
			a.store.Log(state.Event{Kind: "restore", Resource: name})
		}
	}
	return code
}

func runGrab(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "grab", "<resource|group>... [--tree DIR] [--note TEXT] [--no-swap]")
	treeFlag := fs.String("tree", "", "worktree to serve (default: current directory)")
	note := fs.String("note", "", "why, shown to everyone waiting")
	noSwap := fs.Bool("no-swap", false, "pin without recreating containers")
	positional, err := parse(fs, args)
	if err != nil {
		return exitError
	}
	names := a.cfg.Expand(positional)
	if len(names) == 0 {
		fs.Usage()
		return exitError
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	if err := a.validate(ctx, names); err != nil {
		return e.errorf("%v", err)
	}
	var displaced []queue.Displaced
	if err := a.store.Update(func(st *state.State) error {
		for _, name := range names {
			st.Get(name).Virtual = a.cfg.IsVirtual(name)
		}
		displaced = queue.Grab(st, state.Holder{Tree: t.Path, Label: t.Label, Note: *note, NoSwap: *noSwap}, names, time.Now())
		return nil
	}); err != nil {
		return e.errorf("%v", err)
	}
	a.store.Log(state.Event{Kind: "grab", Resource: strings.Join(names, ","), Tree: t.Path, Label: t.Label, Detail: *note})
	for _, d := range displaced {
		e.printf("dibs: %s: displaced %s\n", d.Resource, holderText(&d.Holder))
	}
	e.printf("dibs: pinned %s for %s until `dibs drop`\n", strings.Join(names, ", "), t.Label)
	containers := a.split(names)
	if *noSwap || len(containers) == 0 {
		return exitOK
	}
	sw, err := a.swapper(ctx)
	if err != nil {
		return e.errorf("%v", err)
	}
	code := exitOK
	for _, name := range containers {
		outcome, err := sw.Serve(ctx, name, t)
		if outcome != nil {
			a.report(outcome)
		}
		if err != nil {
			e.errorf("%v", err)
			code = exitError
		}
	}
	return code
}

func runDrop(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "drop", "[resource|group]...")
	positional, err := parse(fs, args)
	if err != nil {
		return exitError
	}
	var dropped []string
	if err := a.store.Update(func(st *state.State) error {
		dropped = queue.Drop(st, a.cfg.Expand(positional), time.Now())
		return nil
	}); err != nil {
		return e.errorf("%v", err)
	}
	if len(dropped) == 0 {
		e.printf("dibs: no manual pins to drop\n")
		return exitNo
	}
	a.store.Log(state.Event{Kind: "drop", Resource: strings.Join(dropped, ",")})
	e.printf("dibs: dropped pins on %s\n", strings.Join(dropped, ", "))
	return exitOK
}
