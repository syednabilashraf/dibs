package swap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/state"
)

const defaultMaxRestarts = 3

type probe struct {
	ready     config.Ready
	log, fail *regexp.Regexp
	settle    time.Duration
	restarts  int
	logSeen   bool
	httpSeen  bool
	tcpSeen   bool
	since     time.Time
	tail      string
}

func (s *Swapper) newProbe(name string) (*probe, error) {
	ready := s.Config.ContainerConfig(name).Ready
	p := &probe{ready: ready, settle: ready.Settle.D(), restarts: -1}
	var err error
	if ready.Log != "" {
		if p.log, err = regexp.Compile(ready.Log); err != nil {
			return nil, fmt.Errorf("containers.%s.ready.log: %w", name, err)
		}
	}
	if ready.Fail != "" {
		if p.fail, err = regexp.Compile(ready.Fail); err != nil {
			return nil, fmt.Errorf("containers.%s.ready.fail: %w", name, err)
		}
	}
	if p.settle == 0 {
		p.settle = 3 * time.Second
	}
	if p.ready.MaxRestarts == 0 {
		p.ready.MaxRestarts = defaultMaxRestarts
	}
	p.logSeen, p.httpSeen, p.tcpSeen = p.log == nil, ready.HTTP == "", ready.TCP == ""
	return p, nil
}

func (p *probe) probes() bool { return p.log != nil || p.ready.HTTP != "" || p.ready.TCP != "" }

func (p *probe) waitingFor(summary docker.Summary) string {
	missing := []string{}
	if summary.HasHealthcheck && summary.Health != "healthy" {
		missing = append(missing, "its healthcheck is "+summary.Health)
	}
	if !p.logSeen {
		missing = append(missing, fmt.Sprintf("its log has not matched %q", p.ready.Log))
	}
	if !p.httpSeen {
		missing = append(missing, p.ready.HTTP+" does not answer")
	}
	if !p.tcpSeen {
		missing = append(missing, p.ready.TCP+" refuses connections")
	}
	if len(missing) == 0 {
		return "it has not settled"
	}
	return strings.Join(missing, ", ")
}

func (s *Swapper) step(ctx context.Context, p *probe, id string) (bool, docker.Summary, error) {
	raw, err := s.Docker.Inspect(ctx, id)
	if err != nil {
		return false, docker.Summary{}, err
	}
	summary := docker.Summarize(raw)
	if p.restarts < 0 {
		p.restarts = summary.RestartCount
	}
	if restarted := summary.RestartCount - p.restarts; restarted >= p.ready.MaxRestarts {
		logs, _ := s.Docker.Logs(ctx, id, time.Time{}, summary.Tty)
		return false, summary, fmt.Errorf("%w: it keeps crashing (restarted %d times while dibs waited); last output:\n%s", ErrNotReady, restarted, tail(logs, 20))
	}
	if !summary.Running && !summary.Restarting {
		logs, _ := s.Docker.Logs(ctx, id, time.Time{}, summary.Tty)
		return false, summary, fmt.Errorf("%w: exited during startup (%s); last output:\n%s", ErrNotReady, summary.Status, tail(logs, 20))
	}
	if p.log != nil || p.fail != nil {
		read := time.Now()
		since := time.Time{}
		if !p.since.IsZero() {
			since = p.since.Add(-time.Second)
		}
		chunk, _ := s.Docker.Logs(ctx, id, since, summary.Tty)
		p.since = read
		text := p.tail + chunk
		if p.fail != nil {
			if match := p.fail.FindString(text); match != "" {
				return false, summary, fmt.Errorf("%w: its log matched the failure pattern (%q); last output:\n%s", ErrNotReady, match, tail(text, 20))
			}
		}
		if !p.logSeen {
			p.logSeen = p.log.MatchString(text)
		}
		if len(text) > 8192 {
			text = text[len(text)-8192:]
		}
		p.tail = text
	}
	if !summary.Running {
		return false, summary, nil
	}
	if !p.httpSeen {
		p.httpSeen = httpAnswers(ctx, p.ready.HTTP)
	}
	if !p.tcpSeen {
		p.tcpSeen = tcpAnswers(p.ready.TCP)
	}
	ok := p.logSeen && p.httpSeen && p.tcpSeen
	if summary.HasHealthcheck {
		ok = ok && summary.Health == "healthy"
	} else if !p.probes() {
		ok = !summary.StartedAt.IsZero() && time.Since(summary.StartedAt) >= p.settle
	}
	return ok, summary, nil
}

func (s *Swapper) readyTimeout(name string) time.Duration {
	if timeout := s.Config.ContainerConfig(name).Ready.Timeout.D(); timeout > 0 {
		return timeout
	}
	return s.Config.ReadyTimeout.D()
}

func (s *Swapper) WaitReady(ctx context.Context, name, id string) error {
	p, err := s.newProbe(name)
	if err != nil {
		return err
	}
	timeout := s.readyTimeout(name)
	deadline := time.Now().Add(timeout)
	announced := false
	for {
		ok, summary, err := s.step(ctx, p, id)
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s: it is running, but %s", ErrStillStarting, timeout, p.waitingFor(summary))
		}
		if !announced && (p.probes() || summary.HasHealthcheck) {
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

func (s *Swapper) Probe(ctx context.Context, name, id string) (state.Status, error) {
	p, err := s.newProbe(name)
	if err != nil {
		return "", err
	}
	ok, _, err := s.step(ctx, p, id)
	switch {
	case errors.Is(err, ErrNotReady):
		return state.StatusFailed, err
	case err != nil:
		return "", err
	case ok:
		return state.StatusReady, nil
	}
	return state.StatusStarting, nil
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
