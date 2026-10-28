package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

func stackConfig(t *testing.T, body string) {
	t.Helper()
	os.WriteFile(filepath.Join(os.Getenv("DIBS_HOME"), "config.yaml"), []byte("stop_timeout: 1s\nexec_grace: 0s\n"+body), 0o644)
}

func TestCLITakeRefusesWhenTheMigrationsCheckObjects(t *testing.T) {
	s := newStack(t)
	stackConfig(t, fmt.Sprintf(`containers:
  %s:
    ready:
      settle: 500ms
    migrations:
      check: 'if [ -e web/migrate.sql ]; then echo "would apply migrate.sql"; exit 1; fi'
`, s.container))
	os.WriteFile(filepath.Join(s.b, "web", "migrate.sql"), []byte("alter"), 0o644)

	code, out := dibs(t, "take", s.container, "--tree", s.b)
	if code != exitNo || !strings.Contains(out, "would apply migrate.sql") || !strings.Contains(out, "--allow-migrations") {
		t.Fatalf("the take should be refused before anything moves: %d\n%s", code, out)
	}
	if got := s.serves(t); got != "main" {
		t.Fatalf("a refused take leaves the container alone, serves %q", got)
	}
	store, _ := state.Open(os.Getenv("DIBS_HOME"))
	if st, _ := store.Read(); len(st.HeldBy(tree.Clean(s.b), time.Now())) != 0 || queue.Position(st, tree.Clean(s.b)) != 0 {
		t.Fatal("a refused take neither holds nor waits")
	}
	if code, out := dibs(t, "take", s.container, "--tree", s.a); code != exitOK {
		t.Fatalf("a worktree without new migrations takes it: %d\n%s", code, out)
	}
	dibs(t, "pass", "--tree", s.a)
	if code, out := dibs(t, "take", s.container, "--tree", s.b, "--allow-migrations"); code != exitOK || s.serves(t) != "ticket-b" {
		t.Fatalf("--allow-migrations takes it anyway: %d\n%s", code, out)
	}
	if code, out := dibs(t, "take", s.container, "--tree", s.b); code != exitOK {
		t.Fatalf("taking again recreates nothing, so it is not checked: %d\n%s", code, out)
	}
}
