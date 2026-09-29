package swap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/syednabilashraf/dibs/internal/tree"
)

const (
	LabelManaged = "dibs.managed"
	LabelTree    = "dibs.tree"
	LabelBranch  = "dibs.label"
)

type MapFunc func(source string) tree.Change

type Spec struct {
	Name     string
	Body     map[string]any
	Networks map[string]map[string]any
	Changes  []tree.Change
}

func (s *Spec) Mapped() int {
	n := 0
	for _, c := range s.Changes {
		if c.Changed() {
			n++
		}
	}
	return n
}

func (s *Spec) Kept() []tree.Change {
	kept := []tree.Change{}
	for _, c := range s.Changes {
		if !c.Mapped && !c.Foreign {
			kept = append(kept, c)
		}
	}
	return kept
}

func (s *Spec) Repo() (roots []string) {
	seen := map[string]bool{}
	for _, c := range s.Changes {
		if c.Root != "" && !c.Foreign && !seen[c.Root] {
			seen[c.Root] = true
			roots = append(roots, c.Root)
		}
	}
	return roots
}

func Transform(inspect map[string]any, mapSource MapFunc, labels map[string]string) (*Spec, error) {
	raw, err := deepCopy(inspect)
	if err != nil {
		return nil, err
	}
	id := str(raw["Id"])
	name := strings.TrimPrefix(str(raw["Name"]), "/")
	config := obj(raw["Config"])
	host := obj(raw["HostConfig"])
	if name == "" || config == nil || host == nil {
		return nil, fmt.Errorf("inspect data is missing Name, Config or HostConfig")
	}
	short := id
	if len(short) > 12 {
		short = short[:12]
	}

	if hostname := str(config["Hostname"]); hostname != "" && hostname == short {
		delete(config, "Hostname")
	}
	delete(config, "MacAddress")

	merged := obj(config["Labels"])
	if merged == nil {
		merged = map[string]any{}
	}
	for k, v := range labels {
		merged[k] = v
	}
	config["Labels"] = merged

	spec := &Spec{Name: name, Networks: map[string]map[string]any{}}
	remap := func(source string) string {
		if mapSource == nil {
			return source
		}
		change := mapSource(source)
		spec.Changes = append(spec.Changes, change)
		return change.Result
	}

	covered := map[string]bool{}
	if binds, ok := host["Binds"].([]any); ok {
		for i, b := range binds {
			source, rest := splitBind(str(b))
			if source == "" {
				continue
			}
			binds[i] = remap(source) + rest
			if dest := bindDestination(rest); dest != "" {
				covered[dest] = true
			}
		}
	}

	effective := volumesByDestination(raw["Mounts"])
	mounts, _ := host["Mounts"].([]any)
	for _, m := range mounts {
		mount := obj(m)
		if mount == nil {
			continue
		}
		target := str(mount["Target"])
		covered[target] = true
		switch str(mount["Type"]) {
		case "bind":
			mount["Source"] = remap(str(mount["Source"]))
		case "volume":
			if str(mount["Source"]) == "" {
				if volume := effective[target]; volume != "" {
					mount["Source"] = volume
				}
			}
		}
	}

	if volumes := obj(config["Volumes"]); volumes != nil {
		for path := range volumes {
			if covered[path] {
				continue
			}
			if volume := effective[path]; volume != "" {
				mounts = append(mounts, map[string]any{"Type": "volume", "Source": volume, "Target": path})
				delete(volumes, path)
			}
		}
		if len(volumes) == 0 {
			delete(config, "Volumes")
		}
	}
	if len(mounts) > 0 {
		host["Mounts"] = mounts
	}

	endpoints := map[string]any{}
	mode := str(host["NetworkMode"])
	primary := mode
	if primary == "default" || primary == "" {
		primary = "bridge"
	}
	attachable := mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:")
	if attachable {
		networks := obj(obj(raw["NetworkSettings"])["Networks"])
		for network, ep := range networks {
			endpoint := endpointConfig(obj(ep), id, short, network != "bridge")
			if network == primary {
				if network != "bridge" {
					endpoints[network] = endpoint
				}
				continue
			}
			spec.Networks[network] = endpoint
		}
	}

	body := map[string]any{}
	for k, v := range config {
		body[k] = v
	}
	body["HostConfig"] = host
	body["NetworkingConfig"] = map[string]any{"EndpointsConfig": endpoints}
	spec.Body = body
	return spec, nil
}

func endpointConfig(ep map[string]any, id, short string, aliasesAllowed bool) map[string]any {
	out := map[string]any{}
	if ep == nil {
		return out
	}
	if aliasesAllowed {
		seen := map[string]bool{}
		aliases := []any{}
		for _, a := range list(ep["Aliases"]) {
			alias := str(a)
			if alias == "" || alias == id || alias == short || seen[alias] {
				continue
			}
			seen[alias] = true
			aliases = append(aliases, alias)
		}
		if len(aliases) > 0 {
			out["Aliases"] = aliases
		}
	}
	for _, key := range []string{"IPAMConfig", "Links", "DriverOpts"} {
		if value := ep[key]; !empty(value) {
			out[key] = value
		}
	}
	return out
}

func splitBind(bind string) (source, rest string) {
	if i := strings.Index(bind, ":"); i > 0 {
		return bind[:i], bind[i:]
	}
	return "", bind
}

func bindDestination(rest string) string {
	parts := strings.SplitN(strings.TrimPrefix(rest, ":"), ":", 2)
	return parts[0]
}

func volumesByDestination(mounts any) map[string]string {
	out := map[string]string{}
	for _, m := range list(mounts) {
		mount := obj(m)
		if str(mount["Type"]) == "volume" && str(mount["Name"]) != "" {
			out[str(mount["Destination"])] = str(mount["Name"])
		}
	}
	return out
}

func deepCopy(in map[string]any) (map[string]any, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}
