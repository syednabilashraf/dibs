package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/swap"
	"github.com/syednabilashraf/dibs/internal/tree"
)

var Version = "dev"

const (
	exitOK    = 0
	exitError = 1
	exitNo    = 2
)

type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type command struct {
	name    string
	summary string
	run     func(e *env, args []string) int
}

func commands() []command {
	return []command{
		{"take", "take containers or virtual locks for this worktree (--wait to queue)", runTake},
		{"pass", "give back what this worktree holds (--restore to put containers back to baseline)", runPass},
		{"check", "exit 0 if this worktree still holds its resources and the containers still serve it", runCheck},
		{"renew", "extend this worktree's leases", runRenew},
		{"restore", "recreate containers from their baseline (--all for every managed container)", runRestore},
		{"grab", "pin resources for a human, displacing any session (no expiry)", runGrab},
		{"drop", "release manual pins", runDrop},
		{"status", "show holders, what each container serves, the queue and recent swaps", runStatus},
		{"line", "show the queue", runLine},
		{"tmpdir", "print a scratch path unique to this worktree, for copying code into containers", runTmpdir},
		{"guard", "Claude Code PreToolUse hook (--post for PostToolUse): guard shared containers and explain swaps", runGuard},
		{"context", "Claude Code SessionStart hook: explain the shared containers and who holds what", runContext},
		{"version", "print the version", runVersion},
	}
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return exitOK
	}
	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(e, args[1:])
		}
	}
	fmt.Fprintf(stderr, "dibs: unknown command %q\n\n", args[0])
	usage(stderr)
	return exitError
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "dibs: share local Docker containers between git worktrees, one holder at a time")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: dibs <command> [flags] [args]")
	fmt.Fprintln(w)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-12s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Config: %s   State: %s\n", config.Path(), config.Home())
	fmt.Fprintln(w, "Exit codes: 0 yes, 2 no, 1 error.")
}

func runVersion(e *env, args []string) int {
	fmt.Fprintln(e.stdout, Version)
	return exitOK
}

func (e *env) errorf(format string, args ...any) int {
	fmt.Fprintf(e.stderr, "dibs: "+format+"\n", args...)
	return exitError
}

func (e *env) printf(format string, args ...any) {
	fmt.Fprintf(e.stdout, format, args...)
}

type app struct {
	*env
	cfg   *config.Config
	store *state.Store
}

func (e *env) app() (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(config.Home())
	if err != nil {
		return nil, err
	}
	return &app{env: e, cfg: cfg, store: store}, nil
}

func (a *app) swapper(ctx context.Context, actor string) (*swap.Swapper, error) {
	client, err := docker.New(ctx)
	if err != nil {
		return nil, err
	}
	return &swap.Swapper{Docker: client, Store: a.store, Config: a.cfg, Out: a.stdout, Actor: actor}, nil
}

func (a *app) split(names []string) (containers []string) {
	for _, name := range names {
		if !a.cfg.IsVirtual(name) {
			containers = append(containers, name)
		}
	}
	return containers
}

func (a *app) validate(ctx context.Context, names []string) error {
	containers := a.split(names)
	if len(containers) == 0 {
		return nil
	}
	client, err := docker.New(ctx)
	if err != nil {
		return err
	}
	for _, name := range containers {
		if _, err := client.Inspect(ctx, name); err != nil {
			if errors.Is(err, docker.ErrNotFound) {
				return fmt.Errorf("%q is not a container or a virtual resource (virtual: %s; groups: %s)", name, strings.Join(a.cfg.Virtual, ", "), strings.Join(groupNames(a.cfg), ", "))
			}
			return err
		}
	}
	return nil
}

func groupNames(cfg *config.Config) []string {
	names := []string{}
	for name := range cfg.Groups {
		names = append(names, name)
	}
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

func resolveTree(flagValue string) (*tree.Tree, error) {
	dir := flagValue
	if dir != "" {
		dir = config.ExpandHome(dir)
	}
	return tree.Resolve(dir)
}

func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	positional := []string{}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func newFlags(e *env, name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: dibs %s %s\n", name, usage)
		fs.PrintDefaults()
	}
	return fs
}

func clock(t time.Time) string { return t.Local().Format("15:04") }

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func treeName(path string) string {
	if path == "" {
		return "baseline"
	}
	return filepath.Base(path)
}

func servingText(r *state.Resource) string {
	if r.Virtual {
		return "-"
	}
	if r.Serving == "" {
		return "baseline"
	}
	if r.ServingLabel != "" {
		return fmt.Sprintf("%s (%s)", r.ServingLabel, treeName(r.Serving))
	}
	return treeName(r.Serving)
}

func holderText(h *state.Holder) string {
	if h == nil {
		return "-"
	}
	text := fmt.Sprintf("%s (%s)", h.Label, treeName(h.Tree))
	if h.Pinned() {
		text += " [pinned"
		if h.Note != "" {
			text += ": " + h.Note
		}
		text += "]"
	}
	return text
}
