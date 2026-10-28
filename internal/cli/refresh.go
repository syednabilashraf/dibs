package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/preflight"
	"github.com/syednabilashraf/dibs/internal/swap"
	"github.com/syednabilashraf/dibs/internal/tree"
)

func runRefresh(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "refresh", "[container...] [--all] [--allow-migrations]")
	all := fs.Bool("all", false, "restart every declared container, not only those older than the watched files")
	allowMigrations := fs.Bool("allow-migrations", false, "restart even if a container's migrations check objects")
	positional, err := parse(fs, args)
	if err != nil {
		return exitError
	}
	declared := a.cfg.Refresh.Containers
	if len(declared) == 0 {
		return e.errorf("no containers are declared under refresh.containers in %s, so there is nothing dibs may restart", config.Path())
	}
	targets := declared
	if len(positional) > 0 {
		targets = a.cfg.Expand(positional)
		for _, name := range targets {
			if !a.cfg.Refresh.Declares(name) {
				return e.errorf("%s is not declared under refresh.containers; dibs only restarts containers the config says read refreshable host state", name)
			}
		}
	}
	ctx, cancel := contextWithSignals()
	defer cancel()

	if check := a.cfg.Refresh.Check; check != "" {
		if out, err := hostCheck(ctx, check); err != nil {
			e.printf("dibs: refresh.check failed, so restarting would not help yet:\n%s\n", indentLines(out))
			e.printf("dibs: ask the user to refresh what it checks on the host first.\n")
			return exitNo
		}
	}
	changed := newest(a.cfg.Refresh.Watch)

	caller, _ := tree.Resolve("")
	actor := ""
	if caller != nil {
		actor = caller.Path
	}
	sw, err := a.swapper(ctx, actor)
	if err != nil {
		return e.errorf("%v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		return e.errorf("%v", err)
	}
	code := exitOK
	restarted := 0
	for _, name := range targets {
		raw, err := sw.Docker.Inspect(ctx, name)
		if errors.Is(err, docker.ErrNotFound) {
			e.printf("dibs: %s: skipped, no such container\n", name)
			continue
		}
		if err != nil {
			return e.errorf("%v", err)
		}
		live := docker.Summarize(raw)
		explicit := len(positional) > 0
		if !*all && !explicit && !changed.IsZero() && live.StartedAt.After(changed) {
			continue
		}
		if h := st.Lookup(name).ActiveHolder(time.Now()); h != nil && (caller == nil || h.Tree != caller.Path) {
			e.printf("dibs: %s: skipped, held by %s; it can run `dibs refresh %s` itself\n", name, holderText(h), name)
			code = exitNo
			continue
		}
		if !*allowMigrations {
			dir := live.Label(swap.LabelTree)
			if dir == "" {
				dir = a.baselineRoot(ctx, sw, st, name)
			}
			if dir != "" {
				if result, checked := a.checkMigrations(ctx, name, dir, preflight.Refresh); checked && !result.Passed {
					e.printf("dibs: %s: not restarted: it would start again on %s, and %s\n", name, dir, result.Explain())
					e.printf("dibs: ask the user before applying migrations to a shared store; `dibs refresh %s --allow-migrations` restarts it anyway\n", name)
					code = exitNo
					continue
				}
			}
		}
		holder, ok := a.holdBriefly(name, caller, "refresh")
		if !ok {
			e.printf("dibs: %s: skipped, someone in line is about to take it (a take starts it fresh anyway)\n", name)
			continue
		}
		outcome, err := sw.Restart(ctx, name)
		a.letGo(holder, name)
		if outcome != nil && outcome.Waited >= time.Second {
			e.printf("dibs: %s: waited %s for running execs\n", name, outcome.Waited.Round(time.Second))
		}
		if err != nil {
			e.errorf("%v", err)
			code = exitError
			continue
		}
		restarted++
		e.printf("dibs: %s: restarted\n", name)
	}
	if restarted == 0 && code == exitOK {
		e.printf("dibs: every declared container started after the watched files last changed; nothing to restart (use --all to restart them anyway)\n")
	}
	return code
}

func hostCheck(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func newest(paths []string) time.Time {
	var latest time.Time
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

func indentLines(text string) string {
	if text == "" {
		return "    (no output)"
	}
	return "    " + strings.ReplaceAll(text, "\n", "\n    ")
}
