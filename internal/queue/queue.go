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
		grant(st, req.Tree, req.Label, req.Resources, req.Lease, req.NoSwap, req.PID, now)
		Leave(st, req.Tree)
		return Result{Outcome: Granted}, nil
	}

	if !req.Enqueue {
		if canGrant(st, req.Tree, req.Resources, now) && !reservedAgainst(st, req.Tree, req.Resources, len(st.Queue), now) {
			grant(st, req.Tree, req.Label, req.Resources, req.Lease, req.NoSwap, req.PID, now)
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
	for {
		next := -1
		for i, w := range st.Queue {
			if canGrant(st, w.Tree, w.Resources, now) && !reservedAgainst(st, w.Tree, w.Resources, i, now) {
				next = i
				break
			}
		}
		if next < 0 {
			return granted
		}
		w := st.Queue[next]
		grant(st, w.Tree, w.Label, w.Resources, w.Lease, w.NoSwap, w.PID, now)
		granted = append(granted, w)
		st.Queue = append(st.Queue[:next:next], st.Queue[next+1:]...)
	}
}

func reservedAgainst(st *state.State, tree string, resources []string, before int, now time.Time) bool {
	return len(reservations(st, tree, resources, before, now)) > 0
}

func reservations(st *state.State, tree string, resources []string, before int, now time.Time) []state.Waiter {
	want := toSet(resources)
	var waits graph
	blocking := []state.Waiter{}
	for _, e := range st.Queue[:min(before, len(st.Queue))] {
		if e.Tree == tree || !overlaps(e.Resources, want) {
			continue
		}
		if waits == nil {
			waits = waitGraph(st, now)
		}
		if !waits.reaches(e.Tree, tree) {
			blocking = append(blocking, e)
		}
	}
	return blocking
}

type graph map[string]map[string]bool

func waitGraph(st *state.State, now time.Time) graph {
	g := graph{}
	edge := func(from, to string) {
		if from == to {
			return
		}
		if g[from] == nil {
			g[from] = map[string]bool{}
		}
		g[from][to] = true
	}
	for i, w := range st.Queue {
		want := toSet(w.Resources)
		for _, name := range w.Resources {
			if h := st.Lookup(name).ActiveHolder(now); h != nil {
				edge(w.Tree, h.Tree)
			}
		}
		for _, earlier := range st.Queue[:i] {
			if overlaps(earlier.Resources, want) {
				edge(w.Tree, earlier.Tree)
			}
		}
	}
	return g
}

func (g graph) reaches(from, to string) bool {
	seen := map[string]bool{}
	pending := []string{from}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for next := range g[current] {
			if next == to {
				return true
			}
			if !seen[next] {
				seen[next] = true
				pending = append(pending, next)
			}
		}
	}
	return false
}

func overlaps(resources []string, want map[string]bool) bool {
	for _, r := range resources {
		if want[r] {
			return true
		}
	}
	return false
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
			h.HoldSince, h.Note = time.Time{}, ""
			renewed = append(renewed, name)
		}
	}
	return renewed
}

type Held struct {
	Resources []string
	Until     time.Time
	AtLimit   []string
}

func Hold(st *state.State, tree, note string, length, limit time.Duration, now time.Time) Held {
	held := Held{}
	for _, name := range st.HeldBy(tree, now) {
		h := st.Resources[name].Holder
		if h.Kind != state.KindSession {
			continue
		}
		start := h.HoldSince
		if start.IsZero() {
			start = now
		}
		end := now.Add(length)
		if limit > 0 && end.After(start.Add(limit)) {
			end = start.Add(limit)
		}
		if !end.After(now) {
			held.AtLimit = append(held.AtLimit, name)
			continue
		}
		if end.After(h.Expires) {
			h.Expires = end
		}
		h.HoldSince, h.Note = start, note
		held.Resources = append(held.Resources, name)
		if h.Expires.After(held.Until) {
			held.Until = h.Expires
		}
	}
	return held
}

const trackingSlack = 2 * time.Minute

