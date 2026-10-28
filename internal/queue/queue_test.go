package queue

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/state"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func allAlive(int) bool { return true }

func take(t *testing.T, st *state.State, tree string, enqueue bool, resources ...string) Result {
	t.Helper()
	result, err := Take(st, Request{
		Tree: tree, Label: tree, Resources: resources, Lease: 30 * time.Minute,
		PID: 1, Enqueue: enqueue, Now: t0,
	}, allAlive)
	if err != nil {
		t.Fatalf("take %s %v: %v", tree, resources, err)
	}
	return result
}

func holder(st *state.State, name string) string {
	if h := st.Lookup(name).ActiveHolder(t0); h != nil {
		return h.Tree
	}
	return ""
}

func TestGrantWhenFree(t *testing.T) {
	st := state.New()
	if got := take(t, st, "a", false, "browser", "web"); got.Outcome != Granted {
		t.Fatalf("outcome = %s", got.Outcome)
	}
	if holder(st, "browser") != "a" || holder(st, "web") != "a" {
		t.Fatal("both resources should be held by a")
	}
	if exp := st.Lookup("web").Holder.Expires; !exp.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("lease expiry = %v", exp)
	}
}

func TestAllOrNothing(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	got := take(t, st, "b", true, "browser", "web")
	if got.Outcome != Queued {
		t.Fatalf("outcome = %s", got.Outcome)
	}
	if holder(st, "browser") != "" {
		t.Fatal("browser must not be partially granted while web is busy")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Resource != "web" || got.Blockers[0].Holder.Tree != "a" {
		t.Fatalf("blockers = %+v", got.Blockers)
	}
	if got.Position != 1 {
		t.Fatalf("position = %d", got.Position)
	}
}

func TestOneShotNeverJumpsTheQueue(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	take(t, st, "b", true, "web", "api")
	if got := take(t, st, "c", false, "api"); got.Outcome != Queued {
		t.Fatalf("api is free but b is waiting for it, so c must not cut ahead: outcome %s", got.Outcome)
	}
	if holder(st, "api") != "" || Position(st, "c") != 0 {
		t.Fatal("a one-shot take must neither grant nor enqueue")
	}
	Pass(st, "a", nil, t0)
	if holder(st, "web") != "b" || holder(st, "api") != "b" {
		t.Fatal("pass should hand both resources to the waiter")
	}
}

func TestFIFOPreventsStarvation(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	take(t, st, "b", true, "web", "api")
	take(t, st, "c", true, "api")
	if holder(st, "api") != "" {
		t.Fatalf("c must wait behind b for api, got holder %q", holder(st, "api"))
	}
	Pass(st, "a", nil, t0)
	if holder(st, "web") != "b" || holder(st, "api") != "b" {
		t.Fatal("b should get both once web frees")
	}
	Pass(st, "b", nil, t0)
	if holder(st, "api") != "c" {
		t.Fatal("c should get api after b passes")
	}
}

func TestRetakeExtendsLease(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	later := t0.Add(20 * time.Minute)
	if _, err := Take(st, Request{Tree: "a", Resources: []string{"web"}, Lease: 30 * time.Minute, Now: later}, allAlive); err != nil {
		t.Fatal(err)
	}
	if exp := st.Lookup("web").Holder.Expires; !exp.Equal(later.Add(30 * time.Minute)) {
		t.Fatalf("expiry = %v", exp)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	later := t0.Add(31 * time.Minute)
	result, err := Take(st, Request{Tree: "b", Resources: []string{"web"}, Lease: time.Minute, Now: later}, allAlive)
	if err != nil || result.Outcome != Granted {
		t.Fatalf("expired lease should be reclaimed: %v %v", result.Outcome, err)
	}
}

func TestPruneDropsDeadWaiters(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	Take(st, Request{Tree: "b", Resources: []string{"web"}, Lease: time.Minute, PID: 99, Enqueue: true, Now: t0}, allAlive)
	_, dropped := Prune(st, t0, func(pid int) bool { return pid != 99 })
	if len(dropped) != 1 || len(st.Queue) != 0 {
		t.Fatalf("dead waiter should be dropped, queue = %+v", st.Queue)
	}
}

func TestPinnedResourceBlocks(t *testing.T) {
	st := state.New()
	Grab(st, state.Holder{Tree: "human", Label: "human", Note: "debugging"}, []string{"web"}, t0)
	got := take(t, st, "a", true, "web")
	if got.Outcome != Blocked {
		t.Fatalf("outcome = %s", got.Outcome)
	}
	if exp := st.Lookup("web").Holder; !exp.Pinned() || exp.Expired(t0.Add(24*time.Hour)) {
		t.Fatal("manual holds are pinned and never expire")
	}
	if released := Pass(st, "human", []string{"web"}, t0); len(released) != 0 {
		t.Fatal("pass must not release a manual hold")
	}
	Drop(st, nil, t0)
	if holder(st, "web") != "a" {
		t.Fatal("dropping the pin should grant the waiter")
	}
}

func TestGrabReportsDisplaced(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	displaced := Grab(st, state.Holder{Tree: "human"}, []string{"web", "api"}, t0)
	if len(displaced) != 1 || displaced[0].Resource != "web" || displaced[0].Holder.Tree != "a" {
		t.Fatalf("displaced = %+v", displaced)
	}
}

func TestDeadlockDetected(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	take(t, st, "b", false, "api")
	take(t, st, "a", true, "web", "api")
	_, err := Take(st, Request{Tree: "b", Resources: []string{"api", "web"}, Lease: time.Minute, PID: 1, Enqueue: true, Now: t0}, allAlive)
	if !errors.Is(err, ErrDeadlock) {
		t.Fatalf("expected deadlock, got %v", err)
	}
	if Position(st, "b") != 0 {
		t.Fatal("deadlocked waiter must not stay queued")
	}
}

func TestRenewOnlySessionHolds(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	Grab(st, state.Holder{Tree: "a"}, []string{"api"}, t0)
	renewed := Renew(st, "a", time.Hour, t0.Add(time.Minute))
	if !reflect.DeepEqual(renewed, []string{"web"}) {
		t.Fatalf("renewed = %v", renewed)
	}
}

func TestSessionOfPinnedTreeCanUseIt(t *testing.T) {
	st := state.New()
	Grab(st, state.Holder{Tree: "a"}, []string{"web"}, t0)
	if got := take(t, st, "a", false, "web"); got.Outcome != Granted {
		t.Fatalf("a worktree pinned by hand should be able to take its own pin: %s", got.Outcome)
	}
	if !st.Lookup("web").Holder.Pinned() {
		t.Fatal("taking must not convert a manual hold into a session lease")
	}
}

func TestAheadListsEarlierWaiters(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	take(t, st, "b", true, "web")
	take(t, st, "c", true, "api")
	got := take(t, st, "d", true, "web")
	if !reflect.DeepEqual(got.Ahead, []string{"b"}) || got.Position != 2 {
		t.Fatalf("ahead = %v position = %d", got.Ahead, got.Position)
	}
}

func activeAt(times map[string]time.Time) func(string) time.Time {
	return func(tree string) time.Time { return times[tree] }
}

func TestIdleHolderLosesWhatOthersWaitFor(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web", "api")
	take(t, st, "b", true, "web")
	policy := IdlePolicy{Limit: 20 * time.Minute, LastActive: activeAt(map[string]time.Time{"a": t0.Add(time.Minute)}), Alive: allAlive}

	if released := ReleaseIdle(st, t0.Add(20*time.Minute), policy); len(released) != 0 {
		t.Fatalf("19 minutes without tool calls is under the limit: %+v", released)
	}
	released := ReleaseIdle(st, t0.Add(22*time.Minute), policy)
	if len(released) != 1 || released[0].Resource != "web" || !released[0].Since.Equal(t0.Add(time.Minute)) {
		t.Fatalf("released = %+v", released)
	}
	if len(released[0].Waiters) != 1 || released[0].Waiters[0].Tree != "b" {
		t.Fatalf("the release should name who was waiting: %+v", released[0].Waiters)
	}
	if holder(st, "web") != "" || holder(st, "api") != "a" {
		t.Fatal("only what someone waits for is released")
	}
	Schedule(st, t0.Add(22*time.Minute))
	if holder(st, "web") != "b" {
		t.Fatal("the waiter should get it at the next schedule")
	}
}

func TestIdleReleaseNeedsWaitersAndTracking(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	late := t0.Add(3 * time.Hour)
	tracked := IdlePolicy{Limit: 20 * time.Minute, LastActive: activeAt(map[string]time.Time{"a": t0}), Alive: allAlive}
	if released := ReleaseIdle(st, late, tracked); len(released) != 0 {
		t.Fatal("nobody is waiting, so an idle holder keeps its lease")
	}

	take(t, st, "b", true, "web")
	untracked := IdlePolicy{Limit: 20 * time.Minute, LastActive: activeAt(nil), Alive: allAlive}
	if released := ReleaseIdle(st, late, untracked); len(released) != 0 {
		t.Fatal("a holder whose tool calls dibs never saw (a human at a terminal) is not judged idle")
	}
	stale := IdlePolicy{Limit: 20 * time.Minute, LastActive: activeAt(map[string]time.Time{"a": t0.Add(-time.Hour)}), Alive: allAlive}
	if released := ReleaseIdle(st, late, stale); len(released) != 0 {
		t.Fatal("activity from before the lease began does not make the holder tracked")
	}
	if released := ReleaseIdle(st, late, IdlePolicy{LastActive: tracked.LastActive}); len(released) != 0 {
		t.Fatal("a zero limit turns idle release off")
	}
}

func TestIdleReleaseSparesHoldsSwapsAndWaitingHolders(t *testing.T) {
	late := t0.Add(time.Hour)
	policy := IdlePolicy{Limit: 20 * time.Minute, LastActive: activeAt(map[string]time.Time{"a": t0}), Alive: func(pid int) bool { return pid == 7 }}

	held := state.New()
	take(t, held, "a", false, "web")
	Hold(held, "a", "the user is testing", 2*time.Hour, 3*time.Hour, t0)
	take(t, held, "b", true, "web")
	if released := ReleaseIdle(held, late, policy); len(released) != 0 {
		t.Fatal("a lease on hold is not idle-released")
	}

	swapping := state.New()
	take(t, swapping, "a", false, "web")
	swapping.Resources["web"].Status = state.StatusSwapping
	swapping.Resources["web"].Intent = &state.Intent{PID: 7}
	take(t, swapping, "b", true, "web")
	if released := ReleaseIdle(swapping, late, policy); len(released) != 0 {
		t.Fatal("a container still being switched for its holder is not idle")
	}

	queued := state.New()
	take(t, queued, "a", false, "web")
	take(t, queued, "c", false, "api")
	take(t, queued, "b", true, "web")
	take(t, queued, "a", true, "api")
	if released := ReleaseIdle(queued, late, policy); len(released) != 0 {
		t.Fatal("a holder that is itself queued for more is waiting on dibs, not idle")
	}
}

func TestHoldIsBoundedAndShownAsANote(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	Grab(st, state.Holder{Tree: "a"}, []string{"api"}, t0)

	held := Hold(st, "a", "waiting for the user to log in", time.Hour, 2*time.Hour, t0)
	if !reflect.DeepEqual(held.Resources, []string{"web"}) || !held.Until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("held = %+v", held)
	}
	h := st.Lookup("web").Holder
	if !h.OnHold() || h.Note != "waiting for the user to log in" || !h.Expires.Equal(t0.Add(time.Hour)) {
		t.Fatalf("holder = %+v", h)
	}

	if held := Hold(st, "a", "still", 2*time.Hour, 2*time.Hour, t0.Add(50*time.Minute)); !held.Until.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("a hold cannot run past the limit counted from when it began: %+v", held)
	}
	if held := Hold(st, "a", "more", time.Hour, 2*time.Hour, t0.Add(2*time.Hour)); len(held.Resources) != 0 || !reflect.DeepEqual(held.AtLimit, []string{"web"}) {
		t.Fatalf("at the limit the hold is refused: %+v", held)
	}

	Renew(st, "a", 30*time.Minute, t0.Add(100*time.Minute))
	if h := st.Lookup("web").Holder; h.OnHold() || h.Note != "" {
		t.Fatal("renewing returns to an ordinary lease")
	}
	Hold(st, "a", "again", time.Hour, 2*time.Hour, t0.Add(110*time.Minute))
	take(t, st, "a", false, "web")
	if st.Lookup("web").Holder.OnHold() {
		t.Fatal("taking again returns to an ordinary lease")
	}
}

func TestHoldNeverShortensALease(t *testing.T) {
	st := state.New()
	take(t, st, "a", false, "web")
	Hold(st, "a", "brief", 5*time.Minute, time.Hour, t0)
	if exp := st.Lookup("web").Holder.Expires; !exp.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("expiry = %v", exp)
	}
}
