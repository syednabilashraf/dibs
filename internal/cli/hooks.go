package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/guard"
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
	decision := guard.Decide(h.cfg, h.st, h.caller, h.in, time.Now(), state.ProcessAlive)
	if decision.Deny {
		emitDeny(e.stdout, decision.Reason)
		return exitOK
	}
	if h.in.ToolName == "Bash" && h.in.ToolUseID != "" {
		if len(guard.Touches(h.st, h.in.ToolInput.Command)) > 0 {
			h.recordInflight()
		}
	}
	return exitOK
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
	if h.in.ToolName != "Bash" {
		return
	}
	now := time.Now()
	touched := guard.Touches(h.st, h.in.ToolInput.Command)
	notes := []string{}
	mem := h.loadSession()

	if len(touched) > 0 && !h.in.ToolInput.RunInBackground {
		started, ok := h.takeInflight()
		if !ok {
			timeout := time.Duration(h.in.ToolInput.Timeout) * time.Millisecond
			if timeout <= 0 || timeout > 10*time.Minute {
				timeout = 2 * time.Minute
			}
			started = now.Add(-timeout)
		}
		windows := map[string]time.Time{}
		earliest := started
		for _, name := range touched {
			windows[name] = started
			if last, seen := mem.LastTouched[name]; seen && last.Before(started) {
				windows[name] = last
			}
			if windows[name].Before(earliest) {
				earliest = windows[name]
			}
		}
		events, _ := h.store.Events(earliest)
		notes = append(notes, guard.SwapNotes(events, touched, windows, h.self())...)
		for _, name := range touched {
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

	if expiring := guard.ExpiringLeases(h.st, h.self(), now, 5*time.Minute); len(expiring) > 0 {
		key := fmt.Sprintf("lease:%d", expiring[0].Expires.Unix())
		if !mem.Told[key] {
			mem.Told[key] = true
			notes = append(notes, guard.LeaseText(expiring, now))
		}
	}

	if len(touched) > 0 || len(notes) > 0 {
		h.saveSession(mem)
	}
	if len(notes) > 0 {
		emitContext(w, "PostToolUse", strings.Join(notes, "\n\n"))
	}
}

func runContext(e *env, args []string) int {
	h, ok := loadHook(e.stdin)
	if !ok || h.caller == nil {
		return exitOK
	}
	if !guard.Relevant(h.cfg, h.st, h.caller) {
		return exitOK
	}
	text := guard.Primer(h.cfg, h.st, h.caller, TmpDir(h.caller.Path), time.Now())
	emitContext(e.stdout, "SessionStart", text)
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
