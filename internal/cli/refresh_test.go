package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/state"
)

func startedAt(t *testing.T, name string) time.Time {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.StartedAt}}", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(out)))
	return at
}

func TestCLIRefreshRestartsDeclaredContainersOlderThanTheWatchedFiles(t *testing.T) {
	s := newStack(t)
	watched := filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(watched, []byte("old"), 0o644)
	os.Chtimes(watched, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	stackConfig(t, fmt.Sprintf(`containers:
  %s:
    ready:
      settle: 500ms
refresh:
  containers: [%s]
  watch: [%s]
  check: 'test -s %s || { echo "no credentials"; exit 1; }'
`, s.container, s.container, watched, watched))

	before := startedAt(t, s.container)
	if code, out := dibs(t, "refresh"); code != exitOK || !strings.Contains(out, "nothing to restart") {
		t.Fatalf("the container is newer than the credentials: %d\n%s", code, out)
	}
	os.WriteFile(watched, []byte("fresh"), 0o644)
	if code, out := dibs(t, "refresh"); code != exitOK || !strings.Contains(out, s.container+": restarted") {
		t.Fatalf("refresh: %d\n%s", code, out)
	}
	if !startedAt(t, s.container).After(before) {
		t.Fatal("the container should have been restarted")
	}
	if code, out := dibs(t, "refresh", "not-declared"); code != exitError || !strings.Contains(out, "not declared under refresh.containers") {
		t.Fatalf("only declared containers can be refreshed: %d\n%s", code, out)
	}
	os.WriteFile(watched, nil, 0o644)
	if code, out := dibs(t, "refresh"); code != exitNo || !strings.Contains(out, "no credentials") {
		t.Fatalf("a failing host check stops the refresh: %d\n%s", code, out)
	}
}

func TestCLIRefreshRespectsLeasesAndMigrationChecks(t *testing.T) {
	s := newStack(t)
	stackConfig(t, fmt.Sprintf(`containers:
  %s:
    ready:
      settle: 500ms
    migrations:
      check: 'if [ -e web/migrate.sql ]; then echo "would apply migrate.sql"; exit 1; fi'
refresh:
  containers: [%s]
`, s.container, s.container))
	if code, out := dibs(t, "take", s.container, "--tree", s.a); code != exitOK {
		t.Fatalf("take: %d %s", code, out)
	}
	if code, out := dibs(t, "refresh", "--all"); code != exitNo || !strings.Contains(out, "held by ticket-a") {
		t.Fatalf("a container held by another worktree is left alone: %d\n%s", code, out)
	}
	os.WriteFile(filepath.Join(s.a, "web", "migrate.sql"), []byte("alter"), 0o644)
	store, _ := state.Open(os.Getenv("DIBS_HOME"))
	store.Update(func(st *state.State) error {
		st.Resources[s.container].Holder = nil
		return nil
	})
	if code, out := dibs(t, "refresh", "--all"); code != exitNo || !strings.Contains(out, "would apply migrate.sql") {
		t.Fatalf("a restart re-runs boot steps, so the migrations check applies: %d\n%s", code, out)
	}
	if code, out := dibs(t, "refresh", "--all", "--allow-migrations"); code != exitOK || !strings.Contains(out, "restarted") {
		t.Fatalf("--allow-migrations restarts it anyway: %d\n%s", code, out)
	}
}
