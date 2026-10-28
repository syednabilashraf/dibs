package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/browser"
	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/guard"
	"github.com/syednabilashraf/dibs/internal/queue"
	"github.com/syednabilashraf/dibs/internal/state"
	"github.com/syednabilashraf/dibs/internal/tree"
)

type hookContext struct {
	in     guard.Input
	cfg    *config.Config
	store  *state.Store
	st     *state.State
	caller *tree.Tree
}

func (h *hookContext) self() string {
	if h.caller == nil {
		return ""
	}
	return h.caller.Path
}

func loadHook(stdin io.Reader) (*hookContext, bool) {
	data, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return nil, false
	}
	h := &hookContext{}
	if json.Unmarshal(data, &h.in) != nil {
		return nil, false
	}
	if h.cfg, err = config.Load(); err != nil {
		return nil, false
	}
	if h.store, err = state.Open(config.Home()); err != nil {
		return nil, false
	}
	if h.st, err = h.store.Read(); err != nil {
		return nil, false
	}
	h.caller, _ = tree.Resolve(h.in.CWD)
	return h, true
}

func emitDeny(w io.Writer, reason string) {
	emit(w, map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	})
}

func emitContext(w io.Writer, event, text string) {
	emit(w, map[string]any{"hookEventName": event, "additionalContext": text})
}

func emit(w io.Writer, specific map[string]any) {
	data, _ := json.Marshal(map[string]any{"hookSpecificOutput": specific})
	w.Write(append(data, '\n'))
}

func runGuard(e *env, args []string) int {
	post := len(args) > 0 && args[0] == "--post"
	h, ok := loadHook(e.stdin)
	if !ok {
		return exitOK
	}
	if post {
		h.post(e.stdout)
		return exitOK
	}
	h.touch(time.Now())
	decision := guard.Decide(h.cfg, h.st, h.caller, h.in, time.Now(), state.ProcessAlive)
	if decision.Deny {
		emitDeny(e.stdout, decision.Reason)
		return exitOK
	}
	if h.cfg.Browser.Drives(h.in.ToolName) {
		if reason := h.prepareBrowser(); reason != "" {
			emitDeny(e.stdout, reason)
		}
		return exitOK
	}
	if h.in.ToolName == "Bash" && h.in.ToolUseID != "" {
		if len(guard.Touches(h.st, h.in.ToolInput.Command)) > 0 {
			h.recordInflight()
		}
	}
	return exitOK
}

func (h *hookContext) prepareBrowser() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := browserOptions(h.cfg)
	if !browser.Managed(opts) || browser.Ensure(ctx, opts) != nil || !strings.HasSuffix(h.in.ToolName, "__new_page") {
		return ""
	}
	limit := h.cfg.Browser.TabLimit()
	pages, err := browser.Pages(ctx, opts.Port)
	if err != nil || limit <= 0 || len(pages) < limit {
		return ""
	}
	return fmt.Sprintf("dibs: %d tabs are already open in the shared browser, the most allowed (browser.max_tabs in the dibs config). Close the tabs you no longer need with close_page (list_pages shows them), then open a new one. Every session uses this one browser, so close only tabs you opened.", len(pages))
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func (h *hookContext) inflightPath() string {
	return filepath.Join(h.store.Dir, "inflight", unsafeFileChars.ReplaceAllString(h.in.ToolUseID, "_")+".json")
}

type inflight struct {
	Started time.Time `json:"started"`
}

func (h *hookContext) recordInflight() {
	path := h.inflightPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(inflight{Started: time.Now()})
	os.WriteFile(path, data, 0o644)
	sweep(filepath.Dir(path), 24*time.Hour)
}

func (h *hookContext) takeInflight() (time.Time, bool) {
	if h.in.ToolUseID == "" {
		return time.Time{}, false
	}
	path := h.inflightPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	os.Remove(path)
	var rec inflight
	if json.Unmarshal(data, &rec) != nil {
		return time.Time{}, false
	}
	return rec.Started, true
}

type sessionMemory struct {
	LastTouched map[string]time.Time `json:"last_touched"`
	Told        map[string]bool      `json:"told"`
	Tree        string               `json:"tree,omitempty"`
	Held        map[string]time.Time `json:"held,omitempty"`
	Observed    time.Time            `json:"observed,omitempty"`
	Reminded    time.Time            `json:"reminded,omitempty"`
}

const waitingReminder = 10 * time.Minute

func (h *hookContext) touch(now time.Time) {
	if h.caller == nil {
		return
	}
	self := h.caller.Path
	relevant := len(h.st.HeldBy(self, now)) > 0 || queue.Position(h.st, self) > 0
	for _, name := range h.st.Names() {
		if relevant {
			break
		}
		r := h.st.Resources[name]
		relevant = r.Repo != "" && r.Repo == h.caller.Common
	}
	if relevant {
		h.store.Touch(self, now)
	}
}

func (h *hookContext) sessionPath() string {
	id := h.in.SessionID
	if id == "" {
		id = "unknown"
	}
	return filepath.Join(h.store.Dir, "sessions", unsafeFileChars.ReplaceAllString(id, "_")+".json")
}

func (h *hookContext) loadSession() *sessionMemory {
	mem := &sessionMemory{LastTouched: map[string]time.Time{}, Told: map[string]bool{}}
	if data, err := os.ReadFile(h.sessionPath()); err == nil {
		json.Unmarshal(data, mem)
	}
	if mem.LastTouched == nil {
		mem.LastTouched = map[string]time.Time{}
	}
	if mem.Told == nil {
		mem.Told = map[string]bool{}
	}
	return mem
}

