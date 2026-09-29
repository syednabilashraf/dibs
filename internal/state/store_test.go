package state

import (
	"os"
	"sync"
	"testing"
	"time"
)

func TestReadMissingStateIsEmpty(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Resources) != 0 || len(st.Queue) != 0 {
		t.Fatalf("expected empty state, got %+v", st)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	store, _ := Open(t.TempDir())
	now := time.Now().Truncate(time.Second)
	err := store.Update(func(st *State) error {
		r := st.Get("web")
		r.Holder = &Holder{Tree: "/w/a", Label: "feat-a", Kind: KindSession, Since: now, Expires: now.Add(time.Minute)}
		r.Serving = "/w/a"
		r.Ports = []int{3000}
		st.Queue = append(st.Queue, Waiter{ID: "1", Tree: "/w/b", Resources: []string{"web"}, PID: 42})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	r := st.Lookup("web")
	if r == nil || r.Holder.Tree != "/w/a" || r.Serving != "/w/a" || len(r.Ports) != 1 {
		t.Fatalf("resource not persisted: %+v", r)
	}
	if len(st.Queue) != 1 || st.Queue[0].PID != 42 {
		t.Fatalf("queue not persisted: %+v", st.Queue)
	}
}

func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	store, _ := Open(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Update(func(st *State) error {
				st.Queue = append(st.Queue, Waiter{ID: "x"})
				return nil
			})
		}()
	}
	wg.Wait()
	st, _ := store.Read()
	if len(st.Queue) != 20 {
		t.Fatalf("expected 20 waiters, got %d", len(st.Queue))
	}
}

func TestEventsSince(t *testing.T) {
	store, _ := Open(t.TempDir())
	base := time.Now()
	store.Log(Event{Time: base.Add(-time.Hour), Kind: "swap", Resource: "old"})
	store.Log(Event{Time: base, Kind: "swap", Resource: "web"})
	store.Log(Event{Time: base.Add(time.Second), Kind: "pass", Resource: "web"})
	events, err := store.Events(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Resource != "web" || events[1].Kind != "pass" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestExpiryAndPins(t *testing.T) {
	now := time.Now()
	session := &Holder{Kind: KindSession, Expires: now.Add(-time.Second)}
	manual := &Holder{Kind: KindManual}
	if !session.Expired(now) {
		t.Fatal("session past its lease should be expired")
	}
	if manual.Expired(now) || !manual.Pinned() {
		t.Fatal("manual holds never expire and are pinned")
	}
	r := &Resource{Holder: session}
	if r.ActiveHolder(now) != nil {
		t.Fatal("expired holder should not be active")
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("own pid should be alive")
	}
	if ProcessAlive(0) || ProcessAlive(-1) {
		t.Fatal("non-positive pids are never alive")
	}
}
