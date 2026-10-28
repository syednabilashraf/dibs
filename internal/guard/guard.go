package guard

import (
	"fmt"
	"path/filepath"
	"regexp"
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
		FilePath        string  `json:"file_path"`
		NotebookPath    string  `json:"notebook_path"`
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
	roots map[string][]string
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

func Decide(cfg *config.Config, st *state.State, caller *tree.Tree, in Input, now time.Time, alive Alive) Decision {
	v := view{cfg: cfg, st: st, now: now, alive: alive, roots: BaselineRoots(cfg, st)}
	if caller != nil {
		v.self = caller.Path
	}
	if v.isBrowserTool(in.ToolName) {
		return v.browser()
	}
	if editTools[in.ToolName] {
		return v.edit(in)
	}
	if in.ToolName == "Bash" {
		if d := v.dibsIdentity(in.ToolInput.Command, in.CWD); d.Deny {
			return d
		}
		return v.bash(StripDibs(in.ToolInput.Command), in.CWD)
	}
	return allow()
}

func (v view) dibsIdentity(command, cwd string) Decision {
	for _, target := range dibsTakeTargets(command, cwd) {
		t, err := tree.Resolve(target)
		if err != nil || t.Path == v.self {
			continue
		}
		where := "outside any git worktree"
		if v.self != "" {
			where = "in " + v.self
		}
		return deny("this session runs %s, but this command takes containers for %s. dibs knows a session only by its working directory, so a lease taken for another worktree would belong to that worktree, and your own browser calls, port checks and notes would then be treated as someone else's. Move the session into %s first with the EnterWorktree tool (path: %s), then run `dibs take` there without `cd` or `--tree`. If you cannot move the session, ask the user to start a session in that worktree.", where, t.Path, t.Path, t.Path)
	}
	return allow()
}

func (v view) edit(in Input) Decision {
	if !v.cfg.Guard.ProtectsBaseline() {
		return allow()
	}
	path := in.ToolInput.FilePath
	if path == "" {
		path = in.ToolInput.NotebookPath
	}
	if path == "" {
		return allow()
	}
	if !filepath.IsAbs(path) && in.CWD != "" {
		path = filepath.Join(in.CWD, path)
	}
	owner := owningTree(path)
	if owner == nil {
		return allow()
	}
	if containers, ok := v.roots[owner.Path]; ok {
		return Decision{Deny: true, Reason: baselineSelfMessage(owner.Path, containers)}
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
	switch {
	case h.Pinned():
		text += ", pinned by a human"
		if h.Note != "" {
			text += ": " + h.Note
		}
	case h.OnHold():
		text += ", on hold until " + h.Expires.Local().Format("15:04") + ": " + h.Note
	case !h.Expires.IsZero():
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
	if !v.holdsAnything() {
		return v.stackForNonHolder("the browser would show their code, not yours", config.BrowserResource)
	}
	return v.mineReady("the browser")
}

func (v view) holdsAnything() bool {
	return v.self != "" && len(v.st.HeldBy(v.self, v.now)) > 0
}

func (v view) servesMe(r *state.Resource) bool {
	if h := v.mine(r); h != nil && h.Serve != "" {
		return r.Serving == h.Serve
	}
	if r.Serving == v.self {
		return true
	}
	_, baseline := v.roots[v.self]
	return r.Serving == "" && baseline
}

func (v view) mineReady(what string) Decision {
	var switching, stalled, failed, starting []string
	for _, name := range v.containers() {
		r := v.st.Resources[name]
		mine := v.mine(r)
		if mine == nil || mine.NoSwap {
			continue
		}
		switch {
		case v.stalled(r):
			stalled = append(stalled, name)
		case r.Status == state.StatusFailed:
			failed = append(failed, name)
		case v.swapping(r) || !v.servesMe(r):
			switching = append(switching, name)
		case r.Status == state.StatusStarting:
			starting = append(starting, name)
		}
	}
	switch {
	case len(failed) > 0:
		return deny("%s failed to start on your worktree. Check `docker logs %s` and fix it (often missing dependencies, which you can install inside the container), then run `dibs take %s` again to re-check it without recreating it. Or `dibs pass` to put it back on the baseline.", strings.Join(failed, ", "), failed[0], strings.Join(failed, " "))
	case len(stalled) > 0:
		return deny("switching %s to your worktree was interrupted. Run `dibs take %s` again to finish it.", strings.Join(stalled, ", "), strings.Join(stalled, " "))
	case len(starting) > 0:
		return deny("%s runs your worktree but is not ready yet. Run `dibs take %s` again, which waits for it without recreating it, then retry.", strings.Join(starting, ", "), strings.Join(starting, " "))
	case len(switching) > 0:
		destination := "your worktree"
		if h := v.mine(v.st.Resources[switching[0]]); h != nil && h.Serve != "" {
			destination = "the checkout you asked for with --ref"
		}
		return deny("you hold %s, but your `dibs take` has not finished switching %s to %s, so %s would still show other code there. Wait for the take to finish, then retry.", strings.Join(switching, ", "), pronoun(switching), destination, what)
	}
	return allow()
}

func (v view) stackForNonHolder(consequence string, take string) Decision {
	parts := []string{}
	names := []string{}
	for _, name := range v.containers() {
		r := v.st.Resources[name]
		if h := v.otherHolder(r); h != nil {
			parts = append(parts, fmt.Sprintf("%s is held by %s %s", name, who(h), since(h)))
			names = append(names, name)
		} else if v.servingOther(r) {
			parts = append(parts, fmt.Sprintf("%s still runs %s's code", name, servingWho(r)))
			names = append(names, name)
		}
	}
	if len(parts) == 0 {
		return allow()
	}
	return deny("you hold nothing, so you are testing the shared stack as it is, and %s; %s. Take %s together with the containers your change touches (for example `dibs take %s <containers> --wait` in the background) and retry when it returns. Containers you do not take keep whatever code they run, so take every one your change needs.", strings.Join(parts, "; "), consequence, take, take)
}

func pronoun(names []string) string {
	if len(names) == 1 {
		return "it"
	}
	return "them"
}

var (
	dockerVerb = regexp.MustCompile(`\bdocker\s+(container\s+)?(restart|stop|start|kill|rm|pause|unpause|update|rename)\b([^;&|\n]*)`)
	compose    = regexp.MustCompile(`\bdocker(?:-|\s+)compose\b([^;&|\n]*)`)
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

	for _, name := range names {
		r := v.st.Resources[name]
		if v.mine(r) != nil || !v.cfg.Guard.CheckPorts() {
			continue
		}
		if v.otherHolder(r) == nil && !v.servingOther(r) {
			continue
		}
		for _, port := range r.Ports {
			re := regexp.MustCompile(fmt.Sprintf(`(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]):%d\b`, port))
			if re.MatchString(command) {
				return deny("port %d is %s, which is serving %s, not your worktree, so any result would describe their code. Run `dibs take %s --wait` first.", port, name, servingWho(r), name)
			}
		}
	}
	for _, pattern := range v.cfg.Guard.UsePatterns {
		if re, err := regexp.Compile(pattern); err == nil && re.MatchString(command) {
			if !v.holdsAnything() {
				if d := v.stackForNonHolder("a test run would exercise their code, not yours", "<containers>"); d.Deny {
					return d
				}
			} else if d := v.mineReady("a test run"); d.Deny {
				return d
			}
			break
		}
	}

	for _, target := range gitTargets(command, cwd) {
		t, err := tree.Resolve(target)
		if err != nil {
			continue
		}
		containers, isBaseline := v.roots[t.Path]
		if t.Path == v.self {
			if isBaseline && v.cfg.Guard.ProtectsBaseline() {
				return Decision{Deny: true, Reason: baselineSelfMessage(t.Path, containers)}
			}
			continue
		}
		for _, name := range names {
			r := v.st.Resources[name]
			if h := v.otherHolder(r); h != nil && r.Serving == t.Path {
				return deny("%s is serving %s from %s for %s. Changing that worktree's branch would change their running code. Work in your own worktree instead.", name, r.ServingLabel, t.Path, who(h))
			}
		}
		if isBaseline {
			return deny("%s is the baseline code for %s. Switching its branch changes what those containers run for every session. Make the change in your own worktree; to test it, use `dibs take`.", t.Path, baselineWhat(containers))
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

var gitMutating = map[string]bool{
	"checkout": true, "switch": true, "reset": true, "rebase": true, "merge": true, "pull": true,
	"stash": true, "restore": true, "clean": true, "cherry-pick": true, "revert": true, "am": true,
}

func segmentFields(segment string) []string {
	fields := strings.Fields(strings.TrimRight(strings.TrimLeft(strings.TrimSpace(segment), "("), ")"))
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
		fields = fields[1:]
	}
	return fields
}

func isDibs(word string) bool {
	return word == "dibs" || strings.HasSuffix(word, "/dibs")
}

func dibsTakeTargets(command, cwd string) []string {
	dir := cwd
	targets := []string{}
	for _, segment := range commandSegments(command) {
		fields := segmentFields(segment)
		if len(fields) == 0 {
			continue
		}
		if (fields[0] == "cd" || fields[0] == "pushd") && len(fields) > 1 {
			dir = resolveDir(dir, fields[1])
			continue
		}
		if !isDibs(fields[0]) || len(fields) < 2 || (fields[1] != "take" && fields[1] != "grab") {
			continue
		}
		target := dir
		for i := 2; i < len(fields); i++ {
			switch {
			case fields[i] == "--tree" || fields[i] == "-tree":
				if i+1 < len(fields) {
					target = resolveDir(dir, fields[i+1])
					i++
				}
			case strings.HasPrefix(fields[i], "--tree="):
				target = resolveDir(dir, strings.TrimPrefix(fields[i], "--tree="))
			case strings.HasPrefix(fields[i], "-tree="):
				target = resolveDir(dir, strings.TrimPrefix(fields[i], "-tree="))
			}
		}
		if target != "" {
			targets = append(targets, target)
		}
	}
	return targets
}

func gitTargets(command, cwd string) []string {
	dir := cwd
	targets := []string{}
	for _, segment := range commandSegments(command) {
		fields := segmentFields(segment)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "cd", "pushd":
			if len(fields) > 1 {
				dir = resolveDir(dir, fields[1])
			}
		case "git":
			target := dir
			i := 1
			for i < len(fields) && strings.HasPrefix(fields[i], "-") {
				switch fields[i] {
				case "-C":
					if i+1 < len(fields) {
						target = resolveDir(dir, fields[i+1])
					}
					i += 2
				case "-c":
					i += 2
				default:
					i++
				}
			}
			if i >= len(fields) || !gitMutating[fields[i]] {
				continue
			}
			if fields[i] == "stash" && i+1 < len(fields) && (fields[i+1] == "list" || fields[i+1] == "show") {
				continue
			}
			if target != "" {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

func resolveDir(base, raw string) string {
	raw = config.ExpandHome(strings.Trim(raw, `"'`))
	if !filepath.IsAbs(raw) && base != "" {
		raw = filepath.Join(base, raw)
	}
	return raw
}

var heredocStart = regexp.MustCompile(`<<-?[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)

func stripHeredocs(command string) string {
	lines := strings.Split(command, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		loc := heredocStart.FindStringSubmatchIndex(lines[i])
		if loc == nil || (loc[0] > 0 && lines[i][loc[0]-1] == '<') {
			continue
		}
		delimiter := lines[i][loc[4]:loc[5]]
		for i+1 < len(lines) {
			i++
			if strings.TrimLeft(lines[i], "\t") == delimiter {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

func splitSegments(command string) []string {
	segments := []string{}
	var current strings.Builder
	inSingle, inDouble := false, false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(runes):
			current.WriteRune(c)
			i++
			current.WriteRune(runes[i])
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && !inDouble && (c == ';' || c == '\n' || c == '|' || c == '&'):
			segments = append(segments, current.String())
			current.Reset()
			if (c == '&' || c == '|') && i+1 < len(runes) && runes[i+1] == c {
				i++
			}
			continue
		}
		current.WriteRune(c)
	}
	return append(segments, current.String())
}

func commandSegments(command string) []string {
	return splitSegments(stripHeredocs(command))
}

func StripDibs(command string) string {
	command = stripHeredocs(command)
	segments := splitSegments(command)
	kept := []string{}
	for _, segment := range segments {
		fields := strings.Fields(segment)
		if len(fields) > 0 && isDibs(fields[0]) {
			continue
		}
		kept = append(kept, segment)
	}
	if len(kept) == len(segments) {
		return command
	}
	return strings.Join(kept, " ; ")
}
