package guard

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

func Touches(st *state.State, command string) []string {
	command = StripDibs(command)
	touched := []string{}
	for _, name := range st.Names() {
		r := st.Resources[name]
		if r.Virtual {
			continue
		}
		if mentions(command, name) || (r.Service != "" && r.Service != name && mentions(command, r.Service)) {
			touched = append(touched, name)
		}
	}
	return touched
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
	for _, repo := range cfg.Repos {
		if t, err := tree.Resolve(repo); err == nil && t.Common == caller.Common {
			return true
		}
	}
	return false
}

func Primer(cfg *config.Config, st *state.State, caller *tree.Tree, tmpdir string, now time.Time) string {
	var b strings.Builder
	b.WriteString("Shared Docker containers are coordinated by dibs.\n\n")
	b.WriteString("This repository's local containers are shared with other agent sessions working in other git worktrees. dibs decides which worktree each container runs at any moment.\n\n")
	b.WriteString("- To test your changes against a running container (browser QA, e2e, requests to its port), first run `dibs take <containers or group> --wait` as a background command and wait for it to finish. It recreates those containers with your worktree mounted. Include `browser` if you will drive a browser.")
	if groups := groupSummary(cfg); groups != "" {
		b.WriteString(" Groups: " + groups + ".")
	}
	b.WriteString("\n")
	b.WriteString("- After each batch of tests run `dibs check`. Exit code 2 means a container stopped serving your worktree: discard those results and take again.\n")
	b.WriteString("- Run `dibs pass` as soon as you finish testing, not at the end of the session. Others may be waiting.\n")
	b.WriteString("- Never restart, recreate, rebuild or re-point shared containers yourself, and never switch branches in another worktree. The dibs hook blocks these; do not work around it.\n")
	b.WriteString("- Unit tests need no lease: copy your code into the container under `" + tmpdir + "` (unique to this worktree, so it cannot collide with other sessions) and run them there with `docker exec`. Another session's `dibs take` can still recreate that container, which kills commands running in it and deletes everything copied into it. If that happens you will be told; copy again and rerun.\n")
	b.WriteString("- `dibs status` shows who holds what. The dibs skill has the full protocol.\n")
	b.WriteString("\nRight now:\n")
	b.WriteString(Snapshot(st, caller, now))
	return b.String()
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
