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
	Log    string   `yaml:"log"`
	HTTP   string   `yaml:"http"`
	TCP    string   `yaml:"tcp"`
	Settle Duration `yaml:"settle"`
}

type Container struct {
	Ready Ready `yaml:"ready"`
}

type Guard struct {
	BrowserTools   []string `yaml:"browser_tools"`
	MutatePatterns []string `yaml:"mutate_patterns"`
	UsePatterns    []string `yaml:"use_patterns"`
	Ports          *bool    `yaml:"ports"`
}

func (g Guard) CheckPorts() bool { return g.Ports == nil || *g.Ports }

type Browser struct {
	Port       int      `yaml:"port"`
	Profile    string   `yaml:"profile"`
	Chrome     string   `yaml:"chrome"`
	MCPCommand []string `yaml:"mcp_command"`
}

type Config struct {
	Lease         Duration             `yaml:"lease"`
	WaitTimeout   Duration             `yaml:"wait_timeout"`
	ReadyTimeout  Duration             `yaml:"ready_timeout"`
	StopTimeout   Duration             `yaml:"stop_timeout"`
	ExecGrace     *Duration            `yaml:"exec_grace"`
	RestoreOnPass bool                 `yaml:"restore_on_pass"`
	Repos         []string             `yaml:"repos"`
	Virtual       []string             `yaml:"virtual"`
	Groups        map[string][]string  `yaml:"groups"`
	Containers    map[string]Container `yaml:"containers"`
	Guard         Guard                `yaml:"guard"`
	Browser       Browser              `yaml:"browser"`
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
	setDuration(&c.WaitTimeout, 30*time.Minute)
	setDuration(&c.ReadyTimeout, 10*time.Minute)
	setDuration(&c.StopTimeout, 10*time.Second)
	if c.ExecGrace == nil {
		grace := Duration(5 * time.Minute)
		c.ExecGrace = &grace
	}
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
	for i, repo := range c.Repos {
		c.Repos[i] = ExpandHome(repo)
	}
}

func setDuration(d *Duration, fallback time.Duration) {
	if *d == 0 {
		*d = Duration(fallback)
	}
}

func (c *Config) validate() error {
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

func ExpandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}