func IdleSince(h *state.Holder, lastActive time.Time) (time.Time, bool) {
	if h == nil || lastActive.IsZero() || lastActive.Before(h.Since.Add(-trackingSlack)) {
		return time.Time{}, false
	}
	if lastActive.Before(h.Since) {
		return h.Since, true
	}
	return lastActive, true
}

type IdlePolicy struct {
	Limit      time.Duration
	LastActive func(tree string) time.Time
	Alive      Alive
}

type Idle struct {
	Resource string
	Holder   state.Holder
	Since    time.Time
	Waiters  []state.Waiter
}

func ReleaseIdle(st *state.State, now time.Time, policy IdlePolicy) []Idle {
	if policy.Limit <= 0 || policy.LastActive == nil || len(st.Queue) == 0 {
		return nil
	}
	queued := map[string]bool{}
	for _, w := range st.Queue {
		queued[w.Tree] = true
	}
	activity := map[string]time.Time{}
	released := []Idle{}
	for _, name := range st.Names() {
		r := st.Resources[name]
		h := r.ActiveHolder(now)
		if h == nil || h.Kind != state.KindSession || h.OnHold() || queued[h.Tree] {
			continue
		}
		if r.Status == state.StatusSwapping && r.Intent != nil && policy.Alive != nil && policy.Alive(r.Intent.PID) {
			continue
		}
		waiters := WantedBy(st, name, h.Tree)
		if len(waiters) == 0 {
			continue
		}
		last, ok := activity[h.Tree]
		if !ok {
			last = policy.LastActive(h.Tree)
			activity[h.Tree] = last
		}
		since, tracked := IdleSince(h, last)
		if !tracked || now.Sub(since) < policy.Limit {
			continue
		}
		released = append(released, Idle{Resource: name, Holder: *h, Since: since, Waiters: waiters})
		r.Holder = nil
	}
	return released
}

func WantedBy(st *state.State, resource, except string) []state.Waiter {
	waiters := []state.Waiter{}
	for _, w := range st.Queue {
		if w.Tree == except {
			continue
		}
		for _, r := range w.Resources {
			if r == resource {
				waiters = append(waiters, w)
				break
			}
		}
	}
	return waiters
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

func grant(st *state.State, tree, label string, resources []string, lease time.Duration, noSwap bool, taker int, now time.Time) {
	for _, name := range resources {
		r := st.Get(name)
		if h := r.ActiveHolder(now); h != nil && h.Tree == tree {
			if h.Kind == state.KindSession {
				h.Expires = now.Add(lease)
				h.NoSwap = noSwap
				h.HoldSince, h.Note = time.Time{}, ""
				h.Taker = taker
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
			Taker:   taker,
		}
	}
}

func Settle(st *state.State, tree string, taker int, now time.Time) {
	for _, name := range st.HeldBy(tree, now) {
		if h := st.Resources[name].Holder; h.Taker == taker {
			h.Taker = 0
		}
	}
}

func Takers(st *state.State, tree string, now time.Time) []int {
	seen := map[int]bool{}
	takers := []int{}
	for _, name := range st.HeldBy(tree, now) {
		if pid := st.Resources[name].Holder.Taker; pid > 0 && !seen[pid] {
			seen[pid] = true
			takers = append(takers, pid)
		}
	}
	for _, w := range st.Queue {
		if w.Tree == tree && w.PID > 0 && !seen[w.PID] {
			seen[w.PID] = true
			takers = append(takers, w.PID)
		}
	}
	return takers
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
	for _, name := range resources {
		if h := st.Lookup(name).ActiveHolder(now); h != nil && h.Tree != tree {
			result.Blockers = append(result.Blockers, Blocker{Resource: name, Holder: *h})
			if h.Pinned() {
				result.Outcome = Blocked
			}
		}
	}
	before := len(st.Queue)
	if position := Position(st, tree); position > 0 {
		before = position - 1
	}
	for _, w := range reservations(st, tree, resources, before, now) {
		result.Ahead = append(result.Ahead, w.Label)
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
