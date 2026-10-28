package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
)

func dibsRun(args ...string) (int, string) {
	var out bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &out)
	return code, out.String()
}

func TestTakeWaitsUntilGrantedByDefault(t *testing.T) {
	w := newHookWorld(t)
	done := make(chan string, 1)
	go func() {
		code, out := dibsRun("take", "browser", "--wait", "--tree", w.b.Path)
		if code != exitOK {
			out = fmt.Sprintf("exit %d: %s", code, out)
		}
		done <- out
	}()
	select {
	case out := <-done:
		t.Fatalf("the take must keep waiting while browser is held: %s", out)
	case <-time.After(2500 * time.Millisecond):
	}
	if code, out := dibsRun("pass", "--tree", w.a.Path); code != exitOK {
		t.Fatalf("pass: %d %s", code, out)
	}
	select {
	case out := <-done:
		if !strings.Contains(out, "ticket-b holds browser") || strings.Contains(out, "note:") {
			t.Fatalf("granted after the pass, with no timeout warning: %s", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting take should be granted once browser is passed")
	}
}

func TestTakeWithATimeoutWarnsAndLogsGivingUp(t *testing.T) {
	w := newHookWorld(t)
	code, out := dibsRun("take", "browser", "--wait", "--timeout", "1s", "--tree", w.b.Path)
	if code != exitNo || !strings.Contains(out, "the lease you are waiting for runs until") || !strings.Contains(out, "gave up after <1m, so your place in line is gone") {
		t.Fatalf("%d %s", code, out)
	}
	store, _ := state.Open(w.home)
	events, _ := store.Events(time.Now().Add(-time.Minute))
	if len(events) != 1 || events[0].Kind != "abandon" || events[0].Tree != w.b.Path || !strings.Contains(events[0].Detail, "gave up") {
		t.Fatalf("giving up is logged: %+v", events)
	}
	st, _ := store.Read()
	if queue.Position(st, w.b.Path) != 0 {
		t.Fatal("a take that gave up leaves the line")
	}
}

func TestTimeoutWarning(t *testing.T) {
	deadline := time.Now().Add(10 * time.Minute)
	if timeoutWarning(queue.Result{Ahead: []string{"x"}}, time.Time{}) != "" {
		t.Fatal("no deadline, nothing to warn about")
	}
	if timeoutWarning(queue.Result{Blockers: []queue.Blocker{{Resource: "web", Holder: state.Holder{Kind: state.KindSession, Expires: deadline.Add(-time.Minute)}}}}, deadline) != "" {
		t.Fatal("a lease ending before the deadline gives the take a chance")
	}
	text := timeoutWarning(queue.Result{
		Ahead:    []string{"x", "y"},
		Blockers: []queue.Blocker{{Resource: "web", Holder: state.Holder{Kind: state.KindManual}}},
	}, deadline)
	if !strings.Contains(text, "2 session(s) ahead of you") || !strings.Contains(text, "a human pinned web") || !strings.Contains(text, "Leave out --timeout") {
		t.Fatalf("warning: %s", text)
	}
}
