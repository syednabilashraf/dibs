package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const BrowserResource = "browser"

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Ready struct {
	Log         string   `yaml:"log"`
	Fail        string   `yaml:"fail"`
	HTTP        string   `yaml:"http"`
	TCP         string   `yaml:"tcp"`
	Settle      Duration `yaml:"settle"`
	Timeout     Duration `yaml:"timeout"`
	MaxRestarts int      `yaml:"max_restarts"`
}

type Migrations struct {
	Check string   `yaml:"check"`
	Paths []string `yaml:"paths"`
}

type Container struct {
	Ready      Ready      `yaml:"ready"`
	Migrations Migrations `yaml:"migrations"`
}

const (
	SyncManual = "manual"
	SyncAuto   = "auto"
)

type Baseline struct {
	Remote    string   `yaml:"remote"`
	Branch    string   `yaml:"branch"`
	Sync      string   `yaml:"sync"`
	AfterSync []string `yaml:"after_sync"`
}

type Repo struct {
	Path     string   `yaml:"path"`
	Baseline Baseline `yaml:"baseline"`
}

func (r *Repo) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&r.Path)
	}
	type plain Repo
	return node.Decode((*plain)(r))
}

type Guard struct {
	BrowserTools    []string `yaml:"browser_tools"`
	MutatePatterns  []string `yaml:"mutate_patterns"`
	UsePatterns     []string `yaml:"use_patterns"`
	Ports           *bool    `yaml:"ports"`
	ProtectBaseline *bool    `yaml:"protect_baseline"`
}

func (g Guard) CheckPorts() bool { return g.Ports == nil || *g.Ports }

func (g Guard) ProtectsBaseline() bool { return g.ProtectBaseline == nil || *g.ProtectBaseline }

type Browser struct {
	Port       int      `yaml:"port"`
	Profile    string   `yaml:"profile"`
	Chrome     string   `yaml:"chrome"`
	MCPCommand []string `yaml:"mcp_command"`
	Tools      []string `yaml:"tools"`
	MaxTabs    *int     `yaml:"max_tabs"`
}

func (b Browser) Drives(tool string) bool {
	for _, prefix := range b.Tools {
		if prefix != "" && strings.HasPrefix(tool, prefix) {
			return true
		}
	}
	return false
}

func (b Browser) TabLimit() int {
	if b.MaxTabs == nil {
		return 6
	}
	return *b.MaxTabs
}

type Refresh struct {
	Containers []string `yaml:"containers"`
	Watch      []string `yaml:"watch"`
	Check      string   `yaml:"check"`
}

func (r Refresh) Declares(name string) bool {
	for _, declared := range r.Containers {
		if declared == name {
			return true
		}
	}
	return false
}

type Test struct {
	MaxParallel int    `yaml:"max_parallel"`
	Memory      string `yaml:"memory"`
}

type Config struct {
	Lease         Duration             `yaml:"lease"`
	WaitTimeout   Duration             `yaml:"wait_timeout"`
	ReadyTimeout  Duration             `yaml:"ready_timeout"`
	StopTimeout   Duration             `yaml:"stop_timeout"`
	ExecGrace     *Duration            `yaml:"exec_grace"`
	IdleRelease   *Duration            `yaml:"idle_release"`
	Hold          Duration             `yaml:"hold"`
	HoldMax       Duration             `yaml:"hold_max"`
	RestoreOnPass bool                 `yaml:"restore_on_pass"`
	Repos         []Repo               `yaml:"repos"`
	Virtual       []string             `yaml:"virtual"`
	Groups        map[string][]string  `yaml:"groups"`
	Containers    map[string]Container `yaml:"containers"`
	Guard         Guard                `yaml:"guard"`
	Browser       Browser              `yaml:"browser"`
	Refresh       Refresh              `yaml:"refresh"`
	Test          Test                 `yaml:"test"`
}

var DefaultUsePatterns = []string{
	`\b(npx|pnpm|yarn|npm|bunx)\s+(exec\s+|run\s+)?playwright\s+test\b`,
	`\bplaywright\s+test\b`,
	`\bcypress\s+(run|open)\b`,
	`\b(npm|pnpm|yarn)\s+(run\s+)?(test[:-])?e2e\b`,
}

