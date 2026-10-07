package swap

import (
	"strings"
	"testing"

	"github.com/syednabilashraf/dibs/internal/docker"
)

func TestSummarizeChanges(t *testing.T) {
	changes := []docker.FileChange{
		{Path: "/opt", Kind: 0},
		{Path: "/opt/venv", Kind: 0},
		{Path: "/opt/venv/lib/python3.12/site-packages", Kind: 0},
		{Path: "/opt/venv/lib/python3.12/site-packages/llm", Kind: 1},
		{Path: "/opt/venv/lib/python3.12/site-packages/llm/quota.py", Kind: 1},
		{Path: "/opt/venv/lib/python3.12/site-packages/llm/__init__.py", Kind: 0},
		{Path: "/opt/venv/lib/python3.12/site-packages/llm/__pycache__/quota.cpython-312.pyc", Kind: 1},
		{Path: "/usr/local/bin/segment-cli", Kind: 1},
		{Path: "/tmp/dibs-wt-1234/test.py", Kind: 1},
		{Path: "/var/log/app.log", Kind: 0},
		{Path: "/root/.cache/pip/http/x", Kind: 1},
		{Path: "/app/node_modules/.cache/babel/y", Kind: 1},
		{Path: "/data", Kind: 1},
		{Path: "/root/.config", Kind: 1},
		{Path: "/root/.config/gcloud", Kind: 1},
	}
	got := SummarizeChanges(changes, []string{"/app", "/data", "/root/.config/gcloud"})
	if got.Files != 3 {
		t.Fatalf("only leaf changes outside scratch, log and cache paths count: %+v", got)
	}
	got.Bytes = 54 << 20
	text := got.String()
	if !strings.Contains(text, "/opt/venv/lib/python3.12/site-packages (2)") || !strings.Contains(text, "/usr/local/bin (1)") {
		t.Fatalf("changes should be grouped by dependency directory: %s", text)
	}
	if strings.Contains(text, "/tmp") || strings.Contains(text, "__pycache__") || strings.Contains(text, ".cache") {
		t.Fatalf("noise must not be reported: %s", text)
	}
	if strings.Contains(text, "/data") || strings.Contains(text, "/root/.config") {
		t.Fatalf("mount points and their parent directories are not runtime changes: %s", text)
	}
	if empty := SummarizeChanges([]docker.FileChange{{Path: "/tmp/x", Kind: 1}, {Path: "/run/app.pid", Kind: 1}}, nil); empty.Files != 0 {
		t.Fatalf("scratch-only changes are not worth a warning: %+v", empty)
	}
}

func TestDroppedWording(t *testing.T) {
	if (Dropped{Bytes: 5 << 20}).Worth() {
		t.Fatal("a small writable layer that could not be listed is not worth a warning")
	}
	if !(Dropped{Bytes: 54 << 20}).Worth() {
		t.Fatal("a large writable layer that could not be listed is worth a warning")
	}
	if (Dropped{Bytes: 54 << 20, Listed: true}).Worth() {
		t.Fatal("when the listing shows only noise, the size alone is not worth a warning")
	}
	if !(Dropped{Bytes: 10, Files: 1, Listed: true}).Worth() {
		t.Fatal("any listed runtime change is worth a warning")
	}
	if got := (Dropped{Bytes: 2500 << 20}).String(); got != "2.4 GB of changes outside its volumes" {
		t.Fatalf("String() = %q", got)
	}
	if got := (Dropped{Bytes: 54 << 20, Files: 3, Groups: []string{"/opt/venv/lib/python3.12/site-packages (3)"}}).String(); !strings.Contains(got, "54 MB of changes outside its volumes (3 files, mostly in /opt/venv") {
		t.Fatalf("String() = %q", got)
	}
}
