package cli

import (
	"encoding/json"
	"io"
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
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}}
	data, _ := json.Marshal(out)
	w.Write(append(data, '\n'))
}

func runGuard(e *env, args []string) int {
	h, ok := loadHook(e.stdin)
	if !ok {
		return exitOK
	}
	decision := guard.Decide(h.cfg, h.st, h.caller, h.in, time.Now(), state.ProcessAlive)
	if decision.Deny {
		emitDeny(e.stdout, decision.Reason)
	}
	return exitOK
}
