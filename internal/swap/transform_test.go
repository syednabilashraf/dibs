package swap

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/syednabilashraf/dibs/internal/tree"
)

func loadFixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/compose_inspect.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func worktreeMapper(source string) tree.Change {
	const main, wt = "/Users/dev/code/app", "/Users/dev/code/app-feature"
	if strings.HasPrefix(source, main+"/") {
		return tree.Change{Source: source, Result: wt + strings.TrimPrefix(source, main), Root: main, Mapped: true}
	}
	return tree.Change{Source: source, Result: source, Foreign: true}
}

func TestTransformRemapsBindsAndMounts(t *testing.T) {
	spec, err := Transform(loadFixture(t), worktreeMapper, map[string]string{LabelManaged: "1", LabelTree: "/Users/dev/code/app-feature"})
	if err != nil {
		t.Fatal(err)
	}
	host := obj(spec.Body["HostConfig"])
	binds := list(host["Binds"])
	want := []string{
		"/Users/dev/code/app-feature/web:/app:rw",
		"/Users/dev/code/app-feature/shared/config.json:/etc/app/config.json:ro",
		"/var/run/docker.sock:/var/run/docker.sock",
	}
	for i, w := range want {
		if str(binds[i]) != w {
			t.Fatalf("bind %d = %q, want %q", i, binds[i], w)
		}
	}
	mounts := list(host["Mounts"])
	if src := str(obj(mounts[1])["Source"]); src != "/Users/dev/code/app-feature/tools" {
		t.Fatalf("bind mount source = %q", src)
	}
	if spec.Mapped() != 3 {
		t.Fatalf("mapped = %d", spec.Mapped())
	}
	if roots := spec.Repo(); len(roots) != 1 || roots[0] != "/Users/dev/code/app" {
		t.Fatalf("roots = %v", roots)
	}
}

func TestTransformPinsAnonymousVolumes(t *testing.T) {
	spec, err := Transform(loadFixture(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	host := obj(spec.Body["HostConfig"])
	byTarget := map[string]string{}
	for _, m := range list(host["Mounts"]) {
		mount := obj(m)
		byTarget[str(mount["Target"])] = str(mount["Source"])
	}
	if byTarget["/app/node_modules"] != "a6d60ed1b23ec46a0809e5cca10eea70e3f44beebf991ee5de9c9afa37f7e99f" {
		t.Fatalf("anonymous volume must be pinned to its existing name, got %q", byTarget["/app/node_modules"])
	}
	if byTarget["/cache"] != "stack_cache" {
		t.Fatal("named volume must be untouched")
	}
	if byTarget["/var/cache/app"] != "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0" {
		t.Fatalf("image/Config.Volumes volume must be pinned, got %q", byTarget["/var/cache/app"])
	}
	if _, ok := spec.Body["Volumes"]; ok {
		t.Fatal("pinned Config.Volumes entries should be removed")
	}
}

func TestTransformConfigAndLabels(t *testing.T) {
	spec, err := Transform(loadFixture(t), nil, map[string]string{LabelManaged: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Body["Hostname"]; ok {
		t.Fatal("an auto hostname equal to the short id must be dropped")
	}
	if _, ok := spec.Body["MacAddress"]; ok {
		t.Fatal("MacAddress must be dropped")
	}
	labels := obj(spec.Body["Labels"])
	if str(labels["com.docker.compose.service"]) != "web" || str(labels[LabelManaged]) != "1" {
		t.Fatalf("labels = %v", labels)
	}
	if str(spec.Body["Image"]) != "registry.example.com/stack/web-dev" {
		t.Fatal("image must be kept")
	}
	memory := obj(spec.Body["HostConfig"])["Memory"]
	if n, ok := memory.(json.Number); !ok || n.String() != "8589934592" {
		t.Fatalf("large numbers must survive exactly, got %v (%T)", memory, memory)
	}
	if spec.Name != "web" {
		t.Fatalf("name = %q", spec.Name)
	}
}

func TestTransformNetworks(t *testing.T) {
	spec, err := Transform(loadFixture(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := obj(obj(spec.Body["NetworkingConfig"])["EndpointsConfig"])
	primary := obj(endpoints["stack_default"])
	aliases := list(primary["Aliases"])
	if len(aliases) != 1 || str(aliases[0]) != "web" {
		t.Fatalf("aliases must be deduped without the short id: %v", aliases)
	}
	for _, key := range []string{"MacAddress", "IPAddress", "NetworkID", "EndpointID", "DNSNames"} {
		if _, ok := primary[key]; ok {
			t.Fatalf("%s must not be copied", key)
		}
	}
	if len(endpoints) != 1 {
		t.Fatalf("only the primary network belongs in create: %v", endpoints)
	}
	extra := spec.Networks["stack_backend"]
	if extra == nil || obj(extra["IPAMConfig"])["IPv4Address"] != "10.10.0.5" {
		t.Fatalf("static IPAM on an extra network must be kept: %v", extra)
	}
}

func TestTransformSpecialNetworkModes(t *testing.T) {
	for _, mode := range []string{"host", "none", "container:abc"} {
		raw := loadFixture(t)
		obj(raw["HostConfig"])["NetworkMode"] = mode
		spec, err := Transform(raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(obj(obj(spec.Body["NetworkingConfig"])["EndpointsConfig"])) != 0 || len(spec.Networks) != 0 {
			t.Fatalf("%s mode must not configure endpoints", mode)
		}
	}
	raw := loadFixture(t)
	obj(raw["HostConfig"])["NetworkMode"] = "default"
	networks := obj(obj(raw["NetworkSettings"])["Networks"])
	networks["bridge"] = networks["stack_default"]
	delete(networks, "stack_default")
	spec, err := Transform(raw, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(obj(obj(spec.Body["NetworkingConfig"])["EndpointsConfig"])) != 0 {
		t.Fatal("the default bridge takes no endpoint config")
	}
	if _, ok := spec.Networks["bridge"]; ok {
		t.Fatal("the primary bridge must not be reconnected")
	}
}

func TestTransformDoesNotMutateInput(t *testing.T) {
	raw := loadFixture(t)
	if _, err := Transform(raw, worktreeMapper, map[string]string{LabelManaged: "1"}); err != nil {
		t.Fatal(err)
	}
	if str(list(obj(raw["HostConfig"])["Binds"])[0]) != "/Users/dev/code/app/web:/app:rw" {
		t.Fatal("the baseline snapshot must not be modified")
	}
}

func TestTransformKeptReportsMissingPaths(t *testing.T) {
	mapper := func(source string) tree.Change {
		if strings.HasSuffix(source, "tools") {
			return tree.Change{Source: source, Result: source, Root: "/Users/dev/code/app", Reason: "tools does not exist in feature"}
		}
		return worktreeMapper(source)
	}
	spec, err := Transform(loadFixture(t), mapper, nil)
	if err != nil {
		t.Fatal(err)
	}
	kept := spec.Kept()
	if len(kept) != 1 || !strings.HasSuffix(kept[0].Source, "tools") {
		t.Fatalf("kept = %+v", kept)
	}
}
