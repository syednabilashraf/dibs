package swap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

var (
	ErrNoRepoMounts = errors.New("none of its bind mounts come from this repository, so it cannot serve your worktree")
	ErrInterrupted  = errors.New("interrupted")
	ErrNotReady     = errors.New("not ready")
)

type Swapper struct {
	Docker *docker.Client
	Store  *state.Store
	Config *config.Config
	Out    io.Writer
	Actor  string

	beforeCreate func(*Spec) error
}

type Outcome struct {
	Name          string
	Swapped       bool
	Previous      string
	PreviousLabel string
	Serving       string
	ServingLabel  string
	Kept          []tree.Change
	Waited        time.Duration
	Interrupted   []string
}

type Meta struct {
	ContainerID   string
	Service       string
	Project       string
	Ports         []int
	Repo          string
	BaselineRoots []string
}

func (s *Swapper) printf(format string, args ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.Out, format, args...)
	}
}

func (s *Swapper) baselinePath(name string) string {
	return filepath.Join(s.Store.Dir, "baseline", name+".json")
}

func (s *Swapper) loadBaseline(name string) (map[string]any, error) {
	data, err := os.ReadFile(s.baselinePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse baseline for %s: %w", name, err)
	}
	return raw, nil
}

func (s *Swapper) saveBaseline(name string, raw map[string]any) error {
	path := s.baselinePath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Swapper) Baseline(ctx context.Context, name string) (map[string]any, map[string]any, error) {
	live, err := s.Docker.Inspect(ctx, name)
	missing := errors.Is(err, docker.ErrNotFound)
	if err != nil && !missing {
		return nil, nil, err
	}
	baseline, err := s.loadBaseline(name)
	if err != nil {
		return nil, nil, err
	}
	if !missing {
		summary := docker.Summarize(live)
		if summary.Label(LabelManaged) == "" {
			recorded := ""
			if st, err := s.Store.Read(); err == nil {
				if r := st.Lookup(name); r != nil {
					recorded = r.ContainerID
				}
			}
			if baseline == nil || summary.ID != recorded {
				if err := s.saveBaseline(name, live); err != nil {
					return nil, nil, err
				}
				baseline = live
			}
		}
	}
	if baseline == nil {
		if missing {
			return nil, nil, fmt.Errorf("container %s does not exist", name)
		}
		return nil, nil, fmt.Errorf("%s was recreated by dibs but its baseline snapshot is gone; recreate it with your usual tooling and retry", name)
	}
	if missing {
		live = nil
	}
	return live, baseline, nil
}

func (s *Swapper) Serve(ctx context.Context, name string, target *tree.Tree) (*Outcome, error) {
	live, baseline, err := s.Baseline(ctx, name)
	if err != nil {
		return nil, err
	}
	mapper := tree.NewMapper()
	outcome := &Outcome{Name: name}

	var summary docker.Summary
	if live != nil {
		summary = docker.Summarize(live)
		if summary.Label(LabelManaged) != "" {
			outcome.Previous = summary.Label(LabelTree)
			outcome.PreviousLabel = summary.Label(LabelBranch)
		}
	}

	spec, servingPath, servingLabel, err := s.plan(baseline, target, mapper)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	outcome.Serving, outcome.ServingLabel = servingPath, servingLabel
	outcome.Kept = spec.Kept()
	meta := s.meta(baseline, target, mapper)

	if live != nil && summary.Running && outcome.Previous == servingPath {
		meta.ContainerID = summary.ID
		s.record(name, meta, servingPath, servingLabel, state.StatusSwapping, time.Time{})
		if err := s.WaitReady(ctx, name, summary.ID); err != nil {
			s.setStatus(name, state.StatusFailed)
			return outcome, fmt.Errorf("%s: %w", name, err)
		}
		s.setStatus(name, state.StatusReady)
		return outcome, nil
	}

	if err := s.Store.Update(func(st *state.State) error {
		r := st.Get(name)
		r.Status = state.StatusSwapping
		r.Intent = &state.Intent{Target: servingPath, PID: os.Getpid(), Started: time.Now()}
		return nil
	}); err != nil {
		return nil, err
	}

	if live != nil && summary.Running {
		waited, running := s.waitExecs(ctx, name, summary.ID)
		outcome.Waited, outcome.Interrupted = waited, running
	}

	oldID := ""
	if live != nil {
		oldID = summary.ID
	}
	id, interrupted, err := s.recreate(ctx, oldID, spec)
	if err != nil {
		rollbackErr := s.rollback(ctx, name, baseline, mapper)
		if rollbackErr != nil {
			return nil, fmt.Errorf("%s: %w; rolling back to the baseline also failed: %v", name, err, rollbackErr)
		}
		return nil, fmt.Errorf("%s: %w (rolled back to the baseline)", name, err)
	}
	outcome.Swapped = true
	meta.ContainerID = id
	s.record(name, meta, servingPath, servingLabel, state.StatusSwapping, time.Now())
	s.Store.Log(state.Event{Kind: "swap", Resource: name, Tree: servingPath, Label: servingLabel, Actor: s.Actor, Detail: outcome.PreviousLabel})
	if interrupted {
		return outcome, ErrInterrupted
	}

	if err := s.WaitReady(ctx, name, id); err != nil {
		s.setStatus(name, state.StatusFailed)
		return outcome, fmt.Errorf("%s: %w", name, err)
	}
	s.setStatus(name, state.StatusReady)
	return outcome, nil
}

