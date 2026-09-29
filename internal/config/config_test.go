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
	if cfg.Lease.D() != 30*time.Minute || cfg.StopTimeout.D() != 10*time.Second || cfg.ExecGrace.D() != 5*time.Minute {
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
