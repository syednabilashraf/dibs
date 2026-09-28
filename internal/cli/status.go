package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/syednabilashraf/dibs/internal/state"
)

func runStatus(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	fs := newFlags(e, "status", "[--json]")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	st, err := a.store.Read()
	if err != nil {
		return e.errorf("%v", err)
	}
	now := time.Now()
	recent, _ := a.store.Events(now.Add(-2 * time.Hour))
	if *asJSON {
		out := map[string]any{"resources": st.Resources, "queue": st.Queue, "recent": recent, "now": now}
		data, _ := json.MarshalIndent(out, "", "  ")
		e.printf("%s\n", data)
		return exitOK
	}
	writeStatus(e.stdout, st, recent, now)
	return exitOK
}

func writeStatus(w io.Writer, st *state.State, recent []state.Event, now time.Time) {
	if len(st.Resources) == 0 && len(st.Queue) == 0 {
		fmt.Fprintln(w, "dibs: nothing is held or managed yet")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tHOLDER\tSERVING\tSTATUS\tLEASE")
	for _, name := range st.Names() {
		r := st.Resources[name]
		h := r.ActiveHolder(now)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, holderText(h), servingText(r), statusText(r), leaseText(h, now))
	}
	tw.Flush()
	if len(st.Queue) > 0 {
		fmt.Fprintln(w)
		writeQueue(w, st, now)
	}
	if len(recent) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "RECENT")
		start := 0
		if len(recent) > 8 {
			start = len(recent) - 8
		}
		for _, ev := range recent[start:] {
			fmt.Fprintf(w, "  %s  %s\n", clock(ev.Time), eventText(ev))
		}
	}
}

func writeQueue(w io.Writer, st *state.State, now time.Time) {
	fmt.Fprintln(w, "QUEUE")
	for i, waiter := range st.Queue {
		alive := ""
		if !state.ProcessAlive(waiter.PID) {
			alive = " (waiter process gone)"
		}
		fmt.Fprintf(w, "  %d. %s (%s) wants %s, waiting %s%s\n", i+1, waiter.Label, treeName(waiter.Tree), strings.Join(waiter.Resources, ", "), fmtDuration(now.Sub(waiter.Since)), alive)
	}
}

func statusText(r *state.Resource) string {
	if r.Virtual {
		return "-"
	}
	if r.Status == state.StatusSwapping && r.Intent != nil && !state.ProcessAlive(r.Intent.PID) {
		return "stalled (retake to finish)"
	}
	if r.Status == "" {
		return "-"
	}
	return string(r.Status)
}

func leaseText(h *state.Holder, now time.Time) string {
	switch {
	case h == nil:
		return "-"
	case h.Pinned():
		return "until dropped"
	default:
		return fmtDuration(h.Expires.Sub(now)) + " left"
	}
}

func eventText(ev state.Event) string {
	who := ev.Label
	if who == "" {
		who = treeName(ev.Tree)
	}
	switch ev.Kind {
	case "swap":
		target := ev.Label
		if ev.Tree == "" {
			target = "the baseline"
		}
		text := fmt.Sprintf("%s recreated to serve %s", ev.Resource, target)
		if ev.Detail != "" {
			text += fmt.Sprintf(" (was %s)", ev.Detail)
		}
		return text
	case "restore":
		return fmt.Sprintf("%s restored to the baseline %s", ev.Resource, ev.Detail)
	case "take":
		return fmt.Sprintf("%s took %s", who, ev.Resource)
	case "pass":
		return fmt.Sprintf("%s passed %s", who, ev.Resource)
	case "expire":
		return fmt.Sprintf("%s's lease on %s expired", who, ev.Resource)
	case "abandon":
		return fmt.Sprintf("%s stopped waiting for %s", who, ev.Resource)
	case "grab":
		return fmt.Sprintf("%s pinned %s by hand %s", who, ev.Resource, ev.Detail)
	case "drop":
		return fmt.Sprintf("pins on %s dropped", ev.Resource)
	}
	return fmt.Sprintf("%s %s %s", ev.Kind, ev.Resource, who)
}

func runLine(e *env, args []string) int {
	a, err := e.app()
	if err != nil {
		return e.errorf("%v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		return e.errorf("%v", err)
	}
	if len(st.Queue) == 0 {
		e.printf("dibs: nobody is waiting\n")
		return exitOK
	}
	writeQueue(e.stdout, st, time.Now())
	return exitOK
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

func TmpDir(treePath string) string {
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(filepath.Base(treePath)), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(treePath))
	return fmt.Sprintf("/tmp/dibs-%s-%s", slug, hex.EncodeToString(sum[:])[:8])
}

func runTmpdir(e *env, args []string) int {
	fs := newFlags(e, "tmpdir", "[--tree DIR]")
	treeFlag := fs.String("tree", "", "worktree (default: current directory)")
	if _, err := parse(fs, args); err != nil {
		return exitError
	}
	t, err := resolveTree(*treeFlag)
	if err != nil {
		return e.errorf("%v", err)
	}
	e.printf("%s\n", TmpDir(t.Path))
	return exitOK
}
