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
	"github.com/syednabilashraf/dibs/internal/guard"
	"github.com/syednabilashraf/dibs/internal/preflight"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/swap"
	"github.com/syednabilashraf/dibs/internal/tree"
)

const idleNotice = 5 * time.Minute

func contextWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

func runTake(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "take", "<resource|group>... [--wait] [--timeout DURATION] [--lease 30m] [--no-swap] [--allow-migrations] [--tree DIR]")
	wait := fs.Bool("wait", false, "queue until granted, then swap (run it in the background)")
	timeout := fs.Duration("timeout", a.cfg.WaitTimeout.D(), "give up waiting after this long, losing your place in line (default: wait until granted)")
	lease := fs.Duration("lease", a.cfg.Lease.D(), "lease length")
	noSwap := fs.Bool("no-swap", false, "hold the lock without recreating containers")
	allowMigrations := fs.Bool("allow-migrations", false, "take even if a container's migrations check objects to your worktree")
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
	if !*noSwap && !*allowMigrations {
		if refusals := a.takePreflight(ctx, t, a.split(names)); len(refusals) > 0 {
			a.refuseTake(refusals, positional)
			return exitNo
		}
	}

	before := a.heldNow(t)
	var deadline time.Time
	if *timeout > 0 {
		deadline = time.Now().Add(*timeout)
	}
	last := ""
	warned := false
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
		message := a.describeWait(result, time.Now())
		if !*wait {
			e.printf("dibs: not granted: %s\n", message)
			e.printf("dibs: run `dibs take %s --wait` in the background to queue\n", strings.Join(positional, " "))
			return exitNo
		}
		if message != last {
			e.printf("dibs: waiting: %s\n", message)
			last = message
		}
		if !warned {
			warned = true
			if note := timeoutWarning(result, deadline); note != "" {
				e.printf("%s\n", note)
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			a.giveBack(t, names, before)
			a.store.Log(state.Event{Kind: "abandon", Resource: strings.Join(names, ","), Tree: t.Path, Label: t.Label, Detail: "gave up after " + fmtDuration(*timeout)})
			e.printf("dibs: gave up after %s, so your place in line is gone. Run the take again without --timeout to wait until it is granted.\n", fmtDuration(*timeout))
			return exitNo
		}
		select {
		case <-ctx.Done():
			if returned := a.giveBack(t, names, before); len(returned) > 0 {
				e.printf("dibs: stopped waiting; %s had just been granted, and is given back untouched\n", strings.Join(returned, ", "))
			} else {
				e.printf("dibs: stopped waiting\n")
			}
			return exitNo
		case <-time.After(time.Second):
		}
	}
	defer a.settleTake(t)

	a.store.Log(state.Event{Kind: "take", Resource: strings.Join(names, ","), Tree: t.Path, Label: t.Label})
	expires := time.Now().Add(*lease)
	e.printf("dibs: %s holds %s until %s\n", t.Label, strings.Join(names, ", "), clock(expires))

	containers := a.split(names)
	if len(containers) == 0 {
		return exitOK
	}
	sw, err := a.swapper(ctx, t.Path)
	if err != nil {
		returned := a.giveBack(t, names, before)
		return e.errorf("%v (gave back %s)", err, strings.Join(returned, ", "))
	}
	if *noSwap {
		for _, name := range containers {
			a.describe(ctx, sw, name)
		}
		return exitOK
	}
	var failed, starting []string
	for _, name := range containers {
		if ctx.Err() != nil {
			return a.stopTake(t, names, before, fmt.Errorf("stopped before %s was touched", name))
		}
		if !*allowMigrations {
			if refusals := a.takePreflight(ctx, t, []string{name}); len(refusals) > 0 {
				returned := a.giveBack(t, names, before)
				a.refuseTake(refusals, positional)
				e.printf("dibs: the check passed when you queued, but not now that it is your turn; gave back %s\n", strings.Join(returned, ", "))
				return exitNo
			}
		}
		outcome, err := sw.Serve(ctx, name, t)
		if outcome != nil {
			a.report(outcome)
		}
		switch {
		case err == nil:
		case errors.Is(err, swap.ErrInterrupted) || ctx.Err() != nil:
			return a.stopTake(t, names, before, err)
		case errors.Is(err, swap.ErrNotReady):
			e.errorf("%v", err)
			failed = append(failed, name)
		case errors.Is(err, swap.ErrStillStarting):
			e.errorf("%v", err)
			starting = append(starting, name)
		default:
			returned := a.giveBack(t, names, before)
			return e.errorf("%v\ndibs: gave back %s", err, strings.Join(returned, ", "))
		}
	}
	if len(failed) > 0 || len(starting) > 0 {
		e.printf("dibs: you still hold %s, so others wait until you pass or your lease ends.\n", strings.Join(names, ", "))
		if len(failed) > 0 {
			e.printf("dibs: %s failed to start on your worktree: fix it (for example install the dependencies your branch needs inside the container), then run `dibs take %s` again, which re-checks it without recreating it. Or run `dibs pass`: a container that failed to start goes back on the baseline when passed.\n", strings.Join(failed, ", "), strings.Join(failed, " "))
		}
		if len(starting) > 0 {
			e.printf("dibs: %s is running your worktree but is not ready yet. It keeps starting on its own; run `dibs take %s` again in a while to re-check it without recreating it, and look at `docker logs` if it never gets there.\n", strings.Join(starting, ", "), strings.Join(starting, " "))
		}
		return exitError
	}
	a.warnIfBaseline(t)
	e.printf("dibs: ready. Run `dibs check` after each test batch and `dibs pass` when done testing.\n")
	return exitOK
}