func (s *Swapper) plan(baseline map[string]any, target *tree.Tree, mapper *tree.Mapper) (*Spec, string, string, error) {
	if target == nil {
		spec, err := Transform(baseline, nil, map[string]string{LabelManaged: "1", LabelTree: "", LabelBranch: ""})
		return spec, "", "", err
	}
	labels := map[string]string{LabelManaged: "1", LabelTree: target.Path, LabelBranch: target.Label}
	spec, err := Transform(baseline, func(source string) tree.Change { return mapper.Map(source, target) }, labels)
	if err != nil {
		return nil, "", "", err
	}
	if spec.Mapped() > 0 {
		return spec, target.Path, target.Label, nil
	}
	for _, change := range spec.Changes {
		if change.Mapped {
			spec, err := Transform(baseline, nil, map[string]string{LabelManaged: "1", LabelTree: "", LabelBranch: ""})
			return spec, "", "", err
		}
	}
	return nil, "", "", ErrNoRepoMounts
}

func (s *Swapper) meta(baseline map[string]any, target *tree.Tree, mapper *tree.Mapper) Meta {
	summary := docker.Summarize(baseline)
	m := Meta{
		Service: summary.Label("com.docker.compose.service"),
		Project: summary.Label("com.docker.compose.project"),
		Ports:   summary.HostPorts,
	}
	if target != nil {
		m.Repo = target.Common
	}
	seen := map[string]bool{}
	for _, source := range BindSources(baseline) {
		owner := mapper.Owner(source)
		if owner == nil {
			continue
		}
		if m.Repo == "" {
			m.Repo = owner.Common
		}
		if owner.Common == m.Repo && !seen[owner.Path] {
			seen[owner.Path] = true
			m.BaselineRoots = append(m.BaselineRoots, owner.Path)
		}
	}
	return m
}

func (s *Swapper) Describe(ctx context.Context, name string) (Meta, error) {
	live, baseline, err := s.Baseline(ctx, name)
	if err != nil {
		return Meta{}, err
	}
	m := s.meta(baseline, nil, tree.NewMapper())
	if live != nil {
		m.ContainerID = docker.Summarize(live).ID
	}
	return m, nil
}

func (s *Swapper) record(name string, meta Meta, serving, label string, status state.Status, swapped time.Time) {
	s.Store.Update(func(st *state.State) error {
		r := st.Get(name)
		r.ContainerID = meta.ContainerID
		r.Service = meta.Service
		r.Project = meta.Project
		r.Ports = meta.Ports
		r.Repo = meta.Repo
		r.BaselineRoots = meta.BaselineRoots
		r.Serving = serving
		r.ServingLabel = label
		r.Status = status
		if status != state.StatusSwapping {
			r.Intent = nil
		}
		if !swapped.IsZero() {
			r.SwappedAt = swapped
		}
		return nil
	})
}

func (s *Swapper) setStatus(name string, status state.Status) {
	s.Store.Update(func(st *state.State) error {
		r := st.Get(name)
		r.Status = status
		r.Intent = nil
		return nil
	})
}

func (s *Swapper) rollback(ctx context.Context, name string, baseline map[string]any, mapper *tree.Mapper) error {
	spec, err := Transform(baseline, nil, map[string]string{LabelManaged: "1", LabelTree: "", LabelBranch: ""})
	if err != nil {
		return err
	}
	current := ""
	if live, err := s.Docker.Inspect(ctx, name); err == nil {
		current = docker.Summarize(live).ID
	}
	id, _, err := s.recreate(ctx, current, spec)
	meta := s.meta(baseline, nil, mapper)
	meta.ContainerID = id
	status := state.StatusFailed
	if err != nil {
		meta.ContainerID = ""
	}
	s.record(name, meta, "", "", status, time.Now())
	if err == nil {
		s.Store.Log(state.Event{Kind: "restore", Resource: name, Actor: s.Actor, Detail: "rollback after a failed swap"})
	}
	return err
}

