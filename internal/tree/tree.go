package tree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Tree struct {
	Path   string
	Common string
	GitDir string
	Branch string
	Label  string
	Main   bool
}

func Resolve(dir string) (*Tree, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	start := Clean(dir)
	if info, err := os.Stat(start); err == nil && !info.IsDir() {
		start = filepath.Dir(start)
	}
	for current := start; ; {
		gitPath := filepath.Join(current, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			return describe(current, gitPath, info.IsDir())
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("%s is not inside a git worktree", start)
		}
		current = parent
	}
}

func describe(root, gitPath string, isDir bool) (*Tree, error) {
	t := &Tree{Path: root, Main: isDir}
	if isDir {
		t.GitDir = Clean(gitPath)
		t.Common = t.GitDir
	} else {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return nil, err
		}
		line := strings.TrimSpace(string(data))
		gitDir, ok := strings.CutPrefix(line, "gitdir:")
		if !ok {
			return nil, fmt.Errorf("%s: unrecognised .git file", gitPath)
		}
		gitDir = strings.TrimSpace(gitDir)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		t.GitDir = Clean(gitDir)
		t.Common = t.GitDir
		if raw, err := os.ReadFile(filepath.Join(t.GitDir, "commondir")); err == nil {
			common := strings.TrimSpace(string(raw))
			if !filepath.IsAbs(common) {
				common = filepath.Join(t.GitDir, common)
			}
			t.Common = Clean(common)
		}
	}
	if head, err := os.ReadFile(filepath.Join(t.GitDir, "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/"); ok {
			t.Branch = ref
		}
	}
	t.Label = t.Branch
	if t.Label == "" {
		t.Label = filepath.Base(root)
	}
	return t, nil
}

func (t *Tree) SameRepo(other *Tree) bool {
	return t != nil && other != nil && t.Common == other.Common
}

func Clean(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

type Change struct {
	Source string
	Result string
	Root   string
	Mapped bool
	Reason string
}

func (c Change) Changed() bool { return c.Mapped && c.Result != c.Source }

type Mapper struct {
	owners map[string]*Tree
}

func NewMapper() *Mapper {
	return &Mapper{owners: map[string]*Tree{}}
}

func (m *Mapper) Owner(path string) *Tree {
	dir := Clean(path)
	if info, err := os.Stat(dir); err != nil {
		return nil
	} else if !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	if owner, ok := m.owners[dir]; ok {
		return owner
	}
	owner, err := Resolve(dir)
	if err != nil {
		owner = nil
	}
	m.owners[dir] = owner
	return owner
}

func (m *Mapper) Map(source string, target *Tree) Change {
	change := Change{Source: source, Result: source}
	owner := m.Owner(source)
	if owner == nil {
		change.Reason = "not inside a git worktree"
		return change
	}
	change.Root = owner.Path
	if target == nil || owner.Common != target.Common {
		change.Reason = "belongs to a different repository"
		return change
	}
	rel, err := filepath.Rel(owner.Path, Clean(source))
	if err != nil || strings.HasPrefix(rel, "..") {
		change.Reason = "outside its worktree"
		return change
	}
	candidate := filepath.Join(target.Path, rel)
	if _, err := os.Stat(candidate); err != nil {
		change.Reason = fmt.Sprintf("%s does not exist in %s", rel, target.Label)
		return change
	}
	if candidate == Clean(source) {
		candidate = source
	}
	change.Result = candidate
	change.Mapped = true
	return change
}
