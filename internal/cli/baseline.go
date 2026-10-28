package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/baseline"
	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/preflight"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/swap"
	"github.com/syednabilashraf/dibs/internal/tree"
)

type baselineRepo struct {
	path   string
	common string
	config config.Baseline
}

func (a *app) baselines(st *state.State) []baselineRepo {
	seen := map[string]bool{}
	repos := []baselineRepo{}
	add := func(path string, cfg config.Baseline) {
		t, err := tree.Resolve(path)
		if err != nil || seen[t.Path] {
			return
		}
		seen[t.Path] = true
		repos = append(repos, baselineRepo{path: t.Path, common: t.Common, config: cfg})
	}
	for _, repo := range a.cfg.Repos {
		add(repo.Path, repo.Baseline)
	}
	for _, name := range st.Names() {
		for _, root := range st.Resources[name].BaselineRoots {
			add(root, config.Baseline{Sync: config.SyncManual})
		}
	}
	return repos
}

func (a *app) baselineAt(st *state.State, root string) baselineRepo {
	for _, repo := range a.baselines(st) {
		if repo.path == root {
			return repo
		}
	}
	return baselineRepo{path: root, config: config.Baseline{Sync: config.SyncManual}}
}

func (a *app) pickBaseline(st *state.State, dir string) (baselineRepo, error) {
	repos := a.baselines(st)
	if len(repos) == 0 {
		return baselineRepo{}, errors.New("dibs knows no baseline checkout yet: list the main checkout under repos in the dibs config, or take a container first")
	}
	if t, err := resolveTree(dir); err == nil {
		for _, repo := range repos {
			if repo.common == t.Common {
				return repo, nil
			}
		}
	}
	if dir == "" && len(repos) == 1 {
		return repos[0], nil
	}
	paths := []string{}
	for _, repo := range repos {
		paths = append(paths, repo.path)
	}
	return baselineRepo{}, fmt.Errorf("choose a baseline with --repo: %s", strings.Join(paths, ", "))
}

func servingBaseline(st *state.State, root string) []string {
	names := []string{}
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Virtual || r.Serving != "" {
			continue
		}
		for _, candidate := range r.BaselineRoots {
			if candidate == root {
				names = append(names, name)
				break
			}
		}
	}
	return names
}

func runBaseline(e *env, args []string) int {
	if len(args) > 0 && args[0] == "sync" {
		return runBaselineSync(e, args[1:])
	}
	if len(args) > 0 && args[0] == "status" {
		args = args[1:]
	}
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "baseline", "[status] [--repo DIR] [--fetch] | sync [--repo DIR] [--stash] [--allow-migrations] [--no-fetch]")
	repoFlag := fs.String("repo", "", "any checkout of the repository (default: current directory)")
	fetch := fs.Bool("fetch", false, "fetch the branch the baseline follows first")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	st, err := a.store.Read()
	if err != nil {
		return e.errorf("%v", err)
	}
	repo, err := a.pickBaseline(st, *repoFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	status, err := baseline.Inspect(ctx, repo.path, repo.config)
	if err != nil {
		return e.errorf("%v", err)
	}
	if *fetch {
		if err := baseline.Fetch(ctx, repo.path, status.Remote, status.Target); err != nil {
			return e.errorf("%v", err)
		}
		if status, err = baseline.Inspect(ctx, repo.path, repo.config); err != nil {
			return e.errorf("%v", err)
		}
	}
	a.writeBaseline(st, repo, status)
	return exitOK
}