func (s *Swapper) recreate(parent context.Context, oldID string, spec *Spec) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), s.Config.StopTimeout.D()+2*time.Minute)
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	interrupted := func() bool {
		select {
		case <-signals:
			return true
		default:
			return false
		}
	}

	if oldID != "" {
		if err := s.Docker.Stop(ctx, oldID, s.Config.StopTimeout.D()); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return "", interrupted(), fmt.Errorf("stop: %w", err)
		}
		if err := s.Docker.Remove(ctx, oldID); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return "", interrupted(), fmt.Errorf("remove: %w", err)
		}
	}
	if s.beforeCreate != nil {
		if err := s.beforeCreate(spec); err != nil {
			return "", interrupted(), fmt.Errorf("create: %w", err)
		}
	}
	id, err := s.Docker.Create(ctx, spec.Name, spec.Body)
	if err != nil {
		return "", interrupted(), fmt.Errorf("create: %w", err)
	}
	for network, endpoint := range spec.Networks {
		if err := s.Docker.Connect(ctx, network, id, endpoint); err != nil {
			s.Docker.Remove(ctx, id)
			return "", interrupted(), fmt.Errorf("connect %s: %w", network, err)
		}
	}
	if err := s.Docker.Start(ctx, id); err != nil {
		s.Docker.Remove(ctx, id)
		return "", interrupted(), fmt.Errorf("start: %w", err)
	}
	return id, interrupted(), nil
}

func (s *Swapper) waitExecs(ctx context.Context, name, id string) (time.Duration, []string) {
	grace := s.Config.ExecGrace.D()
	start := time.Now()
	announced := false
	for {
		running := s.runningExecs(ctx, id)
		if len(running) == 0 {
			return time.Since(start), nil
		}
		if time.Since(start) >= grace {
			if grace > 0 {
				s.printf("%s: %d exec(s) still running after %s; recreating anyway: %s\n", name, len(running), grace, strings.Join(running, "; "))
			}
			return time.Since(start), running
		}
		if !announced {
			s.printf("%s: waiting up to %s for %d running exec(s) from other sessions to finish: %s\n", name, grace, len(running), strings.Join(running, "; "))
			announced = true
		}
		select {
		case <-ctx.Done():
			return time.Since(start), running
		case <-time.After(time.Second):
		}
	}
}

func (s *Swapper) runningExecs(ctx context.Context, id string) []string {
	live, err := s.Docker.Inspect(ctx, id)
	if err != nil {
		return nil
	}
	running := []string{}
	for _, execID := range docker.Summarize(live).ExecIDs {
		ex, err := s.Docker.ExecInspect(ctx, execID)
		if err != nil || !ex.Running {
			continue
		}
		command := ex.Command()
		if len(command) > 80 {
			command = command[:77] + "..."
		}
		running = append(running, command)
	}
	return running
}

func (s *Swapper) WaitReady(ctx context.Context, name, id string) error {
	ready := s.Config.ContainerConfig(name).Ready
	var logPattern *regexp.Regexp
	if ready.Log != "" {
		compiled, err := regexp.Compile(ready.Log)
		if err != nil {
			return fmt.Errorf("containers.%s.ready.log: %w", name, err)
		}
		logPattern = compiled
	}
	settle := ready.Settle.D()
	if settle == 0 {
		settle = 3 * time.Second
	}
	probes := logPattern != nil || ready.HTTP != "" || ready.TCP != ""
	deadline := time.Now().Add(s.Config.ReadyTimeout.D())
	logSeen, httpSeen, tcpSeen := logPattern == nil, ready.HTTP == "", ready.TCP == ""
	announced := false

	for {
		raw, err := s.Docker.Inspect(ctx, id)
		if err != nil {
			return err
		}
		summary := docker.Summarize(raw)
		if !summary.Running {
			logs, _ := s.Docker.Logs(ctx, id, time.Time{}, summary.Tty)
			return fmt.Errorf("%w: exited during startup (%s); last output:\n%s", ErrNotReady, summary.Status, tail(logs, 20))
		}
		ok := true
		if summary.HasHealthcheck {
			ok = summary.Health == "healthy"
		}
		if !logSeen {
			logs, _ := s.Docker.Logs(ctx, id, time.Time{}, summary.Tty)
			logSeen = logPattern.MatchString(logs)
		}
		if !httpSeen {
			httpSeen = httpAnswers(ctx, ready.HTTP)
		}
		if !tcpSeen {
			tcpSeen = tcpAnswers(ready.TCP)
		}
		ok = ok && logSeen && httpSeen && tcpSeen
		if !summary.HasHealthcheck && !probes {
			ok = !summary.StartedAt.IsZero() && time.Since(summary.StartedAt) >= settle
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s (healthcheck=%v log=%v http=%v tcp=%v)", ErrNotReady, s.Config.ReadyTimeout.D(), summary.Health, logSeen, httpSeen, tcpSeen)
		}
		if !announced && (probes || summary.HasHealthcheck) {
			s.printf("%s: waiting for it to become ready\n", name)
			announced = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func BindSources(inspect map[string]any) []string {
	host := obj(inspect["HostConfig"])
	sources := []string{}
	for _, b := range list(host["Binds"]) {
		if source, _ := splitBind(str(b)); source != "" {
			sources = append(sources, source)
		}
	}
	for _, m := range list(host["Mounts"]) {
		mount := obj(m)
		if str(mount["Type"]) == "bind" {
			sources = append(sources, str(mount["Source"]))
		}
	}
	return sources
}

func httpAnswers(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

func tcpAnswers(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func tail(text string, lines int) string {
	parts := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func ShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
