package guard

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

func Touches(st *state.State, command string) []string {
	command = StripDibs(command)
	seen := map[string]bool{}
	touched := []string{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			touched = append(touched, name)
		}
	}
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Virtual {
			continue
		}
		if mentions(command, name) || (r.Service != "" && r.Service != name && mentions(command, r.Service)) {
			add(name)
		}
	}
	for _, name := range ExecTargets(command) {
		if r := st.Lookup(name); r == nil || !r.Virtual {
			add(name)
		}
	}
	sort.Strings(touched)
	return touched
}

var (
	dockerExec = regexp.MustCompile(`\bdocker\s+(?:container\s+)?exec\b([^;&|\n]*)`)
	dockerCp   = regexp.MustCompile(`\bdocker\s+(?:container\s+)?cp\b([^;&|\n]*)`)
	execValue  = map[string]bool{"-e": true, "--env": true, "-w": true, "--workdir": true, "-u": true, "--user": true, "--env-file": true, "--detach-keys": true}
)

func ExecTargets(command string) []string {
	targets := []string{}
	for _, m := range dockerExec.FindAllStringSubmatch(command, -1) {
		fields := strings.Fields(m[1])
		for i := 0; i < len(fields); i++ {
			field := strings.Trim(fields[i], `"'`)
			if strings.HasPrefix(field, "-") {
				if execValue[field] {
					i++
				}
				continue
			}
			targets = append(targets, field)
			break
		}
	}
	for _, m := range dockerCp.FindAllStringSubmatch(command, -1) {
		for _, field := range strings.Fields(m[1]) {
			field = strings.Trim(field, `"'`)
			if strings.HasPrefix(field, "-") || strings.HasPrefix(field, "/") || strings.HasPrefix(field, ".") {
				continue
			}
			if i := strings.Index(field, ":"); i > 0 {
				targets = append(targets, field[:i])
			}
		}
	}
	return targets
}

func Relevant(cfg *config.Config, st *state.State, caller *tree.Tree) bool {
	if caller == nil {
		return false
	}
	for _, name := range st.Names() {
		if r := st.Resources[name]; r.Repo != "" && r.Repo == caller.Common {
			return true
		}
	}
	for _, repo := range cfg.RepoPaths() {
		if t, err := tree.Resolve(repo); err == nil && t.Common == caller.Common {
			return true
		}
	}
	return false
}

