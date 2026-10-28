package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
)

func runCLI(args ...string) (int, string) {
	var out bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &out)
	return code, out.String()
}

func TestHoldCommand(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		for _, name := range []string{"browser", "web"} {
			st.Resources[name].Holder.Expires = time.Now().Add(10 * time.Minute)
		}
		return nil
	})
	if code, out := runCLI("hold", "--tree", w.a.Path); code != exitError || !strings.Contains(out, "--note") {
		t.Fatalf("a hold needs a reason: %d %s", code, out)
	}
	code, out := runCLI("hold", "--note", "the user is testing the toolbar", "--for", "45m", "--tree", w.a.Path)
	if code != exitOK || !strings.Contains(out, "holding browser, web until") || !strings.Contains(out, "everyone waiting sees: the user is testing the toolbar") {
		t.Fatalf("hold: %d %s", code, out)
	}
	if code, out := runCLI("hold", "--note", "x", "--tree", w.b.Path); code != exitNo || !strings.Contains(out, "holds no leases") {
		t.Fatalf("holding nothing: %d %s", code, out)
	}

	st, _ := store.Read()
	if h := st.Lookup("web").Holder; !h.OnHold() || h.Expires.Sub(time.Now()) < 44*time.Minute {
		t.Fatalf("holder = %+v", h)
	}
	_, status := runCLI("status")
	for _, want := range []string{"LAST TOOL CALL", "[on hold: the user is testing the toolbar]", "on hold, 45m left", "put browser,web on hold"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status should show %q:\n%s", want, status)
		}
	}
}

func TestStatusShowsLastToolCall(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		st.Resources["web"].Holder.Since = time.Now().Add(-time.Hour)
		return nil
	})
	if _, status := runCLI("status"); !strings.Contains(status, "unknown") {
		t.Fatalf("without hook activity the last call is unknown:\n%s", status)
	}
	store.Touch(w.a.Path, time.Now().Add(-40*time.Minute))
	if _, status := runCLI("status"); !strings.Contains(status, "40m ago") {
		t.Fatalf("status should show the holder's last tool call:\n%s", status)
	}
}

func TestWaitersSeeIdleAndHeldHolders(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	a := &app{env: &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, cfg: config.Default(), store: store}
	now := time.Now()
	idle := state.Holder{Tree: w.a.Path, Label: w.a.Label, Kind: state.KindSession, Since: now.Add(-time.Hour), Expires: now.Add(10 * time.Minute)}
	store.Touch(w.a.Path, now.Add(-12*time.Minute))
	text := a.describeWait(queue.Result{Blockers: []queue.Blocker{{Resource: "web", Holder: idle}}}, now)
	if !strings.Contains(text, "no tool calls since "+clock(now.Add(-12*time.Minute))) || !strings.Contains(text, "released at "+clock(now.Add(8*time.Minute))) {
		t.Fatalf("waiters should see that the holder is idle and when it loses the lease:\n%s", text)
	}
	held := idle
	held.HoldSince, held.Note = now, "waiting for the user"
	if text := a.describeWait(queue.Result{Blockers: []queue.Blocker{{Resource: "web", Holder: held}}}, now); !strings.Contains(text, "[on hold: waiting for the user]") || !strings.Contains(text, "held until") {
		t.Fatalf("waiters should see why it is held:\n%s", text)
	}
}

func TestTakeWaitReclaimsFromAnIdleHolder(t *testing.T) {
	w := newHookWorld(t)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		st.Resources["browser"].Holder.Since = time.Now().Add(-time.Hour)
		return nil
	})
	store.Touch(w.a.Path, time.Now().Add(-30*time.Minute))

	code, out := runCLI("take", "browser", "--wait", "--timeout", "20s", "--tree", w.b.Path)
	if code != exitOK || !strings.Contains(out, "ticket-b holds browser") {
		t.Fatalf("b should get the browser from the idle holder: %d %s", code, out)
	}
	if !strings.Contains(out, "no tool calls since") {
		t.Fatalf("while waiting, b should have been told the holder was idle:\n%s", out)
	}
	events, _ := store.Events(time.Now().Add(-time.Minute))
	found := false
	for _, ev := range events {
		found = found || (ev.Kind == "idle" && ev.Resource == "browser" && ev.Tree == w.a.Path && strings.Contains(ev.Detail, "ticket-b waited"))
	}
	if !found {
		t.Fatalf("the idle release should be logged: %+v", events)
	}
}
