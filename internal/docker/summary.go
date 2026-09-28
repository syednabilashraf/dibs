package docker

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Summary struct {
	ID             string
	Name           string
	Image          string
	Running        bool
	Status         string
	Health         string
	HasHealthcheck bool
	Tty            bool
	StartedAt      time.Time
	Labels         map[string]string
	ExecIDs        []string
	HostPorts      []int
}

func (s Summary) Label(key string) string { return s.Labels[key] }

func Summarize(raw map[string]any) Summary {
	var view struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running   bool   `json:"Running"`
			Status    string `json:"Status"`
			StartedAt string `json:"StartedAt"`
			Health    *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		Config struct {
			Image       string            `json:"Image"`
			Tty         bool              `json:"Tty"`
			Labels      map[string]string `json:"Labels"`
			Healthcheck *struct {
				Test []string `json:"Test"`
			} `json:"Healthcheck"`
		} `json:"Config"`
		HostConfig struct {
			PortBindings map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
		ExecIDs []string `json:"ExecIDs"`
	}
	data, _ := json.Marshal(raw)
	json.Unmarshal(data, &view)

	s := Summary{
		ID:      view.ID,
		Name:    strings.TrimPrefix(view.Name, "/"),
		Image:   view.Config.Image,
		Running: view.State.Running,
		Status:  view.State.Status,
		Tty:     view.Config.Tty,
		Labels:  view.Config.Labels,
		ExecIDs: view.ExecIDs,
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	if t, err := time.Parse(time.RFC3339Nano, view.State.StartedAt); err == nil {
		s.StartedAt = t
	}
	if view.State.Health != nil {
		s.Health = view.State.Health.Status
	}
	if hc := view.Config.Healthcheck; hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE" {
		s.HasHealthcheck = true
	}
	seen := map[int]bool{}
	for _, bindings := range view.HostConfig.PortBindings {
		for _, b := range bindings {
			if port, err := strconv.Atoi(b.HostPort); err == nil && port > 0 && !seen[port] {
				seen[port] = true
				s.HostPorts = append(s.HostPorts, port)
			}
		}
	}
	sort.Ints(s.HostPorts)
	return s
}
