package browser

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type fakeDevtools struct {
	mu      sync.Mutex
	targets []Target
	closed  []string
	opened  int
	calls   int
	Port    int
}

func newFakeDevtools(t *testing.T, targets ...Target) *fakeDevtools {
	t.Helper()
	f := &fakeDevtools{targets: targets}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	f.Port = listener.Addr().(*net.TCPAddr).Port
	return f
}

func (f *fakeDevtools) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	switch {
	case r.URL.Path == "/json/version":
		w.Write([]byte(`{"Browser":"Chrome/140"}`))
	case r.URL.Path == "/json/list":
		json.NewEncoder(w).Encode(f.targets)
	case r.URL.Path == "/json/new" && r.Method == http.MethodPut:
		f.opened++
		blank := Target{ID: "new-blank", Type: "page", URL: r.URL.RawQuery}
		f.targets = append(f.targets, blank)
		json.NewEncoder(w).Encode(blank)
	case strings.HasPrefix(r.URL.Path, "/json/close/"):
		id := strings.TrimPrefix(r.URL.Path, "/json/close/")
		f.closed = append(f.closed, id)
		kept := f.targets[:0]
		for _, t := range f.targets {
			if t.ID != id {
				kept = append(kept, t)
			}
		}
		f.targets = kept
		w.Write([]byte("Target is closing"))
	default:
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
	}
}

func (f *fakeDevtools) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestResetTabsKeepsOneBlankTab(t *testing.T) {
	f := newFakeDevtools(t,
		Target{ID: "app", Type: "page", URL: "https://app.local/chat"},
		Target{ID: "blank", Type: "page", URL: "about:blank"},
		Target{ID: "sw", Type: "service_worker", URL: "chrome-extension://x/sw.js"},
		Target{ID: "login", Type: "page", URL: "https://login.example.com/"},
	)
	closed, err := ResetTabs(context.Background(), f.Port)
	if err != nil || closed != 2 {
		t.Fatalf("closed %d, %v", closed, err)
	}
	if !reflect.DeepEqual(f.closed, []string{"app", "login"}) || f.opened != 0 {
		t.Fatalf("closed %v, opened %d: only pages other than the blank one go", f.closed, f.opened)
	}
}

func TestResetTabsOpensABlankTabBeforeClosingTheRest(t *testing.T) {
	f := newFakeDevtools(t, Target{ID: "app", Type: "page", URL: "https://app.local/"})
	closed, err := ResetTabs(context.Background(), f.Port)
	if err != nil || closed != 1 || f.opened != 1 {
		t.Fatalf("closed %d opened %d: %v", closed, f.opened, err)
	}
	pages, _ := Pages(context.Background(), f.Port)
	if len(pages) != 1 || pages[0].URL != "about:blank" {
		t.Fatalf("pages left: %+v", pages)
	}
}

func TestResetTabsWithoutChrome(t *testing.T) {
	if _, err := ResetTabs(context.Background(), freePort(t)); err == nil {
		t.Fatal("no browser answering is an error the caller can ignore")
	}
}

func TestLaunchKeepsSessionsAndUsesTheAppBundle(t *testing.T) {
	args := LaunchArgs(Options{Port: 9333, Profile: "/p"})
	if !contains(args, "--restore-last-session") {
		t.Fatalf("relaunches must keep session cookies (logins): %v", args)
	}
	if runtime.GOOS != "darwin" {
		t.Skip("app bundles are a macOS thing")
	}
	if got := appBundle("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"); got != "/Applications/Google Chrome.app" {
		t.Fatalf("bundle = %q", got)
	}
	if got := appBundle("/usr/bin/chromium"); got != "" {
		t.Fatalf("a plain binary has no bundle: %q", got)
	}
}

func TestManagedMarker(t *testing.T) {
	opts := Options{Port: 9333, StateDir: t.TempDir()}
	if Managed(opts) {
		t.Fatal("not managed until browser-mcp has run")
	}
	MarkManaged(opts)
	if !Managed(opts) {
		t.Fatal("managed once browser-mcp has run")
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