func (a *app) takePreflight(ctx context.Context, t *tree.Tree, containers []string) []string {
	st, err := a.store.Read()
	if err != nil {
		return nil
	}
	refusals := []string{}
	for _, name := range containers {
		if r := st.Lookup(name); r != nil && r.Serving == t.Path && r.Status == state.StatusReady {
			continue
		}
		if result, checked := a.checkMigrations(ctx, name, t.Path, preflight.Take); checked && !result.Passed {
			refusals = append(refusals, fmt.Sprintf("%s: on your worktree %s", name, result.Explain()))
		}
	}
	return refusals
}

func (a *app) refuseTake(refusals []string, positional []string) {
	a.printf("dibs: not taken. Starting these containers on your worktree would change the shared stores every session uses:\n")
	for _, refusal := range refusals {
		a.printf("  - %s\n", refusal)
	}
	a.printf("dibs: ask the user before applying migrations to a shared store. To take anyway, run `dibs take %s --allow-migrations`.\n", strings.Join(positional, " "))
}

func (a *app) warnIfBaseline(t *tree.Tree) {
	st, err := a.store.Read()
	if err != nil {
		return
	}
	if _, ok := guard.BaselineRoots(a.cfg, st)[t.Path]; ok {
		a.printf("dibs: note: %s is the baseline checkout, which every session without a lease also runs, so these containers were already serving it. If you are working on a change, create a worktree for it and take from there.\n", t.Path)
	}
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
			a.printf("dibs: %s: it was serving %s; commands that were still running in it and files any session copied into it are gone\n", o.Name, o.PreviousLabel)
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
	now := time.Now()
	expired, dropped := queue.Prune(st, now, state.ProcessAlive)
	for _, x := range expired {
		a.store.Log(state.Event{Kind: "expire", Resource: x.Resource, Tree: x.Holder.Tree, Label: x.Holder.Label})
	}
	for _, w := range dropped {
		a.store.Log(state.Event{Kind: "abandon", Resource: strings.Join(w.Resources, ","), Tree: w.Tree, Label: w.Label})
	}
	idle := queue.ReleaseIdle(st, now, queue.IdlePolicy{Limit: a.cfg.IdleRelease.D(), LastActive: a.store.LastActive, Alive: state.ProcessAlive})
	for _, x := range idle {
		a.store.Log(state.Event{Kind: "idle", Resource: x.Resource, Tree: x.Holder.Tree, Label: x.Holder.Label, Detail: idleDetail(x)})
	}
}

