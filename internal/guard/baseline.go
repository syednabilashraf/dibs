package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

func BaselineRoots(cfg *config.Config, st *state.State) map[string][]string {
	roots := map[string][]string{}
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Virtual {
			continue
		}
		for _, root := range r.BaselineRoots {
			roots[root] = append(roots[root], name)
		}
	}
	for _, repo := range cfg.Repos {
		if t, err := tree.Resolve(repo); err == nil {
			if _, ok := roots[t.Path]; !ok {
				roots[t.Path] = nil
			}
		}
	}
	for root := range roots {
		sort.Strings(roots[root])
	}
	return roots
}

func owningTree(path string) *tree.Tree {
	dir := path
	for {
		if info, err := os.Stat(dir); err == nil {
			if !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
	t, err := tree.Resolve(dir)
	if err != nil {
		return nil
	}
	return t
}

func baselineWhat(containers []string) string {
	if len(containers) == 0 {
		return "the shared containers"
	}
	return strings.Join(containers, ", ")
}

func worktreeAdvice() string {
	return "Create one worktree for this session and move the session into it before changing anything: the EnterWorktree tool does both, or pass it `path` to enter a worktree you made with `git worktree add ../<name> -b <branch>`. If you cannot move the session, ask the user to start a session in the worktree. One worktree per session is enough: for follow-up work, switch branches inside it once the previous branch is pushed. Reading, searching and planning here are fine."
}

func baselineSelfMessage(root string, containers []string) string {
	return fmt.Sprintf("dibs: %s is the baseline checkout: %s run this code for every session that does not hold a lease, so changing files or branches here changes what everyone is testing. %s", root, baselineWhat(containers), worktreeAdvice())
}
