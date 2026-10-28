package swap

import (
	"reflect"
	"strings"
	"testing"

	"github.com/syednabilashraf/dibs/internal/tree"
)

func TestPrivateSpecIsAQuietCopyOnTheWorktree(t *testing.T) {
	spec, err := PrivateSpec(loadFixture(t), worktreeMapper, Private{
		Name: "dibs-test-web-1", Tree: "/Users/dev/code/app-feature", Command: []string{"npx", "vitest", "run"},
		WorkDir: "/app", Env: []string{"CI=1"}, Memory: 8 << 30, PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, host := spec.Body, obj(spec.Body["HostConfig"])
	if spec.Name != "dibs-test-web-1" || !reflect.DeepEqual(body["Cmd"], []string{"npx", "vitest", "run"}) || len(body["Entrypoint"].([]string)) != 0 {
		t.Fatalf("name/cmd/entrypoint: %s %v %v", spec.Name, body["Cmd"], body["Entrypoint"])
	}
	if str(list(host["Binds"])[0]) != "/Users/dev/code/app-feature/web:/app:rw" {
		t.Fatalf("the worktree is mounted where the code goes: %v", host["Binds"])
	}
	if src := str(obj(list(host["Mounts"])[0])["Source"]); src == "" {
		t.Fatal("the shared dependency volume is reused, not replaced by an empty one")
	}
	if len(obj(host["PortBindings"])) != 0 || obj(host["RestartPolicy"])["Name"] != "no" || host["Memory"] != int64(8<<30) {
		t.Fatalf("no ports, no restarts, the memory cap: %v %v %v", host["PortBindings"], host["RestartPolicy"], host["Memory"])
	}
	labels := obj(body["Labels"])
	for key := range labels {
		if strings.HasPrefix(key, "com.docker.compose.") {
			t.Fatalf("compose must not mistake the copy for its service: %s", key)
		}
	}
	if labels[LabelTest] != "1" || labels[LabelTestPID] != "4242" {
		t.Fatalf("labels = %v", labels)
	}
	if env := list(body["Env"]); str(env[len(env)-1]) != "CI=1" || body["WorkingDir"] != "/app" {
		t.Fatalf("env/workdir: %v %v", env, body["WorkingDir"])
	}
	if obj(body["Healthcheck"])["Test"].([]string)[0] != "NONE" {
		t.Fatal("the copy runs no healthcheck")
	}
	for name, endpoint := range spec.Networks {
		if _, ok := endpoint["Aliases"]; ok {
			t.Fatalf("network %s: the copy must not answer to the service's aliases", name)
		}
	}
	for name, endpoint := range obj(obj(body["NetworkingConfig"])["EndpointsConfig"]) {
		if _, ok := obj(endpoint)["Aliases"]; ok {
			t.Fatalf("network %s: the copy must not answer to the service's aliases", name)
		}
	}
}

func TestPrivateSpecNeedsARepoMount(t *testing.T) {
	foreign := func(source string) tree.Change { return tree.Change{Source: source, Result: source, Foreign: true} }
	if _, err := PrivateSpec(loadFixture(t), foreign, Private{Name: "x", Command: []string{"true"}}); err != ErrNoRepoMounts {
		t.Fatalf("err = %v", err)
	}
}

func TestPrivateCommandAndMemory(t *testing.T) {
	if got := command([]string{"cd web && npm test"}); !reflect.DeepEqual(got, []string{"sh", "-c", "cd web && npm test"}) {
		t.Fatalf("a shell string runs through sh: %v", got)
	}
	if got := command([]string{"npm", "test"}); !reflect.DeepEqual(got, []string{"npm", "test"}) {
		t.Fatalf("separate arguments run as they are: %v", got)
	}
	for text, want := range map[string]int64{"": 0, "512m": 512 << 20, "8g": 8 << 30, "8GB": 8 << 30, "1.5g": 3 << 29, "1024": 1024} {
		if got, err := ParseMemory(text); err != nil || got != want {
			t.Fatalf("ParseMemory(%q) = %d, %v", text, got, err)
		}
	}
	if _, err := ParseMemory("lots"); err == nil {
		t.Fatal("nonsense is an error")
	}
}
