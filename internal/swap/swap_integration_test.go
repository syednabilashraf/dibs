package swap

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/docker"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

const testImage = "busybox:latest"

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type rig struct {
	container string
	network   string
	main      *tree.Tree
	feature   *tree.Tree
	swapper   *Swapper
	client    *docker.Client
	log       *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := docker.New(ctx)
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}
	if exec.Command("docker", "image", "inspect", testImage).Run() != nil {
		t.Skipf("%s is not present locally", testImage)
	}

	root := t.TempDir()
	mainDir := filepath.Join(root, "app")
	os.MkdirAll(filepath.Join(mainDir, "web"), 0o755)
	os.WriteFile(filepath.Join(mainDir, "web", "version.txt"), []byte("main\n"), 0o644)
	run(t, "git", "init", "-q", "-b", "main", mainDir)
	run(t, "git", "-C", mainDir, "add", ".")
	run(t, "git", "-C", mainDir, "commit", "-q", "-m", "init")
	featureDir := filepath.Join(root, "app-feature")
	run(t, "git", "-C", mainDir, "worktree", "add", "-q", "-b", "feature", featureDir)
	os.WriteFile(filepath.Join(featureDir, "web", "version.txt"), []byte("feature\n"), 0o644)

	suffix := fmt.Sprintf("%d", rand.Int63())
	r := &rig{container: "dibs-it-" + suffix, network: "dibs-it-net-" + suffix, client: client, log: &bytes.Buffer{}}
	run(t, "docker", "network", "create", r.network)
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "-v", r.container).Run()
		exec.Command("docker", "network", "rm", r.network).Run()
	})
	r.start(t, mainDir, "")

	r.main, _ = tree.Resolve(mainDir)
	r.feature, _ = tree.Resolve(featureDir)
	store, _ := state.Open(t.TempDir())
	cfg := config.Default()
	cfg.StopTimeout = config.Duration(time.Second)
	grace := config.Duration(0)
	cfg.ExecGrace = &grace
	cfg.Containers[r.container] = config.Container{Ready: config.Ready{Settle: config.Duration(500 * time.Millisecond)}}
	r.swapper = &Swapper{Docker: client, Store: store, Config: cfg, Out: r.log}
	return r
}

func (r *rig) start(t *testing.T, mainDir, extraEnv string) {
	t.Helper()
	args := []string{"run", "-d", "--name", r.container, "--network", r.network, "--network-alias", "webalias",
		"-v", filepath.Join(mainDir, "web") + ":/app:ro", "-v", "/data", "--label", "com.docker.compose.service=web"}
	if extraEnv != "" {
		args = append(args, "-e", extraEnv)
	}
	args = append(args, testImage, "sh", "-c", "echo started; sleep 3600")
	run(t, "docker", args...)
}

func (r *rig) exec(t *testing.T, command string) string {
	t.Helper()
	return run(t, "docker", "exec", r.container, "sh", "-c", command)
}

func (r *rig) inspect(t *testing.T) docker.Summary {
	t.Helper()
	raw, err := r.client.Inspect(context.Background(), r.container)
	if err != nil {
		t.Fatal(err)
	}
	return docker.Summarize(raw)
}

func TestIntegrationServeAndRestore(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.exec(t, "echo keep > /data/marker")
	original := r.inspect(t).ID

	outcome, err := r.swapper.Serve(ctx, r.container, r.feature)
	if err != nil {
		t.Fatalf("serve feature: %v\n%s", err, r.log)
	}
	if !outcome.Swapped || outcome.Serving != r.feature.Path {
		t.Fatalf("outcome = %+v", outcome)
	}
	if got := r.exec(t, "cat /app/version.txt"); got != "feature" {
		t.Fatalf("container serves %q, want feature", got)
	}
	if got := r.exec(t, "cat /data/marker"); got != "keep" {
		t.Fatalf("anonymous volume lost: %q", got)
	}
	summary := r.inspect(t)
	if summary.ID == original {
		t.Fatal("container should have been recreated")
	}
	if summary.Label(LabelTree) != r.feature.Path || summary.Label("com.docker.compose.service") != "web" {
		t.Fatalf("labels = %v", summary.Labels)
	}
	aliases := run(t, "docker", "inspect", "-f", "{{json .NetworkSettings.Networks}}", r.container)
	if !strings.Contains(aliases, "webalias") {
		t.Fatalf("network alias lost: %s", aliases)
	}

	st, _ := r.swapper.Store.Read()
	res := st.Lookup(r.container)
	if res.Serving != r.feature.Path || res.Status != state.StatusReady || res.ContainerID != summary.ID || res.Service != "web" {
		t.Fatalf("state = %+v", res)
	}
	if len(res.BaselineRoots) != 1 || res.BaselineRoots[0] != r.main.Path {
		t.Fatalf("baseline roots = %v", res.BaselineRoots)
	}

	again, err := r.swapper.Serve(ctx, r.container, r.feature)
	if err != nil || again.Swapped {
		t.Fatalf("serving the same tree again must be a no-op: %+v %v", again, err)
	}

	restored, err := r.swapper.Serve(ctx, r.container, nil)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, r.log)
	}
	if !restored.Swapped || restored.Previous != r.feature.Path {
		t.Fatalf("restore outcome = %+v", restored)
	}
	if got := r.exec(t, "cat /app/version.txt"); got != "main" {
		t.Fatalf("restored container serves %q", got)
	}
	if got := r.exec(t, "cat /data/marker"); got != "keep" {
		t.Fatalf("anonymous volume lost on restore: %q", got)
	}

	events, _ := r.swapper.Store.Events(time.Now().Add(-time.Minute))
	swaps := 0
	for _, ev := range events {
		if ev.Kind == "swap" && ev.Resource == r.container {
			swaps++
		}
	}
	if swaps != 2 {
		t.Fatalf("expected 2 swap events, got %d", swaps)
	}
}

