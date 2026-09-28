package guard

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

type Input struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolUseID     string `json:"tool_use_id"`
	Source        string `json:"source"`
	ToolInput     struct {
		Command         string  `json:"command"`
		Timeout         float64 `json:"timeout"`
		RunInBackground bool    `json:"run_in_background"`
	} `json:"tool_input"`
}

type Decision struct {
	Deny   bool
	Reason string
}

func allow() Decision { return Decision{} }

func deny(format string, args ...any) Decision {
	return Decision{Deny: true, Reason: "dibs: " + fmt.Sprintf(format, args...)}
}

type Alive func(pid int) bool

type view struct {
	cfg   *config.Config
	st    *state.State
	self  string
	now   time.Time
	alive Alive
}

func Decide(cfg *config.Config, st *state.State, caller *tree.Tree, in Input, now time.Time, alive Alive) Decision {
	v := view{cfg: cfg, st: st, now: now, alive: alive}
	if caller != nil {
		v.self = caller.Path
	}
	if v.isBrowserTool(in.ToolName) {
		return v.browser()
	}
	if in.ToolName == "Bash" {
		return v.bash(StripDibs(in.ToolInput.Command), in.CWD)
	}
	return allow()
}

func (v view) isBrowserTool(name string) bool {
	for _, prefix := range v.cfg.Guard.BrowserTools {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (v view) containers() []string {
	names := []string{}
	for _, name := range v.st.Names() {
		if !v.st.Resources[name].Virtual {
			names = append(names, name)
		}
	}
	return names
}

func (v view) otherHolder(r *state.Resource) *state.Holder {
	if h := r.ActiveHolder(v.now); h != nil && h.Tree != v.self {
		return h
	}
	return nil
}

func (v view) mine(r *state.Resource) *state.Holder {
	if h := r.ActiveHolder(v.now); h != nil && h.Tree == v.self && v.self != "" {
		return h
	}
	return nil
}

func (v view) swapping(r *state.Resource) bool {
	return r.Status == state.StatusSwapping && r.Intent != nil && v.alive(r.Intent.PID)
}

func (v view) stalled(r *state.Resource) bool {
	return r.Status == state.StatusSwapping && (r.Intent == nil || !v.alive(r.Intent.PID))
}

func (v view) servingOther(r *state.Resource) bool {
	return r.Serving != "" && r.Serving != v.self
}

func who(h *state.Holder) string {
	return fmt.Sprintf("%s (worktree %s)", h.Label, filepath.Base(h.Tree))
}

func since(h *state.Holder) string {
	text := "since " + h.Since.Local().Format("15:04")
	if h.Pinned() {
		text += ", pinned by a human"
		if h.Note != "" {
			text += ": " + h.Note
		}
	} else if !h.Expires.IsZero() {
		text += ", lease ends " + h.Expires.Local().Format("15:04")
	}
	return text
}

func servingWho(r *state.Resource) string {
	if r.ServingLabel != "" {
		return fmt.Sprintf("%s (worktree %s)", r.ServingLabel, filepath.Base(r.Serving))
	}
	return "worktree " + filepath.Base(r.Serving)
}

func (v view) browser() Decision {
	if v.cfg.IsVirtual(config.BrowserResource) {
		if b := v.st.Lookup(config.BrowserResource); b != nil {
			if h := v.otherHolder(b); h != nil {
				return deny("the browser is in use by %s %s, testing their own worktree. Queue for it by running `dibs take %s <the containers you changed> --wait` in the background, and retry when it returns. Do not work around this.", who(h), since(h), config.BrowserResource)
			}
		}
	}
	for _, name := range v.containers() {
		r := v.st.Resources[name]
		if h := v.otherHolder(r); h != nil {
			return deny("%s is held by %s %s, so the browser would show their code, not yours. Run `dibs take %s %s --wait` in the background and retry when it returns.", name, who(h), since(h), config.BrowserResource, name)
		}
		if mine := v.mine(r); mine != nil {
			if v.swapping(r) {
				return deny("%s is still being switched to your worktree. Wait for `dibs take` to finish, then retry.", name)
			}
			if v.stalled(r) {
				return deny("switching %s to your worktree was interrupted. Run `dibs take %s` again to finish it.", name, name)
			}
			if r.Status == state.StatusFailed {
				return deny("%s failed to start on your worktree. Check `dibs status` and `docker logs %s`, fix it, then run `dibs take %s` again.", name, name, name)
			}
			if mine.NoSwap {
				continue
			}
		}
		if v.servingOther(r) {
			return deny("%s is still running %s's code; nobody holds it. Run `dibs take %s %s --wait` to switch it to your worktree (or `dibs restore %s` for the baseline), then retry.", name, servingWho(r), config.BrowserResource, name, name)
		}
	}
	return allow()
}

var (
	dockerVerb = regexp.MustCompile(`\bdocker\s+(container\s+)?(restart|stop|start|kill|rm|pause|unpause|update|rename)\b([^;&|\n]*)`)
	compose    = regexp.MustCompile(`\bdocker(?:-|\s+)compose\b([^;&|\n]*)`)
	gitMutate  = regexp.MustCompile(`\bgit\s+-C\s+("[^"]+"|'[^']+'|\S+)\s+(checkout|switch|reset|rebase|merge|pull|stash|restore|clean)\b`)
	cdThenGit  = regexp.MustCompile(`\bcd\s+("[^"]+"|'[^']+'|[^\s;&|]+)\s*(?:&&|;)[^|]*?\bgit\s+(checkout|switch|reset|rebase|merge|pull|stash|restore|clean)\b`)
)

func mentions(text, name string) bool {
	if name == "" {
		return false
	}
	re := regexp.MustCompile(`(^|[\s'"=/:,])` + regexp.QuoteMeta(name) + `($|[\s'"/:;&|),])`)
	return re.MatchString(text)
}

func (v view) bash(command, cwd string) Decision {
	if strings.TrimSpace(command) == "" {
		return allow()
	}
	names := v.containers()

	heldByOthers := []string{}
	for _, name := range names {
		r := v.st.Resources[name]
		h := v.otherHolder(r)
		if h == nil {
			continue
		}
		heldByOthers = append(heldByOthers, name)
		if v.mutates(command, name, r) {
			return deny("%s is held by %s %s. Restarting, recreating or re-pointing it would pull it out from under their tests. If you need it, run `dibs take %s --wait` in the background and wait for your turn.", name, who(h), since(h), name)
		}
		for _, pattern := range v.cfg.Guard.MutatePatterns {
			if !templated(pattern) {
				continue
			}
			if matchTemplate(pattern, command, name, r.Service) {
				return deny("%s is held by %s %s, and this command would restart or re-point it. Run `dibs take %s --wait` in the background and wait for your turn.", name, who(h), since(h), name)
			}
		}
	}
	if len(heldByOthers) > 0 {
		for _, pattern := range v.cfg.Guard.MutatePatterns {
			if templated(pattern) {
				continue
			}
			if re, err := regexp.Compile(pattern); err == nil && re.MatchString(command) {
				return deny("this command can restart or re-point shared containers, and %s are held by other worktrees. Wait for them with `dibs take <containers> --wait`, or check `dibs status`.", strings.Join(heldByOthers, ", "))
			}
		}
	}

	notMine := []string{}
	for _, name := range names {
		r := v.st.Resources[name]
		if v.mine(r) != nil {
			continue
		}
		h := v.otherHolder(r)
		if h == nil && !v.servingOther(r) {
			continue
		}
		notMine = append(notMine, name)
		if v.cfg.Guard.CheckPorts() {
			for _, port := range r.Ports {
				re := regexp.MustCompile(fmt.Sprintf(`(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]):%d\b`, port))
				if re.MatchString(command) {
					return deny("port %d is %s, which is serving %s, not your worktree, so any result would describe their code. Run `dibs take %s --wait` first.", port, name, servingWho(r), name)
				}
			}
		}
	}
	if len(notMine) > 0 {
		for _, pattern := range v.cfg.Guard.UsePatterns {
			if re, err := regexp.Compile(pattern); err == nil && re.MatchString(command) {
				return deny("this looks like a test run against the shared stack, but %s serve other worktrees right now. Run `dibs take <what you need> --wait` in the background first, then rerun.", strings.Join(notMine, ", "))
			}
		}
	}

	for _, target := range gitTargets(command, cwd) {
		t, err := tree.Resolve(target)
		if err != nil || t.Path == v.self {
			continue
		}
		for _, name := range names {
			r := v.st.Resources[name]
			if h := v.otherHolder(r); h != nil && r.Serving == t.Path {
				return deny("%s is serving %s from %s for %s. Changing that worktree's branch would change their running code. Work in your own worktree instead.", name, r.ServingLabel, t.Path, who(h))
			}
		}
		baselineFor := []string{}
		for _, name := range names {
			for _, root := range v.st.Resources[name].BaselineRoots {
				if root == t.Path {
					baselineFor = append(baselineFor, name)
				}
			}
		}
		if len(baselineFor) > 0 {
			sort.Strings(baselineFor)
			return deny("%s is the baseline code for %s. Switching its branch changes what those containers run for every session. Make the change in your own worktree; to test it, use `dibs take`.", t.Path, strings.Join(baselineFor, ", "))
		}
	}
	return allow()
}

func (v view) mutates(command, name string, r *state.Resource) bool {
	for _, m := range dockerVerb.FindAllStringSubmatch(command, -1) {
		if mentions(" "+m[3]+" ", name) {
			return true
		}
	}
	for _, m := range compose.FindAllStringSubmatch(command, -1) {
		if composeMutates(strings.Fields(m[1]), name, r) {
			return true
		}
	}
	return false
}

var composeMutating = map[string]bool{"up": true, "down": true, "restart": true, "stop": true, "start": true, "rm": true, "kill": true, "create": true}

var composeGlobalWithValue = map[string]bool{"-f": true, "--file": true, "--profile": true, "--env-file": true, "--project-directory": true, "--ansi": true, "--progress": true, "--parallel": true}

var composeFlagWithValue = map[string]bool{"-t": true, "--timeout": true, "--scale": true, "--pull": true, "--wait-timeout": true, "--exit-code-from": true, "--attach": true, "--no-attach": true, "--rmi": true}

func composeMutates(tokens []string, name string, r *state.Resource) bool {
	projectName := ""
	i := 0
	for i < len(tokens) {
		token := strings.Trim(tokens[i], `"'`)
		if !strings.HasPrefix(token, "-") {
			break
		}
		switch {
		case token == "-p" || token == "--project-name":
			if i+1 < len(tokens) {
				projectName = strings.Trim(tokens[i+1], `"'`)
			}
			i += 2
		case strings.HasPrefix(token, "--project-name="):
			projectName = strings.TrimPrefix(token, "--project-name=")
			i++
		case composeGlobalWithValue[token]:
			i += 2
		default:
			i++
		}
	}
	if i >= len(tokens) || !composeMutating[tokens[i]] {
		return false
	}
	if projectName != "" && r.Project != "" && projectName != r.Project {
		return false
	}
	subcommand := tokens[i]
	services := []string{}
	for j := i + 1; j < len(tokens); j++ {
		token := strings.Trim(tokens[j], `"'`)
		if strings.HasPrefix(token, "-") {
			if composeFlagWithValue[token] {
				j++
			}
			continue
		}
		services = append(services, token)
	}
	if subcommand == "down" || len(services) == 0 {
		return true
	}
	for _, service := range services {
		if service == name || (r.Service != "" && service == r.Service) {
			return true
		}
	}
	return false
}

func templated(pattern string) bool {
	return strings.Contains(pattern, "{service}") || strings.Contains(pattern, "{container}")
}

func matchTemplate(pattern, command, name, service string) bool {
	if service == "" {
		service = name
	}
	expanded := strings.NewReplacer("{container}", regexp.QuoteMeta(name), "{service}", regexp.QuoteMeta(service)).Replace(pattern)
	re, err := regexp.Compile(expanded)
	return err == nil && re.MatchString(command)
}

func gitTargets(command, cwd string) []string {
	targets := []string{}
	add := func(raw string) {
		raw = strings.Trim(raw, `"'`)
		raw = config.ExpandHome(raw)
		if !filepath.IsAbs(raw) && cwd != "" {
			raw = filepath.Join(cwd, raw)
		}
		targets = append(targets, raw)
	}
	for _, m := range gitMutate.FindAllStringSubmatch(command, -1) {
		add(m[1])
	}
	for _, m := range cdThenGit.FindAllStringSubmatch(command, -1) {
		add(m[1])
	}
	return targets
}

var separator = regexp.MustCompile(`&&|\|\||[;|&\n]`)

func StripDibs(command string) string {
	kept := []string{}
	for _, segment := range separator.Split(command, -1) {
		fields := strings.Fields(segment)
		if len(fields) > 0 && (fields[0] == "dibs" || strings.HasSuffix(fields[0], "/dibs")) {
			continue
		}
		kept = append(kept, segment)
	}
	if len(kept) == len(separator.Split(command, -1)) {
		return command
	}
	return strings.Join(kept, " ; ")
}