func (h *hookContext) saveSession(mem *sessionMemory) {
	path := h.sessionPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(mem)
	os.WriteFile(path, data, 0o644)
	sweep(filepath.Dir(path), 7*24*time.Hour)
}

func (h *hookContext) post(w io.Writer) {
	now := time.Now()
	h.touch(now)
	mem := h.loadSession()
	notes, dirty := h.swapNotes(mem, now)
	leaseNotes, leaseDirty := h.leaseNotes(mem, now)
	notes = append(notes, leaseNotes...)
	if dirty || leaseDirty || len(notes) > 0 {
		h.saveSession(mem)
	}
	if len(notes) > 0 {
		emitContext(w, "PostToolUse", strings.Join(notes, "\n\n"))
	}
}

func (h *hookContext) swapNotes(mem *sessionMemory, now time.Time) ([]string, bool) {
	if h.in.ToolName != "Bash" && h.in.ToolName != "Read" {
		return nil, false
	}
	windows := map[string]time.Time{}
	for name, last := range mem.LastTouched {
		if now.Sub(last) > 6*time.Hour {
			delete(mem.LastTouched, name)
			continue
		}
		windows[name] = last
	}

	var touched []string
	if h.in.ToolName == "Bash" {
		touched = guard.Touches(h.st, h.in.ToolInput.Command)
		if len(touched) > 0 {
			started := now
			if !h.in.ToolInput.RunInBackground {
				if recorded, ok := h.takeInflight(); ok {
					started = recorded
				} else {
					timeout := time.Duration(h.in.ToolInput.Timeout) * time.Millisecond
					if timeout <= 0 || timeout > 10*time.Minute {
						timeout = 2 * time.Minute
					}
					started = now.Add(-timeout)
				}
			}
			for _, name := range touched {
				if last, ok := windows[name]; !ok || started.Before(last) {
					windows[name] = started
				}
			}
		}
	}

	notes := []string{}
	if len(windows) > 0 {
		names := make([]string, 0, len(windows))
		earliest := now
		for name, start := range windows {
			names = append(names, name)
			if start.Before(earliest) {
				earliest = start
			}
		}
		sort.Strings(names)
		events, _ := h.store.Events(earliest)
		notes = append(notes, guard.SwapNotes(events, names, windows, h.self())...)
		for _, name := range names {
			mem.LastTouched[name] = now
		}
	}

	for _, held := range guard.HeldByOthers(h.st, touched, h.self(), now) {
		key := fmt.Sprintf("held:%s:%s:%d", held.Resource, held.Holder.Tree, held.Holder.Since.Unix())
		if !mem.Told[key] {
			mem.Told[key] = true
			notes = append(notes, guard.HeldText(held))
		}
	}
	return notes, len(windows) > 0
}

func (h *hookContext) leaseNotes(mem *sessionMemory, now time.Time) ([]string, bool) {
	self := h.self()
	held := map[string]time.Time{}
	for _, name := range h.st.HeldBy(self, now) {
		held[name] = h.st.Resources[name].Holder.Since
	}
	notes := []string{}
	if self != "" && mem.Tree == self {
		lost := []string{}
		for name := range mem.Held {
			if _, ok := held[name]; !ok {
				lost = append(lost, name)
			}
		}
		if len(lost) > 0 {
			sort.Strings(lost)
			events, _ := h.store.Events(mem.Observed.Add(-time.Minute))
			if lapses := guard.Lapses(h.st, events, lost, self, now); len(lapses) > 0 {
				notes = append(notes, guard.LapseText(lapses))
			}
		}
	}
	dirty := mem.Tree != self || !sameHeld(mem.Held, held)
	mem.Tree, mem.Held = self, held
	if dirty {
		mem.Observed = now
	}

	if expiring := guard.ExpiringLeases(h.st, self, now, 5*time.Minute); len(expiring) > 0 {
		key := fmt.Sprintf("lease:%d", expiring[0].Expires.Unix())
		if !mem.Told[key] {
			mem.Told[key] = true
			notes = append(notes, guard.LeaseText(expiring, now))
		}
	}

	if waiting := guard.WaitingOn(h.st, self, now); len(waiting) > 0 {
		fresh := false
		for _, w := range waiting {
			key := "waiting:" + w.Waiter.ID
			if !mem.Told[key] {
				mem.Told[key] = true
				fresh = true
			}
		}
		if fresh || now.Sub(mem.Reminded) >= waitingReminder {
			mem.Reminded = now
			notes = append(notes, guard.WaitingText(waiting, h.hold(now), h.cfg.IdleRelease.D(), now))
		}
	}
	return notes, dirty || len(notes) > 0
}

func (h *hookContext) hold(now time.Time) *state.Holder {
	for _, name := range h.st.HeldBy(h.self(), now) {
		if holder := h.st.Resources[name].Holder; holder.OnHold() {
			return holder
		}
	}
	return nil
}

func sameHeld(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for name, since := range a {
		if other, ok := b[name]; !ok || !other.Equal(since) {
			return false
		}
	}
	return true
}

func runContext(e *env, args []string) int {
	h, ok := loadHook(e.stdin)
	if !ok || h.caller == nil {
		return exitOK
	}
	if !guard.Relevant(h.cfg, h.st, h.caller) {
		return exitOK
	}
	event := "SessionStart"
	if h.in.HookEventName == "SubagentStart" {
		event = "SubagentStart"
	}
	text := guard.Primer(h.cfg, h.st, h.caller, TmpDir(h.caller.Path), time.Now(), event == "SubagentStart")
	emitContext(e.stdout, event, text)
	return exitOK
}

func sweep(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) < 50 {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
