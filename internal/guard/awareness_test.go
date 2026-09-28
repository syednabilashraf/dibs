package guard

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/state"
)

func TestTouches(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	cases := map[string][]string{
		"docker exec web pytest":                  {"web"},
		"docker compose exec web-svc ls":          {"web"},
		"docker exec api ls && docker logs web":   {"api", "web"},
		"docker exec webhook ls":                  {"webhook"},
		"dibs take web --wait":                    {},
		"docker cp ./x web:/tmp/dibs-app-b-1234/": {"web"},
	}
	for command, want := range cases {
		if got := Touches(st, command); !reflect.DeepEqual(got, want) {
			t.Errorf("Touches(%q) = %v, want %v", command, got, want)
		}
	}
}

func TestExecTargets(t *testing.T) {
	cases := map[string][]string{
		"docker exec -e SLOW=60 qa-web sh /tmp/x/test.sh":     {"qa-web"},
		"docker exec -it -w /tmp/dibs-x --user root api bash": {"api"},
		"docker cp ./web/. qa-web:/tmp/dibs-app-c/":           {"qa-web"},
		"docker cp qa-web:/tmp/out.log ./out.log":             {"qa-web"},
		"docker container exec worker true":                   {"worker"},
		"echo docker":                                         {},
	}
	for command, want := range cases {
		got := ExecTargets(command)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ExecTargets(%q) = %v, want %v", command, got, want)
		}
	}
	if got := Touches(state.New(), "docker exec -e SLOW=1 qa-web sh t.sh"); !reflect.DeepEqual(got, []string{"qa-web"}) {
		t.Fatalf("containers dibs does not manage yet are still tracked: %v", got)
	}
}

func TestSwapNotes(t *testing.T) {
	start := now.Add(-time.Minute)
	events := []state.Event{
		{Time: now.Add(-2 * time.Minute), Kind: "swap", Resource: "web", Tree: "/w/old", Label: "old", Actor: "/w/old"},
		{Time: now.Add(-30 * time.Second), Kind: "swap", Resource: "web", Tree: "/w/a", Label: "ticket-a", Actor: "/w/a"},
		{Time: now.Add(-20 * time.Second), Kind: "swap", Resource: "api", Tree: "", Actor: "/w/b"},
	}
	windows := map[string]time.Time{"web": start, "api": start}

	notes := SwapNotes(events, []string{"web", "api"}, windows, "/w/b")
	if len(notes) != 1 || !strings.Contains(notes[0], "web was recreated") || !strings.Contains(notes[0], "ticket-a (worktree a)") {
		t.Fatalf("b should hear about a's swap of web but not its own swap of api: %v", notes)
	}
	if notes := SwapNotes(events, []string{"web"}, windows, "/w/a"); len(notes) != 0 {
		t.Fatalf("a caused the swap and must not be told about it: %v", notes)
	}
	if notes := SwapNotes(events, []string{"web"}, map[string]time.Time{"web": now}, "/w/b"); len(notes) != 0 {
		t.Fatalf("swaps before the window are old news: %v", notes)
	}
	restored := []state.Event{{Time: now, Kind: "swap", Resource: "web", Tree: "", Actor: "/w/a"}}
	if notes := SwapNotes(restored, []string{"web"}, windows, "/w/b"); len(notes) != 1 || !strings.Contains(notes[0], "the baseline") {
		t.Fatalf("restores should read as the baseline: %v", notes)
	}
}

func TestExpiringLeases(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	if got := ExpiringLeases(st, w.a.Path, now, 5*time.Minute); len(got) != 0 {
		t.Fatalf("25 minutes left is not expiring: %v", got)
	}
	got := ExpiringLeases(st, w.a.Path, now.Add(21*time.Minute), 5*time.Minute)
	if len(got) != 2 {
		t.Fatalf("both of a's leases end within 5 minutes: %v", got)
	}
	if text := LeaseText(got, now.Add(21*time.Minute)); !strings.Contains(text, "dibs renew") {
		t.Fatalf("lease text should say how to renew: %s", text)
	}
}

func TestPrimerAndRelevance(t *testing.T) {
	w := newWorld(t)
	st := w.aHoldsEverything()
	w.cfg.Groups = map[string][]string{"ui": {"browser", "web"}}
	st.Resources["web"].Repo = w.main.Common

	if !Relevant(w.cfg, st, w.b) {
		t.Fatal("a worktree of a managed repo is relevant")
	}
	if Relevant(w.cfg, state.New(), w.b) {
		t.Fatal("nothing managed and no configured repos means stay quiet")
	}
	w.cfg.Repos = []string{w.main.Path}
	if !Relevant(w.cfg, state.New(), w.b) {
		t.Fatal("a configured repo is relevant before anything is managed")
	}

	text := Primer(w.cfg, st, w.b, "/tmp/dibs-app-b-12345678", now)
	for _, want := range []string{"dibs take", "dibs check", "dibs pass", "/tmp/dibs-app-b-12345678", "ui = browser, web", "web: held by ticket-a", "running ticket-a", "api: free, running the baseline"} {
		if !strings.Contains(text, want) {
			t.Errorf("primer should mention %q:\n%s", want, text)
		}
	}
	if text := Snapshot(st, w.a, now); !strings.Contains(text, "web: held by you, running your worktree") {
		t.Errorf("snapshot for the holder:\n%s", text)
	}
}
