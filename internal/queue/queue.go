package queue

import (
	"errors"
	"fmt"
	"time"

	"github.com/syednabilashraf/dibs/internal/state"
)

type Outcome string

const (
	Granted Outcome = "granted"
	Queued  Outcome = "queued"
	Blocked Outcome = "blocked"
)

var ErrDeadlock = errors.New("waiting would deadlock: another worktree holds something you need and is waiting for something you hold; pass what you hold first")

type Alive func(pid int) bool

type Request struct {
	Tree      string
	Label     string
	Resources []string
	Lease     time.Duration
	PID       int
	NoSwap    bool
	Enqueue   bool
	Now       time.Time
}

type Blocker struct {
	Resource string
	Holder   state.Holder
}

type Result struct {
	Outcome  Outcome
	Position int
	Ahead    []string
	Blockers []Blocker
}

type Expired struct {
	Resource string
	Holder   state.Holder
}

type Displaced struct {
	Resource string
	Holder   state.Holder
}

func Prune(st *state.State, now time.Time, alive Alive) ([]Expired, []state.Waiter) {
	expired := []Expired{}
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Holder != nil && r.Holder.Expired(now) {
			expired = append(expired, Expired{Resource: name, Holder: *r.Holder})
			r.Holder = nil
		}
	}
	dropped := []state.Waiter{}
	kept := st.Queue[:0]
	for _, w := range st.Queue {
		if alive != nil && !alive(w.PID) {
			dropped = append(dropped, w)
			continue
		}
		kept = append(kept, w)
	}
	st.Queue = kept
	return expired, dropped
}