func TestIntegrationMainTreeTakeIsBaseline(t *testing.T) {
	r := newRig(t)
	outcome, err := r.swapper.Serve(context.Background(), r.container, r.main)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Swapped || outcome.Serving != "" {
		t.Fatalf("taking from the baseline tree must not recreate: %+v", outcome)
	}
}

func TestIntegrationWaitsForRunningExecs(t *testing.T) {
	r := newRig(t)
	grace := config.Duration(20 * time.Second)
	r.swapper.Config.ExecGrace = &grace
	run(t, "docker", "exec", "-d", r.container, "sleep", "3")
	time.Sleep(300 * time.Millisecond)

	outcome, err := r.swapper.Serve(context.Background(), r.container, r.feature)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Waited < 2*time.Second || len(outcome.Interrupted) != 0 {
		t.Fatalf("swap should wait for the exec to finish: waited %v, interrupted %v", outcome.Waited, outcome.Interrupted)
	}
	if !strings.Contains(r.log.String(), "sleep 3") {
		t.Fatalf("the wait should name the exec it waits for:\n%s", r.log)
	}
}

func TestIntegrationRecreatedOutsideBecomesBaseline(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.swapper.Serve(ctx, r.container, r.feature); err != nil {
		t.Fatal(err)
	}
	run(t, "docker", "rm", "-f", r.container)
	r.start(t, r.main.Path, "MARK=rebuilt")

	if _, err := r.swapper.Serve(ctx, r.container, r.feature); err != nil {
		t.Fatal(err)
	}
	if got := r.exec(t, "echo $MARK"); got != "rebuilt" {
		t.Fatalf("a container recreated outside dibs must become the new baseline; MARK=%q", got)
	}
}

func TestIntegrationRecoversMissingContainer(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.swapper.Serve(ctx, r.container, r.feature); err != nil {
		t.Fatal(err)
	}
	run(t, "docker", "rm", "-f", r.container)
	outcome, err := r.swapper.Serve(ctx, r.container, r.feature)
	if err != nil {
		t.Fatalf("a missing container should be recreated from the baseline: %v", err)
	}
	if !outcome.Swapped {
		t.Fatal("expected a recreate")
	}
	if got := r.exec(t, "cat /app/version.txt"); got != "feature" {
		t.Fatalf("serves %q", got)
	}
}

func TestIntegrationRollbackOnFailedCreate(t *testing.T) {
	r := newRig(t)
	failed := false
	r.swapper.beforeCreate = func(spec *Spec) error {
		if !failed {
			failed = true
			return fmt.Errorf("injected failure")
		}
		return nil
	}
	_, err := r.swapper.Serve(context.Background(), r.container, r.feature)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected a rolled back failure, got %v", err)
	}
	if got := r.exec(t, "cat /app/version.txt"); got != "main" {
		t.Fatalf("rollback should leave the baseline running, got %q", got)
	}
	st, _ := r.swapper.Store.Read()
	res := st.Lookup(r.container)
	if res.Status != state.StatusFailed || res.Serving != "" || res.Intent != nil {
		t.Fatalf("state after rollback = %+v", res)
	}
}

func TestIntegrationReadinessFailureMarksFailed(t *testing.T) {
	r := newRig(t)
	r.swapper.Config.Containers[r.container] = config.Container{Ready: config.Ready{Log: "never printed"}}
	r.swapper.Config.ReadyTimeout = config.Duration(2 * time.Second)
	_, err := r.swapper.Serve(context.Background(), r.container, r.feature)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("expected a readiness failure, got %v", err)
	}
	st, _ := r.swapper.Store.Read()
	if st.Lookup(r.container).Status != state.StatusFailed {
		t.Fatalf("status should be failed: %+v", st.Lookup(r.container))
	}
}
