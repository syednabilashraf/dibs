package preflight

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunPassesTheTargetInTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	result := Run(context.Background(), `test "$DIBS_TREE" = "$PWD" && echo "$DIBS_CONTAINER $DIBS_ACTION $DIBS_REV"`, Target{Container: "web", Tree: dir, Rev: "abc123", Action: Take})
	if !result.Passed || result.Output != "web take abc123" || result.Err != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunReportsAFailingCheck(t *testing.T) {
	result := Run(context.Background(), `echo "would apply 017_add_column"; exit 3`, Target{Container: "web", Tree: t.TempDir(), Action: Restore})
	if result.Passed || result.Err != nil {
		t.Fatalf("a non-zero exit is a refusal, not an error: %+v", result)
	}
	if text := result.Explain(); !strings.Contains(text, "says no") || !strings.Contains(text, "017_add_column") {
		t.Fatalf("explanation: %s", text)
	}
}

func TestRunGivesUpOnAHangingCheck(t *testing.T) {
	saved := Timeout
	Timeout = 300 * time.Millisecond
	defer func() { Timeout = saved }()
	start := time.Now()
	result := Run(context.Background(), `sleep 30 & wait`, Target{Container: "web", Tree: t.TempDir(), Action: Sync})
	if result.Passed || result.Err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("a hanging check must fail with an error quickly: %+v after %s", result, time.Since(start))
	}
	if !strings.Contains(result.Explain(), "could not run") {
		t.Fatalf("explanation: %s", result.Explain())
	}
}

func TestRunClipsLongOutput(t *testing.T) {
	result := Run(context.Background(), `i=0; while [ $i -lt 3000 ]; do echo "line $i"; i=$((i+1)); done; exit 1`, Target{Container: "web", Tree: t.TempDir(), Action: Take})
	if len(result.Output) > maxOutput+3 || !strings.HasSuffix(result.Output, "line 2999") {
		t.Fatalf("output should keep its end: %d bytes", len(result.Output))
	}
}