func Take(st *state.State, req Request, alive Alive) (Result, error) {
	if req.Tree == "" {
		return Result{}, fmt.Errorf("a worktree is required")
	}
	if len(req.Resources) == 0 {
		return Result{}, fmt.Errorf("name at least one resource")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	Prune(st, now, alive)

	if holdsAll(st, req.Tree, req.Resources, now) {
		grant(st, req.Tree, req.Label, req.Resources, req.Lease, req.NoSwap, now)
		Leave(st, req.Tree)
		return Result{Outcome: Granted}, nil
	}

	if !req.Enqueue {
		if canGrant(st, req.Tree, req.Resources, now) && !wantedByOthers(st, req.Tree, req.Resources) {
			grant(st, req.Tree, req.Label, req.Resources, req.Lease, req.NoSwap, now)
			return Result{Outcome: Granted}, nil
		}
		return describe(st, req.Tree, req.Resources, now), nil
	}

	upsert(st, state.Waiter{
		ID:        fmt.Sprintf("%d-%d", now.UnixNano(), req.PID),
		Tree:      req.Tree,
		Label:     req.Label,
		Resources: append([]string(nil), req.Resources...),
		PID:       req.PID,
		Lease:     req.Lease,
		NoSwap:    req.NoSwap,
		Since:     now,
	})
	if deadlocked(st, req.Tree, now) {
		Leave(st, req.Tree)
		return Result{}, ErrDeadlock
	}
	Schedule(st, now)
	if holdsAll(st, req.Tree, req.Resources, now) {
		return Result{Outcome: Granted}, nil
	}
	return describe(st, req.Tree, req.Resources, now), nil
}

func Schedule(st *state.State, now time.Time) []state.Waiter {
	granted := []state.Waiter{}
	wanted := map[string]bool{}
	remaining := []state.Waiter{}
	for _, w := range st.Queue {
		blocked := false
		for _, r := range w.Resources {
			if wanted[r] {
				blocked = true
			}
		}
		if !blocked && canGrant(st, w.Tree, w.Resources, now) {
			grant(st, w.Tree, w.Label, w.Resources, w.Lease, w.NoSwap, now)
			granted = append(granted, w)
			continue
		}
		remaining = append(remaining, w)
		for _, r := range w.Resources {
			wanted[r] = true
		}
	}
	st.Queue = remaining
	return granted
}

func Pass(st *state.State, tree string, resources []string, now time.Time) []string {
	if len(resources) == 0 {
		resources = st.HeldBy(tree, now)
	}
	released := []string{}
	for _, name := range resources {
		r := st.Lookup(name)
		if h := r.ActiveHolder(now); h != nil && h.Tree == tree && h.Kind == state.KindSession {
			r.Holder = nil
			released = append(released, name)
		}
	}
	Schedule(st, now)
	return released
}

func Renew(st *state.State, tree string, lease time.Duration, now time.Time) []string {
	renewed := []string{}
	for _, name := range st.HeldBy(tree, now) {
		h := st.Resources[name].Holder
		if h.Kind == state.KindSession {
			h.Expires = now.Add(lease)
			renewed = append(renewed, name)
		}
	}
	return renewed
}

func Grab(st *state.State, holder state.Holder, resources []string, now time.Time) []Displaced {
	holder.Kind = state.KindManual
	holder.Expires = time.Time{}
	if holder.Since.IsZero() {
		holder.Since = now
	}
	displaced := []Displaced{}
	for _, name := range resources {
		r := st.Get(name)
		if h := r.ActiveHolder(now); h != nil && h.Tree != holder.Tree {
			displaced = append(displaced, Displaced{Resource: name, Holder: *h})
		}
		copied := holder
		r.Holder = &copied
	}
	return displaced
}

func Drop(st *state.State, resources []string, now time.Time) []string {
	if len(resources) == 0 {
		resources = st.Names()
	}
	dropped := []string{}
	for _, name := range resources {
		r := st.Lookup(name)
		if r != nil && r.Holder.Pinned() {
			r.Holder = nil
			dropped = append(dropped, name)
		}
	}
	Schedule(st, now)
	return dropped
}

func Leave(st *state.State, tree string) {
	kept := st.Queue[:0]
	for _, w := range st.Queue {
		if w.Tree != tree {
			kept = append(kept, w)
		}
	}
	st.Queue = kept
}

func Position(st *state.State, tree string) int {
	for i, w := range st.Queue {
		if w.Tree == tree {
			return i + 1
		}
	}
	return 0
}

func holdsAll(st *state.State, tree string, resources []string, now time.Time) bool {
	for _, name := range resources {
		h := st.Lookup(name).ActiveHolder(now)
		if h == nil || h.Tree != tree {
			return false
		}
	}
	return true
}

func canGrant(st *state.State, tree string, resources []string, now time.Time) bool {
	for _, name := range resources {
		if h := st.Lookup(name).ActiveHolder(now); h != nil && h.Tree != tree {
			return false
		}
	}
	return true
}

func wantedByOthers(st *state.State, tree string, resources []string) bool {
	want := toSet(resources)
	for _, w := range st.Queue {
		if w.Tree == tree {
			continue
		}
		for _, r := range w.Resources {
			if want[r] {
				return true
			}
		}
	}
	return false
}

func grant(st *state.State, tree, label string, resources []string, lease time.Duration, noSwap bool, now time.Time) {
	for _, name := range resources {
		r := st.Get(name)
		if h := r.ActiveHolder(now); h != nil && h.Tree == tree {
			if h.Kind == state.KindSession {
				h.Expires = now.Add(lease)
				h.NoSwap = noSwap
			}
			continue
		}
		r.Holder = &state.Holder{
			Tree:    tree,
			Label:   label,
			Kind:    state.KindSession,
			Since:   now,
			Expires: now.Add(lease),
			NoSwap:  noSwap,
		}
	}
}

func upsert(st *state.State, waiter state.Waiter) {
	for i, w := range st.Queue {
		if w.Tree == waiter.Tree {
			waiter.ID = w.ID
			waiter.Since = w.Since
			st.Queue[i] = waiter
			return
		}
	}
	st.Queue = append(st.Queue, waiter)
}

func deadlocked(st *state.State, start string, now time.Time) bool {
	edges := map[string][]string{}
	for _, w := range st.Queue {
		for _, name := range w.Resources {
			if h := st.Lookup(name).ActiveHolder(now); h != nil && h.Tree != w.Tree {
				edges[w.Tree] = append(edges[w.Tree], h.Tree)
			}
		}
	}
	seen := map[string]bool{}
	pending := append([]string(nil), edges[start]...)
	for len(pending) > 0 {
		tree := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if tree == start {
			return true
		}
		if seen[tree] {
			continue
		}
		seen[tree] = true
		pending = append(pending, edges[tree]...)
	}
	return false
}

func describe(st *state.State, tree string, resources []string, now time.Time) Result {
	result := Result{Outcome: Queued, Position: Position(st, tree)}
	want := toSet(resources)
	for _, name := range resources {
		if h := st.Lookup(name).ActiveHolder(now); h != nil && h.Tree != tree {
			result.Blockers = append(result.Blockers, Blocker{Resource: name, Holder: *h})
			if h.Pinned() {
				result.Outcome = Blocked
			}
		}
	}
	for _, w := range st.Queue {
		if w.Tree == tree {
			break
		}
		for _, r := range w.Resources {
			if want[r] {
				result.Ahead = append(result.Ahead, w.Label)
				break
			}
		}
	}
	return result
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}