func (a *app) writeBaseline(st *state.State, repo baselineRepo, status baseline.Status) {
	branch := status.Branch
	if branch == "" {
		branch = "detached HEAD"
	}
	a.printf("baseline %s: %s at %s\n", repo.path, branch, status.Head)
	position := "up to date with " + status.TargetRef()
	if status.Behind > 0 {
		position = fmt.Sprintf("%d commit(s) behind %s", status.Behind, status.TargetRef())
	}
	if status.Ahead > 0 {
		position += fmt.Sprintf(", %d ahead", status.Ahead)
	}
	if !status.Fetched.IsZero() {
		position += fmt.Sprintf(" (%s last fetched %s)", status.TargetRef(), status.Fetched.Local().Format("01-02 15:04"))
	}
	a.printf("  %s\n", position)
	if len(status.Dirty) > 0 {
		a.printf("  local changes: %s\n", strings.Join(status.Dirty, ", "))
	}
	now := time.Now()
	serving := []string{}
	for _, name := range servingBaseline(st, repo.path) {
		if h := st.Resources[name].ActiveHolder(now); h != nil {
			serving = append(serving, fmt.Sprintf("%s (held by %s)", name, holderText(h)))
		} else {
			serving = append(serving, name)
		}
	}
	if len(serving) > 0 {
		a.printf("  serving it: %s\n", strings.Join(serving, ", "))
	}
	if status.Behind > 0 {
		a.printf("  `dibs baseline sync` fast-forwards it to %s when that is safe (sync: %s)\n", status.TargetRef(), repo.config.Sync)
	}
}

type syncOptions struct {
	fetch           bool
	stash           bool
	allowMigrations bool
}

type syncResult struct {
	synced    bool
	upToDate  bool
	from, to  string
	commits   int
	obstacles []string
}

func runBaselineSync(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "baseline sync", "[--repo DIR] [--stash] [--allow-migrations] [--no-fetch]")
	repoFlag := fs.String("repo", "", "any checkout of the repository (default: current directory)")
	stash := fs.Bool("stash", false, "stash local changes in the baseline checkout first (kept in git stash)")
	allow := fs.Bool("allow-migrations", false, "fast-forward even if a migrations check objects")
	noFetch := fs.Bool("no-fetch", false, "use the remote branch as last fetched")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	st, err := a.store.Read()
	if err != nil {
		return e.errorf("%v", err)
	}
	repo, err := a.pickBaseline(st, *repoFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	result, err := a.syncBaseline(ctx, repo, syncOptions{fetch: !*noFetch, stash: *stash, allowMigrations: *allow})
	if err != nil {
		return e.errorf("%v", err)
	}
	return a.reportSync(repo, result)
}

func (a *app) reportSync(repo baselineRepo, result syncResult) int {
	switch {
	case result.upToDate:
		a.printf("dibs: baseline %s is up to date (%s)\n", repo.path, result.from)
		return exitOK
	case len(result.obstacles) > 0:
		a.printf("dibs: baseline %s was not fast-forwarded:\n", repo.path)
		for _, reason := range result.obstacles {
			a.printf("  - %s\n", reason)
		}
		return exitNo
	default:
		a.printf("dibs: baseline %s fast-forwarded from %s to %s (%d commits). Containers serving it run the new code as they reload or restart.\n", repo.path, result.from, result.to, result.commits)
		return exitOK
	}
}