func idleDetail(x queue.Idle) string {
	labels := []string{}
	for _, w := range x.Waiters {
		labels = append(labels, w.Label)
	}
	return fmt.Sprintf("no tool calls since %s while %s waited", clock(x.Since), strings.Join(labels, ", "))
}

func (a *app) lastCall(h *state.Holder) (time.Time, bool) {
	if h == nil || h.Kind != state.KindSession {
		return time.Time{}, false
	}
	return queue.IdleSince(h, a.store.LastActive(h.Tree))
}

func (a *app) leave(treePath string) {
	a.store.Update(func(st *state.State) error {
		queue.Leave(st, treePath)
		return nil
	})
}

func (a *app) release(t *tree.Tree, names []string) {
	var released []string
	a.store.Update(func(st *state.State) error {
		released = queue.Pass(st, t.Path, names, time.Now())
		return nil
	})
	if len(released) > 0 {
		a.store.Log(state.Event{Kind: "pass", Resource: strings.Join(released, ","), Tree: t.Path, Label: t.Label})
	}
}

func (a *app) setStatusIf(name, containerID string, from, to state.Status) {
	a.store.Update(func(st *state.State) error {
		if r := st.Lookup(name); r != nil && r.ContainerID == containerID && r.Status == from {
			r.Status = to
		}
		return nil
	})
}

func (a *app) heldNow(t *tree.Tree) map[string]bool {
	held := map[string]bool{}
	if st, err := a.store.Read(); err == nil {
		for _, name := range st.HeldBy(t.Path, time.Now()) {
			held[name] = true
		}
	}
	return held
}

func (a *app) giveBack(t *tree.Tree, names []string, before map[string]bool) []string {
	var returned []string
	a.store.Update(func(st *state.State) error {
		queue.Leave(st, t.Path)
		acquired := []string{}
		for _, name := range names {
			if !before[name] {
				acquired = append(acquired, name)
			}
		}
		if len(acquired) > 0 {
			returned = queue.Pass(st, t.Path, acquired, time.Now())
		}
		return nil
	})
	if len(returned) > 0 {
		a.store.Log(state.Event{Kind: "pass", Resource: strings.Join(returned, ","), Tree: t.Path, Label: t.Label})
	}
	return returned
}

func (a *app) settleTake(t *tree.Tree) {
	a.store.Update(func(st *state.State) error {
		queue.Settle(st, t.Path, os.Getpid(), time.Now())
		return nil
	})
}

func (a *app) stopTake(t *tree.Tree, names []string, before map[string]bool, cause error) int {
	returned := a.giveBack(t, names, before)
	a.printf("dibs: take stopped: %v\n", cause)
	if len(returned) > 0 {
		a.printf("dibs: gave back %s. Containers already switched keep running your worktree until the next holder takes them; one that was still starting finishes on its own.\n", strings.Join(returned, ", "))
	}
	if len(returned) < len(names) {
		kept := []string{}
		for _, name := range names {
			if before[name] {
				kept = append(kept, name)
			}
		}
		if len(kept) > 0 {
			a.printf("dibs: you still hold %s, which you held before this take\n", strings.Join(kept, ", "))
		}
	}
	return exitNo
}

