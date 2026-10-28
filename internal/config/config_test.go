package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DIBS_CONFIG", path)
}

func TestDefaultsWithoutFile(t *testing.T) {
	t.Setenv("DIBS_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lease.D() != 30*time.Minute || cfg.StopTimeout.D() != 10*time.Second || cfg.ExecGrace.D() != 5*time.Minute || cfg.WaitTimeout.D() != 0 {
		t.Fatalf("unexpected duration defaults: %+v", cfg)
	}
	if !cfg.IsVirtual("browser") {
		t.Fatal("browser should be virtual by default")
	}
	if !cfg.Guard.CheckPorts() {
		t.Fatal("port checks should default on")
	}
	if !cfg.Guard.ProtectsBaseline() {
		t.Fatal("baseline protection should default on")
	}
	if cfg.Browser.Port != 9222 || filepath.Base(cfg.Browser.Profile) != "chrome-profile" {
		t.Fatalf("unexpected browser defaults: %+v", cfg.Browser)
	}
}

func TestLoadOverrides(t *testing.T) {
	writeConfig(t, `
lease: 45m
exec_grace: 0s
virtual: [browser, db-lock]
groups:
  ui: [browser, web, api]
containers:
  web:
    ready:
      log: 'compiled successfully'
      settle: 2s
guard:
  ports: false
  protect_baseline: false
  mutate_patterns:
    - 'mycli\s+restart\s+{service}'
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lease.D() != 45*time.Minute {
		t.Fatalf("lease = %v", cfg.Lease.D())
	}
	if cfg.ExecGrace.D() != 0 {
		t.Fatalf("explicit zero exec_grace must be kept, got %v", cfg.ExecGrace.D())
	}
	if !cfg.IsVirtual("db-lock") {
		t.Fatal("db-lock should be virtual")
	}
	if cfg.Guard.CheckPorts() {
		t.Fatal("ports: false should disable port checks")
	}
	if cfg.Guard.ProtectsBaseline() {
		t.Fatal("protect_baseline: false should disable baseline protection")
	}
	if got := cfg.ContainerConfig("web").Ready; got.Log != "compiled successfully" || got.Settle.D() != 2*time.Second {
		t.Fatalf("ready config = %+v", got)
	}
	if len(cfg.Guard.UsePatterns) == 0 {
		t.Fatal("use patterns should fall back to defaults")
	}
}

func TestExpandGroups(t *testing.T) {
	cfg := Default()
	cfg.Groups = map[string][]string{"ui": {"browser", "web"}, "all": {"web", "api"}}
	got := cfg.Expand([]string{"ui", "api", "all", "worker"})
	want := []string{"browser", "web", "api", "worker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Expand = %v, want %v", got, want)
	}
}

func TestRejectsNestedGroups(t *testing.T) {
	writeConfig(t, `
groups:
  ui: [browser, web]
  everything: [ui, api]
`)
	if _, err := Load(); err == nil {
		t.Fatal("nested groups should be rejected")
	}
}

func TestRejectsBadDuration(t *testing.T) {
	writeConfig(t, "lease: soon\n")
	if _, err := Load(); err == nil {
		t.Fatal("bad duration should be rejected")
	}
}

func TestIdleAndHoldSettings(t *testing.T) {
	t.Setenv("DIBS_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleRelease.D() != 20*time.Minute || cfg.Hold.D() != 30*time.Minute || cfg.HoldMax.D() != 2*time.Hour {
		t.Fatalf("defaults: idle %s hold %s max %s", cfg.IdleRelease.D(), cfg.Hold.D(), cfg.HoldMax.D())
	}
	writeConfig(t, "idle_release: 0s\nhold: 1h\nhold_max: 4h\n")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleRelease.D() != 0 || cfg.Hold.D() != time.Hour || cfg.HoldMax.D() != 4*time.Hour {
		t.Fatalf("configured: idle %s hold %s max %s", cfg.IdleRelease.D(), cfg.Hold.D(), cfg.HoldMax.D())
	}
}

func TestReposAcceptPathsOrBaselineSettings(t *testing.T) {
	writeConfig(t, `
repos:
  - ~/code/app
  - path: ~/code/other
    baseline:
      remote: upstream
      branch: trunk
      sync: auto
      after_sync: ['make deps']
containers:
  web:
    migrations:
      check: 'check-migrations web'
      paths: [db/migrations]
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if len(cfg.Repos) != 2 || cfg.Repos[0].Path != filepath.Join(home, "code/app") || cfg.Repos[0].Baseline.Sync != SyncManual {
		t.Fatalf("plain path: %+v", cfg.Repos)
	}
	want := Baseline{Remote: "upstream", Branch: "trunk", Sync: SyncAuto, AfterSync: []string{"make deps"}}
	if cfg.Repos[1].Path != filepath.Join(home, "code/other") || !reflect.DeepEqual(cfg.Repos[1].Baseline, want) {
		t.Fatalf("settings: %+v", cfg.Repos[1])
	}
	if !reflect.DeepEqual(cfg.RepoPaths(), []string{filepath.Join(home, "code/app"), filepath.Join(home, "code/other")}) {
		t.Fatalf("paths: %v", cfg.RepoPaths())
	}
	if m := cfg.ContainerConfig("web").Migrations; m.Check != "check-migrations web" || !reflect.DeepEqual(m.Paths, []string{"db/migrations"}) {
		t.Fatalf("migrations: %+v", m)
	}
}

func TestBaselineSettingsAreValidated(t *testing.T) {
	for _, body := range []string{
		"repos: [{baseline: {sync: auto}}]\n",
		"repos: [{path: /x, baseline: {sync: sometimes}}]\n",
		"containers: {web: {migrations: {paths: [db]}}}\n",
	} {
		writeConfig(t, body)
		if _, err := Load(); err == nil {
			t.Fatalf("expected an error for %q", body)
		}
	}
}

func TestRefreshSettings(t *testing.T) {
	writeConfig(t, "refresh:\n  containers: [api, worker]\n  watch: [~/.config/creds.json]\n  check: 'true'\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if !cfg.Refresh.Declares("worker") || cfg.Refresh.Declares("web") || cfg.Refresh.Watch[0] != filepath.Join(home, ".config/creds.json") || cfg.Refresh.Check != "true" {
		t.Fatalf("refresh = %+v", cfg.Refresh)
	}
}