func (a *app) syncBaseline(ctx context.Context, repo baselineRepo, opts syncOptions) (syncResult, error) {
	status, err := baseline.Inspect(ctx, repo.path, repo.config)
	if err != nil {
		return syncResult{}, err
	}
	if opts.fetch {
		if err := baseline.Fetch(ctx, repo.path, status.Remote, status.Target); err != nil {
			return syncResult{}, err
		}
		if status, err = baseline.Inspect(ctx, repo.path, repo.config); err != nil {
			return syncResult{}, err
		}
	}
	result := syncResult{from: status.Head, commits: status.Behind}
	if status.Behind == 0 {
		result.upToDate = true
		return result, nil
	}
	result.obstacles = status.Obstacles(opts.stash)
	st, err := a.store.Read()
	if err != nil {
		return result, err
	}
	result.obstacles = append(result.obstacles, a.leasesOnBaseline(st, repo.path)...)
	if len(result.obstacles) > 0 {
		return result, nil
	}
	target, err := baseline.Resolve(ctx, repo.path, status.TargetRef())
	if err != nil {
		return result, err
	}
	if !opts.allowMigrations {
		result.obstacles = append(result.obstacles, a.syncPreflight(ctx, st, repo.path, target)...)
		if len(result.obstacles) > 0 {
			result.obstacles = append(result.obstacles, "pass --allow-migrations to fast-forward anyway")
			return result, nil
		}
	}
	if len(status.Dirty) > 0 {
		message := "dibs baseline sync " + time.Now().Format("2006-01-02 15:04:05")
		if err := baseline.Stash(ctx, repo.path, message); err != nil {
			return result, err
		}
		a.printf("dibs: stashed local changes in %s as %q (`git stash list` shows them)\n", repo.path, message)
	}
	err = a.store.Update(func(st *state.State) error {
		if blocked := a.leasesOnBaseline(st, repo.path); len(blocked) > 0 {
			result.obstacles = blocked
			return nil
		}
		return baseline.FastForward(ctx, repo.path, status.TargetRef())
	})
	if err != nil || len(result.obstacles) > 0 {
		return result, err
	}
	result.synced = true
	if result.to, err = baseline.Resolve(ctx, repo.path, "HEAD"); err == nil {
		result.to = swap.ShortID(result.to)
	}
	a.store.Log(state.Event{Kind: "sync", Tree: repo.path, Label: treeName(repo.path), Detail: fmt.Sprintf("%s..%s (%d commits)", result.from, result.to, result.commits)})
	a.afterSync(ctx, repo, target)
	return result, nil
}

func (a *app) leasesOnBaseline(st *state.State, root string) []string {
	obstacles := []string{}
	now := time.Now()
	for _, name := range servingBaseline(st, root) {
		r := st.Resources[name]
		switch {
		case r.ActiveHolder(now) != nil:
			obstacles = append(obstacles, fmt.Sprintf("%s runs the baseline under a lease held by %s; sync once it is passed", name, holderText(r.ActiveHolder(now))))
		case r.Status == state.StatusSwapping && r.Intent != nil && state.ProcessAlive(r.Intent.PID):
			obstacles = append(obstacles, fmt.Sprintf("%s is being recreated on the baseline right now", name))
		}
	}
	return obstacles
}

func (a *app) syncPreflight(ctx context.Context, st *state.State, root, rev string) []string {
	failures := []string{}
	var client *docker.Client
	for _, name := range sortedKeys(a.cfg.Containers) {
		migrations := a.cfg.Containers[name].Migrations
		if migrations.Check == "" {
			continue
		}
		serves, err := a.servesBaseline(ctx, st, &client, name, root)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: cannot tell whether it runs the baseline (%v)", name, err))
			continue
		}
		if !serves {
			continue
		}
		if len(migrations.Paths) == 0 {
			failures = append(failures, fmt.Sprintf("%s: its migrations check cannot judge a revision that is not checked out; list the files it reads under containers.%s.migrations.paths", name, name))
			continue
		}
		dir, err := os.MkdirTemp("", "dibs-sync-")
		if err != nil {
			return append(failures, err.Error())
		}
		result := preflight.Result{Container: name}
		if err := baseline.Export(ctx, root, rev, migrations.Paths, dir); err != nil {
			result.Err = err
		} else {
			result = preflight.Run(ctx, migrations.Check, preflight.Target{Container: name, Tree: dir, Rev: rev, Action: preflight.Sync})
		}
		os.RemoveAll(dir)
		if !result.Passed {
			failures = append(failures, fmt.Sprintf("%s runs the baseline, and on %s %s", name, swap.ShortID(rev), result.Explain()))
		}
	}
	return failures
}