func (a *app) describeWait(result queue.Result, now time.Time) string {
	parts := []string{}
	for _, b := range result.Blockers {
		part := fmt.Sprintf("%s held by %s since %s", b.Resource, holderText(&b.Holder), clock(b.Holder.Since))
		switch {
		case b.Holder.OnHold():
			part += fmt.Sprintf(" (held until %s)", clock(b.Holder.Expires))
		case !b.Holder.Pinned() && !b.Holder.Expires.IsZero():
			detail := "lease ends " + clock(b.Holder.Expires)
			if since, ok := a.lastCall(&b.Holder); ok && now.Sub(since) >= idleNotice {
				detail += fmt.Sprintf("; no tool calls since %s", clock(since))
				if limit := a.cfg.IdleRelease.D(); limit > 0 {
					detail += fmt.Sprintf(", released at %s if that lasts", clock(since.Add(limit)))
				}
			}
			part += " (" + detail + ")"
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

func timeoutWarning(result queue.Result, deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	reasons := []string{}
	if n := len(result.Ahead); n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d session(s) ahead of you want the same resources", n))
	}
	var latest time.Time
	for _, b := range result.Blockers {
		switch {
		case b.Holder.Pinned():
			reasons = append(reasons, fmt.Sprintf("a human pinned %s", b.Resource))
		case b.Holder.Expires.After(deadline) && b.Holder.Expires.After(latest):
			latest = b.Holder.Expires
		}
	}
	if !latest.IsZero() {
		reasons = append(reasons, "the lease you are waiting for runs until "+clock(latest))
	}
	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("dibs: note: %s, and your --timeout runs out at %s, so you may give up before your turn and lose your place in line. Leave out --timeout to wait until granted.", strings.Join(reasons, "; "), clock(deadline))
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
	var released, failed []string
	if err := a.store.Update(func(st *state.State) error {
		a.prune(st)
		now := time.Now()
		released = queue.Pass(st, t.Path, names, now)
		if len(names) == 0 {
			queue.Leave(st, t.Path)
		}
		for _, name := range released {
			r := st.Lookup(name)
			if !r.Virtual && r.Status == state.StatusFailed && r.Serving == t.Path && r.ActiveHolder(now) == nil {
				failed = append(failed, name)
			}
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
	toRestore := failed
	if *restore {
		toRestore = a.split(released)
	} else if len(failed) > 0 {
		e.printf("dibs: %s failed to start on your worktree, so it goes back on the baseline\n", strings.Join(failed, ", "))
	}
	if len(toRestore) == 0 {
		return exitOK
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	return a.restore(ctx, t, toRestore, false)
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
		if client == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			client, err = docker.New(ctx)
			cancel()
			if err != nil {
				return e.errorf("%v", err)
			}
		}
		if r.Status == state.StatusStarting || r.Status == state.StatusFailed {
			sw := &swap.Swapper{Docker: client, Store: a.store, Config: a.cfg}
			status, probeErr := sw.Probe(context.Background(), name, r.ContainerID)
			if status == state.StatusReady {
				a.setStatusIf(name, r.ContainerID, r.Status, status)
				r.Status = status
			} else if probeErr != nil {
				problems = append(problems, fmt.Sprintf("%s: failed to start: %v", name, probeErr))
				continue
			}
		}
		if r.Status != state.StatusReady {
			problems = append(problems, fmt.Sprintf("%s: %s, not ready yet", name, statusText(r)))
			continue
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

func runHold(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "hold", "--note TEXT [--for 30m] [--tree DIR]")
	length := fs.Duration("for", a.cfg.Hold.D(), fmt.Sprintf("how long to keep the leases without tool calls (at most %s in a row)", fmtDuration(a.cfg.HoldMax.D())))
	noteFlag := fs.String("note", "", "why, shown to everyone waiting (required)")
	treeFlag := fs.String("tree", "", "worktree to act for (default: current directory)")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	note := strings.TrimSpace(*noteFlag)
	if note == "" {
		fs.Usage()
		return e.errorf("say why with --note; everyone waiting sees it")
	}
	if *length <= 0 {
		return e.errorf("--for must be positive")
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	var held queue.Held
	if err := a.store.Update(func(st *state.State) error {
		a.prune(st)
		held = queue.Hold(st, t.Path, note, *length, a.cfg.HoldMax.D(), time.Now())
		return nil
	}); err != nil {
		return e.errorf("%v", err)
	}
	if len(held.AtLimit) > 0 {
		e.printf("dibs: %s already held for the longest allowed (%s); it ends when its lease does. Pass it, or take it again once you are testing.\n", strings.Join(held.AtLimit, ", "), fmtDuration(a.cfg.HoldMax.D()))
	}
	if len(held.Resources) == 0 {
		if len(held.AtLimit) == 0 {
			e.printf("dibs: %s holds no leases\n", t.Label)
		}
		return exitNo
	}
	a.store.Log(state.Event{Kind: "hold", Resource: strings.Join(held.Resources, ","), Tree: t.Path, Label: t.Label, Detail: note})
	e.printf("dibs: holding %s until %s; everyone waiting sees: %s\n", strings.Join(held.Resources, ", "), clock(held.Until), note)
	e.printf("dibs: run `dibs pass` as soon as it is no longer needed. Holds last at most %s in a row; the lease ends when the hold does.\n", fmtDuration(a.cfg.HoldMax.D()))
	return exitOK
}

func runRestore(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "restore", "<container|group>... | --all [--allow-migrations]")
	all := fs.Bool("all", false, "every container dibs has managed")
	allow := fs.Bool("allow-migrations", false, "restore even if a container's migrations check objects")
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
	return a.restore(ctx, caller, names, *allow)
}

func (a *app) restore(ctx context.Context, caller *tree.Tree, names []string, allowMigrations bool) int {
	if len(names) == 0 {
		return exitOK
	}
	actor := ""
	if caller != nil {
		actor = caller.Path
	}
	sw, err := a.swapper(ctx, actor)
	if err != nil {
		return a.errorf("%v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		return a.errorf("%v", err)
	}
	code := exitOK
	synced, told := map[string]bool{}, map[string]bool{}
	for _, name := range names {
		if h := st.Lookup(name).ActiveHolder(time.Now()); h != nil && (caller == nil || h.Tree != caller.Path) {
			a.printf("dibs: %s: skipped, held by %s\n", name, holderText(h))
			code = exitNo
			continue
		}
		if root := a.baselineRoot(ctx, sw, st, name); root != "" {
			a.autoSync(ctx, st, root, synced)
			if !allowMigrations && !a.restorePreflight(ctx, st, name, root, told) {
				code = exitNo
				continue
			}
		}
		holder, ok := a.holdForRestore(name, caller)
		if !ok {
			a.printf("dibs: %s: skipped, someone in line is about to take it\n", name)
			code = exitNo
			continue
		}
		outcome, err := sw.Serve(ctx, name, nil)
		if outcome != nil {
			a.report(outcome)
		}
		if holder != "" {
			a.store.Update(func(st *state.State) error {
				queue.Pass(st, holder, []string{name}, time.Now())
				return nil
			})
		}
		if err != nil {
			a.errorf("%v", err)
			code = exitError
			continue
		}
	}
	return code
}

func (a *app) holdForRestore(name string, caller *tree.Tree) (string, bool) {
	holder, label := "dibs-restore", "dibs restore"
	if caller != nil {
		holder, label = caller.Path, caller.Label
	}
	mine, granted := false, false
	a.store.Update(func(st *state.State) error {
		if h := st.Lookup(name).ActiveHolder(time.Now()); h != nil && h.Tree == holder {
			mine = true
			return nil
		}
		result, err := queue.Take(st, queue.Request{
			Tree: holder, Label: label, Resources: []string{name},
			Lease: a.cfg.ReadyTimeout.D() + 10*time.Minute, PID: os.Getpid(),
		}, state.ProcessAlive)
		granted = err == nil && result.Outcome == queue.Granted
		return nil
	})
	switch {
	case mine:
		return "", true
	case granted:
		return holder, true
	}
	return "", false
}

func (a *app) baselineRoot(ctx context.Context, sw *swap.Swapper, st *state.State, name string) string {
	if r := st.Lookup(name); r != nil && len(r.BaselineRoots) > 0 {
		return r.BaselineRoots[0]
	}
	if meta, err := sw.Describe(ctx, name); err == nil && len(meta.BaselineRoots) > 0 {
		return meta.BaselineRoots[0]
	}
	return ""
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
	sw, err := a.swapper(ctx, t.Path)
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
