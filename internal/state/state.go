package state

import (
	"sort"
	"time"
)

type Kind string

const (
	KindSession Kind = "session"
	KindManual  Kind = "manual"
)

type Status string

const (
	StatusReady    Status = "ready"
	StatusSwapping Status = "swapping"
	StatusFailed   Status = "failed"
)

type Holder struct {
	Tree    string    `json:"tree"`
	Label   string    `json:"label"`
	Kind    Kind      `json:"kind"`
	Since   time.Time `json:"since"`
	Expires time.Time `json:"expires,omitempty"`
	Note    string    `json:"note,omitempty"`
	NoSwap  bool      `json:"no_swap,omitempty"`
}

func (h *Holder) Pinned() bool { return h != nil && h.Kind == KindManual }

func (h *Holder) Expired(now time.Time) bool {
	return h != nil && h.Kind == KindSession && !h.Expires.IsZero() && now.After(h.Expires)
}

type Intent struct {
	Target  string    `json:"target"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

type Resource struct {
	Virtual       bool      `json:"virtual,omitempty"`
	Holder        *Holder   `json:"holder,omitempty"`
	Serving       string    `json:"serving,omitempty"`
	ServingLabel  string    `json:"serving_label,omitempty"`
	Status        Status    `json:"status,omitempty"`
	Intent        *Intent   `json:"intent,omitempty"`
	ContainerID   string    `json:"container_id,omitempty"`
	Service       string    `json:"service,omitempty"`
	Project       string    `json:"project,omitempty"`
	Ports         []int     `json:"ports,omitempty"`
	Repo          string    `json:"repo,omitempty"`
	BaselineRoots []string  `json:"baseline_roots,omitempty"`
	SwappedAt     time.Time `json:"swapped_at,omitempty"`
}

func (r *Resource) ActiveHolder(now time.Time) *Holder {
	if r == nil || r.Holder == nil || r.Holder.Expired(now) {
		return nil
	}
	return r.Holder
}

type Waiter struct {
	ID        string        `json:"id"`
	Tree      string        `json:"tree"`
	Label     string        `json:"label"`
	Resources []string      `json:"resources"`
	PID       int           `json:"pid"`
	Lease     time.Duration `json:"lease"`
	NoSwap    bool          `json:"no_swap,omitempty"`
	Since     time.Time     `json:"since"`
}

type State struct {
	Resources map[string]*Resource `json:"resources"`
	Queue     []Waiter             `json:"queue"`
}

func New() *State {
	return &State{Resources: map[string]*Resource{}, Queue: []Waiter{}}
}

func (s *State) normalize() {
	if s.Resources == nil {
		s.Resources = map[string]*Resource{}
	}
	if s.Queue == nil {
		s.Queue = []Waiter{}
	}
}

func (s *State) Get(name string) *Resource {
	s.normalize()
	r, ok := s.Resources[name]
	if !ok {
		r = &Resource{}
		s.Resources[name] = r
	}
	return r
}

func (s *State) Lookup(name string) *Resource {
	if s.Resources == nil {
		return nil
	}
	return s.Resources[name]
}

func (s *State) Names() []string {
	names := make([]string, 0, len(s.Resources))
	for name := range s.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *State) HeldBy(tree string, now time.Time) []string {
	held := []string{}
	for _, name := range s.Names() {
		if h := s.Resources[name].ActiveHolder(now); h != nil && h.Tree == tree {
			held = append(held, name)
		}
	}
	return held
}