func (a *app) servesBaseline(ctx context.Context, st *state.State, client **docker.Client, name, root string) (bool, error) {
	if r := st.Lookup(name); r != nil && len(r.BaselineRoots) > 0 {
		if r.Serving != "" {
			return false, nil
		}
		for _, candidate := range r.BaselineRoots {
			if candidate == root {
				return true, nil
			}
		}
		return false, nil
	}
	if *client == nil {
		c, err := docker.New(ctx)
		if err != nil {
			return false, err
		}
		*client = c
	}
	raw, err := (*client).Inspect(ctx, name)
	if errors.Is(err, docker.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	mapper := tree.NewMapper()
	for _, source := range swap.BindSources(raw) {
		if owner := mapper.Owner(source); owner != nil && owner.Path == root {
			return true, nil
		}
	}
	return false, nil
}

func (a *app) afterSync(ctx context.Context, repo baselineRepo, rev string) {
	for _, command := range repo.config.AfterSync {
		a.printf("dibs: after sync: %s\n", command)
		stepCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		cmd := exec.CommandContext(stepCtx, "sh", "-c", command)
		cmd.Dir = repo.path
		cmd.Env = append(os.Environ(), "PWD="+repo.path, "DIBS_TREE="+repo.path, "DIBS_REV="+rev, "DIBS_ACTION="+preflight.Sync)
		cmd.Stdout, cmd.Stderr = a.stdout, a.stdout
		if err := cmd.Run(); err != nil {
			a.printf("dibs: after sync: %q failed: %v\n", command, err)
		}
		cancel()
	}
}

func (a *app) restorePreflight(ctx context.Context, st *state.State, name, root string, told map[string]bool) bool {
	repo := a.baselineAt(st, root)
	status, statusErr := baseline.Inspect(ctx, root, repo.config)
	behind := ""
	if statusErr == nil && status.Behind > 0 {
		behind = fmt.Sprintf("The baseline checkout %s is %d commit(s) behind %s; `dibs baseline sync` fast-forwards it.", root, status.Behind, status.TargetRef())
	}
	check := a.cfg.ContainerConfig(name).Migrations.Check
	if check == "" {
		if behind != "" && !told[root] {
			told[root] = true
			a.printf("dibs: note: %s If a restored container fails to start because of that, sync first.\n", behind)
		}
		return true
	}
	result, _ := a.checkMigrations(ctx, name, root, preflight.Restore)
	if result.Passed {
		return true
	}
	a.printf("dibs: %s: not restored: on the baseline %s\n", name, result.Explain())
	if behind != "" {
		a.printf("dibs: %s\n", behind)
	}
	a.printf("dibs: to restore it anyway, run `dibs restore %s --allow-migrations`\n", name)
	return false
}

func (a *app) checkMigrations(ctx context.Context, name, dir, action string) (preflight.Result, bool) {
	check := a.cfg.ContainerConfig(name).Migrations.Check
	if check == "" {
		return preflight.Result{Container: name, Passed: true}, false
	}
	rev, _ := baseline.Resolve(ctx, dir, "HEAD")
	return preflight.Run(ctx, check, preflight.Target{Container: name, Tree: dir, Rev: rev, Action: action}), true
}

func (a *app) autoSync(ctx context.Context, st *state.State, root string, done map[string]bool) {
	repo := a.baselineAt(st, root)
	if repo.config.Sync != config.SyncAuto || done[root] {
		return
	}
	done[root] = true
	result, err := a.syncBaseline(ctx, repo, syncOptions{fetch: true})
	switch {
	case err != nil:
		a.printf("dibs: baseline sync skipped: %v\n", err)
	case result.synced:
		a.printf("dibs: baseline %s fast-forwarded from %s to %s (%d commits) before restoring\n", root, result.from, result.to, result.commits)
	case len(result.obstacles) > 0:
		a.printf("dibs: baseline %s is %d commit(s) behind and was not fast-forwarded: %s\n", root, result.commits, strings.Join(result.obstacles, "; "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
