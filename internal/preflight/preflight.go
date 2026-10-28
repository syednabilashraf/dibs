package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	Take    = "take"
	Restore = "restore"
	Sync    = "sync"
)

var Timeout = 2 * time.Minute

const maxOutput = 4000

type Target struct {
	Container string
	Tree      string
	Rev       string
	Action    string
}

type Result struct {
	Container string
	Passed    bool
	Output    string
	Err       error
}

func Run(ctx context.Context, command string, t Target) Result {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = t.Tree
	cmd.Env = append(os.Environ(),
		"PWD="+t.Tree,
		"DIBS_TREE="+t.Tree,
		"DIBS_REV="+t.Rev,
		"DIBS_CONTAINER="+t.Container,
		"DIBS_ACTION="+t.Action,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()

	result := Result{Container: t.Container, Output: clip(strings.TrimSpace(string(out)))}
	var exit *exec.ExitError
	switch {
	case err == nil:
		result.Passed = true
	case ctx.Err() != nil:
		result.Err = fmt.Errorf("the check did not finish within %s", Timeout)
	case errors.As(err, &exit):
	default:
		result.Err = err
	}
	return result
}

func (r Result) Explain() string {
	var b strings.Builder
	if r.Err != nil {
		fmt.Fprintf(&b, "the migrations check could not run: %v", r.Err)
	} else {
		b.WriteString("the migrations check says no")
	}
	if r.Output != "" {
		b.WriteString(":\n" + indent(r.Output))
	}
	return b.String()
}

func clip(text string) string {
	if len(text) <= maxOutput {
		return text
	}
	return "..." + text[len(text)-maxOutput:]
}

func indent(text string) string {
	return "    " + strings.ReplaceAll(text, "\n", "\n    ")
}