func Home() string {
	if dir := os.Getenv("DIBS_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dibs"
	}
	return filepath.Join(home, ".dibs")
}

func Path() string {
	if path := os.Getenv("DIBS_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(Home(), "config.yaml")
}

func Load() (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", Path(), err)
		}
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	return cfg, nil
}

func Default() *Config {
	cfg := &Config{}
	cfg.applyDefaults()
	return cfg
}

func (c *Config) applyDefaults() {
	setDuration(&c.Lease, 30*time.Minute)
	setDuration(&c.ReadyTimeout, 10*time.Minute)
	setDuration(&c.StopTimeout, 10*time.Second)
	if c.ExecGrace == nil {
		grace := Duration(5 * time.Minute)
		c.ExecGrace = &grace
	}
	if c.IdleRelease == nil {
		idle := Duration(20 * time.Minute)
		c.IdleRelease = &idle
	}
	setDuration(&c.Hold, 30*time.Minute)
	setDuration(&c.HoldMax, 2*time.Hour)
	if c.Virtual == nil {
		c.Virtual = []string{BrowserResource}
	}
	if c.Groups == nil {
		c.Groups = map[string][]string{}
	}
	if c.Containers == nil {
		c.Containers = map[string]Container{}
	}
	if c.Guard.BrowserTools == nil {
		c.Guard.BrowserTools = []string{"mcp__chrome-devtools__", "mcp__playwright__"}
	}
	if c.Guard.UsePatterns == nil {
		c.Guard.UsePatterns = DefaultUsePatterns
	}
	if c.Test.MaxParallel == 0 {
		c.Test.MaxParallel = 2
	}
	if c.Browser.Port == 0 {
		c.Browser.Port = 9222
	}
	if c.Browser.Profile == "" {
		c.Browser.Profile = "~/.cache/dibs/chrome-profile"
	}
	c.Browser.Profile = ExpandHome(c.Browser.Profile)
	if len(c.Browser.MCPCommand) == 0 {
		c.Browser.MCPCommand = []string{"npx", "-y", "chrome-devtools-mcp@latest"}
	}
	if c.Browser.Tools == nil {
		c.Browser.Tools = []string{"mcp__chrome-devtools__"}
	}
	for i, path := range c.Refresh.Watch {
		c.Refresh.Watch[i] = ExpandHome(path)
	}
	for i := range c.Repos {
		c.Repos[i].Path = ExpandHome(c.Repos[i].Path)
		if c.Repos[i].Baseline.Sync == "" {
			c.Repos[i].Baseline.Sync = SyncManual
		}
	}
}

func setDuration(d *Duration, fallback time.Duration) {
	if *d == 0 {
		*d = Duration(fallback)
	}
}

func (c *Config) validate() error {
	for _, repo := range c.Repos {
		if repo.Path == "" {
			return fmt.Errorf("every entry under repos needs a path")
		}
		if repo.Baseline.Sync != SyncManual && repo.Baseline.Sync != SyncAuto {
			return fmt.Errorf("repo %s: baseline.sync must be %q or %q", repo.Path, SyncManual, SyncAuto)
		}
	}
	for name, container := range c.Containers {
		if container.Migrations.Check == "" && len(container.Migrations.Paths) > 0 {
			return fmt.Errorf("containers.%s.migrations lists paths but no check", name)
		}
	}
	for name, members := range c.Groups {
		if c.IsVirtual(name) {
			return fmt.Errorf("group %q has the same name as a virtual resource", name)
		}
		for _, member := range members {
			if _, nested := c.Groups[member]; nested {
				return fmt.Errorf("group %q contains group %q; groups cannot nest", name, member)
			}
		}
	}
	return nil
}

func (c *Config) IsVirtual(name string) bool {
	for _, v := range c.Virtual {
		if v == name {
			return true
		}
	}
	return false
}

func (c *Config) Expand(names []string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, name := range names {
		if members, ok := c.Groups[name]; ok {
			for _, member := range members {
				add(member)
			}
			continue
		}
		add(name)
	}
	return out
}

func (c *Config) ContainerConfig(name string) Container {
	return c.Containers[name]
}

func (c *Config) RepoPaths() []string {
	paths := make([]string, 0, len(c.Repos))
	for _, repo := range c.Repos {
		paths = append(paths, repo.Path)
	}
	return paths
}

func ExpandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}
