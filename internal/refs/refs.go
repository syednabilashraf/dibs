package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/tree"
)

var unsafe = regexp.MustCompile(`[^a-z0-9]+`)

func Dir(home, owner string) string {
	slug := strings.Trim(unsafe.ReplaceAllString(strings.ToLower(filepath.Base(owner)), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(home, "refs", slug+"-"+hex.EncodeToString(sum[:4]))
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func Prepare(ctx context.Context, home string, owner *tree.Tree, ref string, patches []string) (*tree.Tree, error) {
	commit, err := git(ctx, owner.Path, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%s is not a commit in %s", ref, owner.Path)
	}
	path := Dir(home, owner.Path)
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		if _, err := git(ctx, path, "checkout", "--quiet", "--detach", "--force", commit); err != nil {
			return nil, err
		}
		if _, err := git(ctx, path, "clean", "-fdq"); err != nil {
			return nil, err
		}
	} else {
		os.RemoveAll(path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		git(ctx, owner.Path, "worktree", "prune")
		if _, err := git(ctx, owner.Path, "worktree", "add", "--quiet", "--detach", "--force", path, commit); err != nil {
			return nil, err
		}
	}
	for _, patch := range patches {
		absolute, _ := filepath.Abs(patch)
		if _, err := git(ctx, path, "apply", "--whitespace=nowarn", absolute); err != nil {
			return nil, fmt.Errorf("apply %s on %s: %w", patch, ref, err)
		}
	}
	if err := os.WriteFile(path+".owner", []byte(owner.Path+"\n"), 0o644); err != nil {
		return nil, err
	}
	t, err := tree.Resolve(path)
	if err != nil {
		return nil, err
	}
	t.Label = fmt.Sprintf("%s at %s", ref, commit[:min(10, len(commit))])
	if len(patches) > 0 {
		t.Label += " with " + strings.Join(baseNames(patches), ", ")
	}
	return t, nil
}

func Prune(ctx context.Context, home string, inUse func(path string) bool) []string {
	owners, _ := filepath.Glob(filepath.Join(home, "refs", "*.owner"))
	removed := []string{}
	for _, marker := range owners {
		data, err := os.ReadFile(marker)
		if err != nil {
			continue
		}
		owner := strings.TrimSpace(string(data))
		path := tree.Clean(strings.TrimSuffix(marker, ".owner"))
		if _, err := os.Stat(owner); err == nil || inUse(path) {
			continue
		}
		if t, err := tree.Resolve(path); err == nil {
			git(ctx, home, "--git-dir="+t.Common, "worktree", "remove", "--force", path)
		}
		os.RemoveAll(path)
		os.Remove(marker)
		removed = append(removed, path)
	}
	return removed
}

func baseNames(paths []string) []string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}
