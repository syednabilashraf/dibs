package swap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/syednabilashraf/dibs/internal/docker"
)

var ignoredChangePrefixes = []string{
	"/tmp", "/var/tmp", "/run", "/var/run", "/var/log", "/var/cache", "/proc", "/sys", "/dev",
	"/root/.cache", "/root/.npm", "/root/.bash_history", "/root/.python_history",
	"/etc/hostname", "/etc/hosts", "/etc/resolv.conf", "/etc/mtab",
}

var dependencyMarkers = []string{"site-packages", "dist-packages", "node_modules", "pkg/mod"}

const dropWarnBytes = 10 << 20

type Dropped struct {
	Bytes  int64
	Files  int
	Groups []string
	Listed bool
}

func (d Dropped) Worth() bool {
	if d.Listed {
		return d.Files > 0
	}
	return d.Bytes >= dropWarnBytes
}

func (d Dropped) String() string {
	text := humanBytes(d.Bytes) + " of changes outside its volumes"
	if d.Files > 0 {
		text = fmt.Sprintf("%s (%d files", text, d.Files)
		if len(d.Groups) > 0 {
			text += ", mostly in " + strings.Join(d.Groups, ", ")
		}
		text += ")"
	}
	return text
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func SummarizeChanges(changes []docker.FileChange, mounts []string) Dropped {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	sort.Strings(paths)
	counts := map[string]int{}
	files := 0
	for i, path := range paths {
		if i+1 < len(paths) && strings.HasPrefix(paths[i+1], path+"/") {
			continue
		}
		if ignoredChange(path) || underAny(path, mounts) {
			continue
		}
		files++
		counts[changeGroup(path)]++
	}
	groups := make([]string, 0, len(counts))
	for group := range counts {
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if counts[groups[i]] != counts[groups[j]] {
			return counts[groups[i]] > counts[groups[j]]
		}
		return groups[i] < groups[j]
	})
	if len(groups) > 3 {
		groups = groups[:3]
	}
	for i, group := range groups {
		groups[i] = fmt.Sprintf("%s (%d)", group, counts[group])
	}
	return Dropped{Files: files, Groups: groups}
}

func underAny(path string, roots []string) bool {
	for _, root := range roots {
		if root != "" && (path == root || strings.HasPrefix(path, root+"/")) {
			return true
		}
	}
	return false
}

func ignoredChange(path string) bool {
	for _, prefix := range ignoredChangePrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return strings.Contains(path, "/__pycache__") || strings.HasSuffix(path, ".pyc") || strings.Contains(path, "/.cache/")
}

func changeGroup(path string) string {
	for _, marker := range dependencyMarkers {
		if i := strings.Index(path, "/"+marker); i >= 0 {
			return path[:i+len(marker)+1]
		}
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 4)
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return "/" + strings.Join(parts, "/")
}
