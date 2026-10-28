package baseline

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
)

type Git struct {
	Dir string
}

func (g Git) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func (g Git) read(ctx context.Context, args ...string) (string, error) {
	return g.run(ctx, append([]string{"--no-optional-locks"}, args...)...)
}

type Status struct {
	Path    string
	Branch  string
	Head    string
	Remote  string
	Target  string
	Behind  int
	Ahead   int
	Dirty   []string
	Fetched time.Time
}

func (s Status) TargetRef() string { return s.Remote + "/" + s.Target }

func (s Status) Obstacles(stash bool) []string {
	obstacles := []string{}
	switch {
	case s.Branch == "":
		obstacles = append(obstacles, fmt.Sprintf("its HEAD is detached at %s; check out %s there first", s.Head, s.Target))
	case s.Branch != s.Target:
		obstacles = append(obstacles, fmt.Sprintf("it is on branch %s, not %s", s.Branch, s.Target))
	}
	if s.Ahead > 0 {
		obstacles = append(obstacles, fmt.Sprintf("it has %d commit(s) that are not on %s, so it cannot fast-forward", s.Ahead, s.TargetRef()))
	}
	if len(s.Dirty) > 0 && !stash {
		obstacles = append(obstacles, fmt.Sprintf("it has local changes (%s); commit or stash them first, or pass --stash to keep them in git stash", strings.Join(s.Dirty, ", ")))
	}
	return obstacles
}

func Inspect(ctx context.Context, path string, cfg config.Baseline) (Status, error) {
	g := Git{Dir: path}
	st := Status{Path: path}
	var err error
	if st.Head, err = g.read(ctx, "rev-parse", "--short", "HEAD"); err != nil {
		return st, err
	}
	st.Branch, _ = g.read(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	st.Remote, st.Target = cfg.Remote, cfg.Branch
	if st.Remote == "" && st.Branch != "" {
		st.Remote, _ = g.read(ctx, "config", "branch."+st.Branch+".remote")
	}
	if st.Remote == "" {
		st.Remote = "origin"
	}
	if st.Target == "" {
		if head, err := g.read(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+st.Remote+"/HEAD"); err == nil {
			st.Target = strings.TrimPrefix(head, st.Remote+"/")
		}
	}
	if st.Target == "" && st.Branch != "" {
		merge, _ := g.read(ctx, "config", "branch."+st.Branch+".merge")
		st.Target = strings.TrimPrefix(merge, "refs/heads/")
	}
	if st.Target == "" {
		return st, fmt.Errorf("%s: cannot tell which remote branch it follows; set repos[].baseline.branch in the dibs config", path)
	}
	counts, err := g.read(ctx, "rev-list", "--left-right", "--count", "HEAD..."+st.TargetRef())
	if err != nil {
		return st, err
	}
	if fields := strings.Fields(counts); len(fields) == 2 {
		st.Ahead, _ = strconv.Atoi(fields[0])
		st.Behind, _ = strconv.Atoi(fields[1])
	}
	porcelain, err := g.read(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) > 3 {
			st.Dirty = append(st.Dirty, strings.TrimSpace(line[3:]))
		}
	}
	if entry, err := g.read(ctx, "reflog", "show", "--date=unix", "-n1", "--format=%gd", "refs/remotes/"+st.TargetRef()); err == nil {
		if i, j := strings.LastIndex(entry, "{"), strings.LastIndex(entry, "}"); i >= 0 && j > i {
			if secs, err := strconv.ParseInt(entry[i+1:j], 10, 64); err == nil {
				st.Fetched = time.Unix(secs, 0)
			}
		}
	}
	return st, nil
}

func Fetch(ctx context.Context, path, remote, branch string) error {
	_, err := Git{Dir: path}.run(ctx, "fetch", "--quiet", remote, branch)
	return err
}

func Stash(ctx context.Context, path, message string) error {
	_, err := Git{Dir: path}.run(ctx, "stash", "push", "--quiet", "-m", message)
	return err
}

func Resolve(ctx context.Context, path, ref string) (string, error) {
	return Git{Dir: path}.read(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

func FastForward(ctx context.Context, path, ref string) error {
	g := Git{Dir: path}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err = g.run(ctx, "merge", "--ff-only", "--quiet", ref); err == nil || !strings.Contains(err.Error(), "index.lock") {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return err
}

func Export(ctx context.Context, repo, rev string, paths []string, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := append([]string{"-C", repo, "archive", "--format=tar", rev, "--"}, paths...)
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extract(tar.NewReader(stdout), dest)
	io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %s", rev, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

func extract(r *tar.Reader, dest string) error {
	for {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, r)
			if err := file.Close(); copyErr == nil {
				copyErr = err
			}
			if copyErr != nil {
				return copyErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Symlink(header.Linkname, target)
		}
	}
}