func Primer(cfg *config.Config, st *state.State, caller *tree.Tree, tmpdir string, now time.Time, subagent bool) string {
	var b strings.Builder
	b.WriteString("Shared Docker containers are coordinated by dibs.\n\n")
	b.WriteString("This repository's local containers are shared with other agent sessions working in other git worktrees. dibs decides which worktree each container runs at any moment.\n\n")
	if caller != nil {
		if containers, ok := BaselineRoots(cfg, st)[caller.Path]; ok {
			b.WriteString(BaselineWarning(caller.Path, containers, cfg.Guard.ProtectsBaseline()) + "\n\n")
		}
	}
	if subagent {
		b.WriteString("- You are a subagent. Leases belong to the worktree, so you share them with the session that started you when you work in the same worktree. If it already holds what you need (see below), use it without taking again. Never run `dibs pass` for anything you did not take yourself.\n")
	}
	b.WriteString("- To test your changes against a running container (browser QA, e2e, requests to its port), first run `dibs take <containers or group> --wait` as a background command and wait for it to finish. It recreates those containers with your worktree mounted. Include `browser` if you will drive a browser. To stop a take, run `dibs cancel` rather than killing it.")
	if groups := groupSummary(cfg); groups != "" {
		b.WriteString(" Groups: " + groups + ".")
	}
	b.WriteString("\n")
	b.WriteString("- dibs knows this session only by its working directory, not by `cd` inside a command. Take, test, check and pass from the worktree the session runs in. To work in a different worktree, move the session there with the EnterWorktree tool (`path`); `dibs take` for another worktree is refused. One worktree per session is enough: switch branches inside it for follow-up work once the previous branch is pushed.\n")
	b.WriteString("- After each batch of tests run `dibs check`. Exit code 2 means a container stopped serving your worktree: discard those results and take again.\n")
	b.WriteString("- Run `dibs pass` as soon as you finish testing, not at the end of the session. Others may be waiting.\n")
	if idle := cfg.IdleRelease.D(); idle > 0 {
		b.WriteString(fmt.Sprintf("- While someone waits for what you hold, %s without a tool call loses you those leases; renewing does not count. To keep them on purpose (the user wants to test, or you stop to wait for the user), run `dibs hold --for <time> --note '<why>'`, which is bounded and tells the others why, and tell the user. Never renew in a loop.\n", fmtMinutes(idle)))
	}
	b.WriteString("- Never restart, recreate, rebuild or re-point shared containers yourself, and never switch branches in another worktree. The dibs hook blocks these; do not work around it.\n")
	b.WriteString("- Unit tests need no lease: copy your code into the container under `" + tmpdir + "` (unique to this worktree, so it cannot collide with other sessions) and run them there with `docker exec`. Another session's `dibs take` can still recreate that container, which kills commands running in it and deletes everything copied into it. If that happens you will be told; copy again and rerun.\n")
	b.WriteString("- Databases and other stateful services are shared by every worktree. A migration or data you apply is seen by all sessions and can break containers built from other checkouts, so ask the user before applying a schema migration to a shared database.\n")
	b.WriteString("- `dibs status` shows who holds what. The dibs skill has the full protocol.\n")
	b.WriteString("\nRight now:\n")
	b.WriteString(Snapshot(st, caller, now))
	return b.String()
}

func BaselineWarning(root string, containers []string, enforced bool) string {
	subject := "The shared containers"
	if len(containers) > 0 {
		subject = "The containers " + strings.Join(containers, ", ")
	}
	text := fmt.Sprintf("IMPORTANT: this session is running in %s, the baseline checkout. %s run this code for every session that does not hold a lease, so editing files or switching branches here changes what everyone is testing. Do not change anything here. %s", root, subject, worktreeAdvice())
	if enforced {
		text += " dibs blocks edits and branch changes in this checkout."
	}
	return text
}

func groupSummary(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Groups))
	for name := range cfg.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := []string{}
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s = %s", name, strings.Join(cfg.Groups[name], ", ")))
	}
	return strings.Join(parts, "; ")
}

