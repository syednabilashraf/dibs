package swap

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/syednabilashraf/dibs/internal/tree"
)

const (
	LabelTest    = "dibs.test"
	LabelTestPID = "dibs.test.pid"
)

type Private struct {
	Name    string
	Tree    string
	Command []string
	WorkDir string
	Env     []string
	Memory  int64
	PID     int
}

func Worktree(target *tree.Tree) MapFunc {
	mapper := tree.NewMapper()
	return func(source string) tree.Change { return mapper.Map(source, target) }
}

func PrivateSpec(inspect map[string]any, mapSource MapFunc, p Private) (*Spec, error) {
	labels := map[string]string{LabelTest: "1", LabelTestPID: strconv.Itoa(p.PID), LabelTree: p.Tree}
	spec, err := Transform(inspect, mapSource, labels)
	if err != nil {
		return nil, err
	}
	mounted := false
	for _, change := range spec.Changes {
		mounted = mounted || change.Mapped
	}
	if !mounted {
		return nil, ErrNoRepoMounts
	}
	spec.Name = p.Name
	body := spec.Body

	if merged, ok := body["Labels"].(map[string]any); ok {
		for key := range merged {
			if strings.HasPrefix(key, "com.docker.compose.") || key == LabelManaged || key == LabelBranch {
				delete(merged, key)
			}
		}
	}
	body["Entrypoint"] = []string{}
	body["Cmd"] = command(p.Command)
	body["Tty"], body["OpenStdin"], body["AttachStdin"], body["StdinOnce"] = false, false, false, false
	body["Healthcheck"] = map[string]any{"Test": []string{"NONE"}}
	delete(body, "Hostname")
	delete(body, "ExposedPorts")
	if p.WorkDir != "" {
		body["WorkingDir"] = p.WorkDir
	}
	if len(p.Env) > 0 {
		env, _ := body["Env"].([]any)
		for _, kv := range p.Env {
			env = append(env, kv)
		}
		body["Env"] = env
	}

	host := obj(body["HostConfig"])
	host["PortBindings"] = map[string]any{}
	host["PublishAllPorts"] = false
	host["RestartPolicy"] = map[string]any{"Name": "no"}
	host["AutoRemove"] = false
	delete(host, "Links")
	if p.Memory > 0 {
		host["Memory"] = p.Memory
		host["MemorySwap"] = p.Memory
	}

	for _, endpoint := range obj(obj(body["NetworkingConfig"])["EndpointsConfig"]) {
		private(obj(endpoint))
	}
	for _, endpoint := range spec.Networks {
		private(endpoint)
	}
	return spec, nil
}

func private(endpoint map[string]any) {
	for _, key := range []string{"Aliases", "Links", "IPAMConfig", "IPAddress", "MacAddress", "DNSNames"} {
		delete(endpoint, key)
	}
}

func command(args []string) []string {
	if len(args) == 1 && strings.ContainsAny(args[0], " \t;&|<>()$`\"'*?") {
		return []string{"sh", "-c", args[0]}
	}
	return args
}

func ParseMemory(text string) (int64, error) {
	size := strings.TrimSuffix(strings.TrimSpace(strings.ToLower(text)), "b")
	if size == "" {
		return 0, nil
	}
	multiplier := map[byte]int64{'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30}[size[len(size)-1]]
	if multiplier > 0 {
		size = size[:len(size)-1]
	} else {
		multiplier = 1
	}
	value, err := strconv.ParseFloat(size, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("memory %q: use a size such as 512m or 8g", text)
	}
	return int64(value * float64(multiplier)), nil
}