func Snapshot(st *state.State, caller *tree.Tree, now time.Time) string {
	self := ""
	if caller != nil {
		self = caller.Path
	}
	lines := []string{}
	for _, name := range st.Names() {
		r := st.Resources[name]
		line := "- " + name + ": "
		if h := r.ActiveHolder(now); h != nil {
			if h.Tree == self {
				line += "held by you"
			} else {
				line += "held by " + who(h) + " " + since(h)
			}
		} else {
			line += "free"
		}
		if !r.Virtual {
			switch {
			case r.Serving == "":
				line += ", running the baseline"
			case r.Serving == self:
				line += ", running your worktree"
			default:
				line += ", running " + servingWho(r)
			}
		}
		lines = append(lines, line)
	}
	for i, w := range st.Queue {
		lines = append(lines, fmt.Sprintf("- waiting #%d: %s (worktree %s) for %s", i+1, w.Label, filepath.Base(w.Tree), strings.Join(w.Resources, ", ")))
	}
	if len(lines) == 0 {
		return "- nothing is held; all containers run the baseline\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func SwapNotes(events []state.Event, touched []string, windows map[string]time.Time, self string) []string {
	notes := []string{}
	for _, name := range touched {
		start, ok := windows[name]
		if !ok {
			continue
		}
		var last *state.Event
		for i := range events {
			ev := events[i]
			if ev.Resource == name && (ev.Kind == "swap" || ev.Kind == "restore") && ev.Time.After(start) {
				last = &events[i]
			}
		}
		if last == nil || (self != "" && last.Actor == self) {
			continue
		}
		serving := "the baseline"
		if last.Kind == "swap" && last.Tree != "" {
			serving = fmt.Sprintf("%s (worktree %s)", last.Label, filepath.Base(last.Tree))
		}
		notes = append(notes, fmt.Sprintf("dibs: %s was recreated at %s by another session and now runs %s. Any command that was running inside it was killed, and anything you copied into it (for example under /tmp) is gone. If that affected what you just ran, copy your code again and rerun; do not treat the failure as a bug in your change.", name, last.Time.Local().Format("15:04:05"), serving))
	}
	return notes
}

type LeaseNote struct {
	Resource string
	Expires  time.Time
}

func ExpiringLeases(st *state.State, self string, now time.Time, within time.Duration) []LeaseNote {
	notes := []LeaseNote{}
	if self == "" {
		return notes
	}
	for _, name := range st.HeldBy(self, now) {
		h := st.Resources[name].Holder
		if h.Kind == state.KindSession && h.Expires.Sub(now) <= within {
			notes = append(notes, LeaseNote{Resource: name, Expires: h.Expires})
		}
	}
	return notes
}

func LeaseText(notes []LeaseNote, now time.Time) string {
	names := []string{}
	for _, n := range notes {
		names = append(names, n.Resource)
	}
	return fmt.Sprintf("dibs: your lease on %s ends at %s (in %s). If you are still testing, run `dibs renew`; otherwise run `dibs pass`. When it lapses another session may take the containers.", strings.Join(names, ", "), notes[0].Expires.Local().Format("15:04"), notes[0].Expires.Sub(now).Round(time.Minute))
}

type HeldNote struct {
	Resource string
	Holder   state.Holder
}

func HeldByOthers(st *state.State, touched []string, self string, now time.Time) []HeldNote {
	notes := []HeldNote{}
	for _, name := range touched {
		if h := st.Lookup(name).ActiveHolder(now); h != nil && h.Tree != self {
			notes = append(notes, HeldNote{Resource: name, Holder: *h})
		}
	}
	return notes
}

func HeldText(n HeldNote) string {
	return fmt.Sprintf("dibs: %s is held by %s %s and is running their worktree, not yours. Running your own unit tests inside it from a copy is fine, but results from its running app describe their code. It can be recreated when the lease changes hands, which kills commands running in it and deletes files copied into it.", n.Resource, who(&n.Holder), since(&n.Holder))
}

type Waiting struct {
	Waiter    state.Waiter
	Resources []string
}

func WaitingOn(st *state.State, self string, now time.Time) []Waiting {
	if self == "" {
		return nil
	}
	held := st.HeldBy(self, now)
	for _, name := range held {
		if r := st.Resources[name]; r.Status == state.StatusSwapping && r.Intent != nil {
			return nil
		}
	}
	byWaiter := map[string]*Waiting{}
	order := []string{}
	for _, name := range held {
		for _, w := range queue.WantedBy(st, name, self) {
			entry, ok := byWaiter[w.ID]
			if !ok {
				entry = &Waiting{Waiter: w}
				byWaiter[w.ID] = entry
				order = append(order, w.ID)
			}
			entry.Resources = append(entry.Resources, name)
		}
	}
	waiting := make([]Waiting, 0, len(order))
	for _, id := range order {
		waiting = append(waiting, *byWaiter[id])
	}
	sort.SliceStable(waiting, func(i, j int) bool { return waiting[i].Waiter.Since.Before(waiting[j].Waiter.Since) })
	return waiting
}

func WaitingText(waiting []Waiting, hold *state.Holder, idleLimit time.Duration, now time.Time) string {
	parts := make([]string, 0, len(waiting))
	for _, w := range waiting {
		parts = append(parts, fmt.Sprintf("%s (worktree %s) for %s, waiting %s", w.Waiter.Label, filepath.Base(w.Waiter.Tree), strings.Join(w.Resources, ", "), roundedSince(w.Waiter.Since, now)))
	}
	sessions := "1 session is"
	if len(waiting) != 1 {
		sessions = fmt.Sprintf("%d sessions are", len(waiting))
	}
	text := fmt.Sprintf("dibs: %s waiting for what this worktree holds: %s.", sessions, strings.Join(parts, "; "))
	if hold != nil && hold.OnHold() {
		return text + fmt.Sprintf(" You put it on hold until %s (%s); run `dibs pass` as soon as that is over.", hold.Expires.Local().Format("15:04"), hold.Note)
	}
	text += " Pass what you are not testing with right now (`dibs pass`, or `dibs pass <names>` for part of it). If the user wants to test it, or you are stopping to wait for the user, run `dibs hold --for <time> --note '<why>'` so the others see why."
	if idleLimit > 0 {
		text += fmt.Sprintf(" A session that makes no tool calls for %s while others wait loses its leases.", fmtMinutes(idleLimit))
	}
	return text
}

type Lapse struct {
	Resource string
	Why      string
	Now      *state.Holder
}

func Lapses(st *state.State, events []state.Event, lost []string, self string, now time.Time) []Lapse {
	lapses := []Lapse{}
	for _, name := range lost {
		r := st.Lookup(name)
		lapse := Lapse{Resource: name, Why: "your lease on it ended"}
		passed := false
		if r != nil && r.Holder != nil && r.Holder.Tree == self && r.Holder.Expired(now) {
			lapse.Why = "your lease on it expired at " + r.Holder.Expires.Local().Format("15:04")
		} else {
			for i := len(events) - 1; i >= 0; i-- {
				if !containsName(events[i].Resource, name) {
					continue
				}
				why, byItself := lapseReason(events[i], self)
				if byItself {
					passed = true
					break
				}
				if why != "" {
					lapse.Why = why
					break
				}
			}
		}
		if passed {
			continue
		}
		if h := r.ActiveHolder(now); h != nil && h.Tree != self {
			lapse.Now = h
		}
		lapses = append(lapses, lapse)
	}
	return lapses
}

func lapseReason(ev state.Event, self string) (string, bool) {
	at := ev.Time.Local().Format("15:04")
	switch ev.Kind {
	case "pass":
		if ev.Tree == self {
			return "", true
		}
	case "expire":
		if ev.Tree == self {
			return "your lease on it expired (noticed at " + at + ")", false
		}
	case "idle":
		if ev.Tree == self {
			return fmt.Sprintf("dibs released it at %s because this worktree made %s", at, ev.Detail), false
		}
	case "grab":
		if ev.Tree != self {
			return fmt.Sprintf("a human pinned it for %s at %s", ev.Label, at), false
		}
	}
	return "", false
}

func LapseText(lapses []Lapse) string {
	parts := make([]string, 0, len(lapses))
	names := make([]string, 0, len(lapses))
	for _, l := range lapses {
		part := fmt.Sprintf("%s: %s", l.Resource, l.Why)
		if l.Now != nil {
			part += fmt.Sprintf(", and %s has held it %s", who(l.Now), since(l.Now))
		}
		parts = append(parts, part)
		names = append(names, l.Resource)
	}
	return fmt.Sprintf("dibs: this worktree no longer holds %s. %s. Results collected from them since then may not come from your code: discard them. If you still need them, run `dibs take %s --wait` in the background and wait for it.", strings.Join(names, ", "), strings.Join(parts, "; "), strings.Join(names, " "))
}

func containsName(list, name string) bool {
	for _, item := range strings.Split(list, ",") {
		if item == name {
			return true
		}
	}
	return false
}

func roundedSince(t, now time.Time) string {
	return fmtMinutes(now.Sub(t))
}

func fmtMinutes(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
