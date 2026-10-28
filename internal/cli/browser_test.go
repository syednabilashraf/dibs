package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/syednabilashraf/dibs/internal/browser"
	"github.com/syednabilashraf/dibs/internal/config"
	"github.com/syednabilashraf/dibs/internal/pressure"
	"github.com/syednabilashraf/dibs/internal/state"
)

type devtoolsStub struct {
	mu     sync.Mutex
	pages  []browser.Target
	closed []string
	calls  int
	port   int
}

func newDevtoolsStub(t *testing.T, urls ...string) *devtoolsStub {
	t.Helper()
	d := &devtoolsStub{}
	for i, u := range urls {
		d.pages = append(d.pages, browser.Target{ID: fmt.Sprintf("p%d", i), Type: "page", URL: u})
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.calls++
		switch {
		case r.URL.Path == "/json/version":
			w.Write([]byte(`{}`))
		case r.URL.Path == "/json/list":
			json.NewEncoder(w).Encode(d.pages)
		case strings.HasPrefix(r.URL.Path, "/json/close/"):
			id := strings.TrimPrefix(r.URL.Path, "/json/close/")
			d.closed = append(d.closed, id)
			kept := d.pages[:0]
			for _, p := range d.pages {
				if p.ID != id {
					kept = append(kept, p)
				}
			}
			d.pages = kept
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	d.port = listener.Addr().(*net.TCPAddr).Port
	return d
}

func (d *devtoolsStub) snapshot() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, append([]string(nil), d.closed...)
}

func browserWorld(t *testing.T, port int, extra string, managed bool) hookWorld {
	t.Helper()
	w := newHookWorld(t)
	body := fmt.Sprintf("browser:\n  port: %d\n  profile: %s\n%s", port, filepath.Join(t.TempDir(), "profile"), extra)
	os.WriteFile(filepath.Join(w.home, "config.yaml"), []byte(body), 0o644)
	if managed {
		browser.MarkManaged(browser.Options{Port: port, StateDir: w.home})
	}
	return w
}

func browserCallInput(t *testing.T, tool, cwd string) string {
	return hookInput(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__chrome-devtools__" + tool, "cwd": cwd})
}

func TestBrowserHookCapsTabs(t *testing.T) {
	d := newDevtoolsStub(t, "about:blank", "https://app.local/a", "https://app.local/b")
	w := browserWorld(t, d.port, "  max_tabs: 3\n", true)
	_, out := callHook(t, "guard", browserCallInput(t, "new_page", w.a.Path))
	if !strings.Contains(out, "3 tabs are already open in the shared browser") || !strings.Contains(out, "close_page") {
		t.Fatalf("a new tab past the cap is denied: %q", out)
	}
	if _, out := callHook(t, "guard", browserCallInput(t, "click", w.a.Path)); out != "" {
		t.Fatalf("other browser calls are not capped: %q", out)
	}
	d.mu.Lock()
	d.pages = d.pages[:2]
	d.mu.Unlock()
	if _, out := callHook(t, "guard", browserCallInput(t, "new_page", w.a.Path)); out != "" {
		t.Fatalf("under the cap a new tab is fine: %q", out)
	}
}

func TestBrowserHookLeavesAnUnmanagedBrowserAlone(t *testing.T) {
	d := newDevtoolsStub(t, "about:blank", "https://a", "https://b", "https://c")
	w := browserWorld(t, d.port, "  max_tabs: 1\n", false)
	if _, out := callHook(t, "guard", browserCallInput(t, "new_page", w.a.Path)); out != "" {
		t.Fatalf("without dibs browser-mcp, dibs does not police the browser: %q", out)
	}
	if calls, _ := d.snapshot(); calls != 0 {
		t.Fatalf("an unmanaged browser must not even be contacted: %d calls", calls)
	}
}

const fakeChrome = `#!/bin/sh
exec python3 -I -c '
import sys, os, http.server
port = int([a.split("=", 1)[1] for a in sys.argv[1:] if a.startswith("--remote-debugging-port=")][0])
open(os.environ["FAKE_CHROME_PID"], "w").write(str(os.getpid()))
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
' "$@"
`

func TestBrowserHookRelaunchesAChromeThatDied(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	script := filepath.Join(t.TempDir(), "chrome")
	os.WriteFile(script, []byte(fakeChrome), 0o755)
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_CHROME_PID", pidFile)
	w := browserWorld(t, port, "  chrome: "+script+"\n", true)
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	if _, out := callHook(t, "guard", browserCallInput(t, "take_screenshot", w.a.Path)); out != "" {
		t.Fatalf("the call is allowed: %q", out)
	}
	if !browser.Ready(t.Context(), port) {
		t.Fatal("the hook should have relaunched the shared browser before the call")
	}
}

func TestTakeGivesANewHolderACleanBrowser(t *testing.T) {
	d := newDevtoolsStub(t, "https://app.local/left-behind", "about:blank")
	w := browserWorld(t, d.port, "", true)
	store, _ := state.Open(w.home)
	store.Update(func(st *state.State) error {
		delete(st.Resources, config.BrowserResource)
		return nil
	})
	var out bytes.Buffer
	if code := Run([]string{"take", "browser", "--tree", w.b.Path}, strings.NewReader(""), &out, &out); code != exitOK {
		t.Fatalf("take: %d %s", code, out.String())
	}
	if _, closed := d.snapshot(); len(closed) != 1 || closed[0] != "p0" || !strings.Contains(out.String(), "closed 1 tab(s) left by earlier holders") {
		t.Fatalf("closed %v:\n%s", closed, out.String())
	}
	d.mu.Lock()
	d.pages = append(d.pages, browser.Target{ID: "mine", Type: "page", URL: "https://app.local/mine"})
	d.mu.Unlock()
	out.Reset()
	time.Sleep(10 * time.Millisecond)
	if code := Run([]string{"take", "browser", "--tree", w.b.Path}, strings.NewReader(""), &out, &out); code != exitOK {
		t.Fatalf("retake: %d %s", code, out.String())
	}
	if _, closed := d.snapshot(); len(closed) != 1 {
		t.Fatalf("taking again must not close the holder's own tabs: %v", closed)
	}
}

func underPressure(t *testing.T, level pressure.Level) {
	t.Helper()
	saved := pressure.Current
	pressure.Current = func(context.Context) pressure.Level { return level }
	t.Cleanup(func() { pressure.Current = saved })
}

func TestBrowserHookAsksToWaitWhenMemoryIsCritical(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	w := browserWorld(t, port, "  chrome: /nonexistent/chrome\n", true)
	cfg, _ := config.Load()
	os.MkdirAll(cfg.Browser.Profile, 0o755)
	os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), filepath.Join(cfg.Browser.Profile, "SingletonLock"))

	underPressure(t, pressure.Critical)
	if _, out := callHook(t, "guard", browserCallInput(t, "take_screenshot", w.a.Path)); !strings.Contains(out, "Do not kill or relaunch it") {
		t.Fatalf("a hung browser under memory pressure should be waited for: %q", out)
	}
	underPressure(t, pressure.Normal)
	if _, out := callHook(t, "guard", browserCallInput(t, "take_screenshot", w.a.Path)); out != "" {
		t.Fatalf("without pressure dibs stays out of the way: %q", out)
	}
}

func TestBrowserHookHoldsBackTabsWhenMemoryIsCritical(t *testing.T) {
	d := newDevtoolsStub(t, "about:blank")
	w := browserWorld(t, d.port, "", true)
	underPressure(t, pressure.Critical)
	if _, out := callHook(t, "guard", browserCallInput(t, "new_page", w.a.Path)); out != "" {
		t.Fatalf("a session with only the blank tab may open one: %q", out)
	}
	d.mu.Lock()
	d.pages = append(d.pages, browser.Target{ID: "work", Type: "page", URL: "https://app.local/"})
	d.mu.Unlock()
	if _, out := callHook(t, "guard", browserCallInput(t, "new_page", w.a.Path)); !strings.Contains(out, "memory pressure is critical") {
		t.Fatalf("under critical pressure, reuse a tab instead of opening more: %q", out)
	}
}

func TestPostHookWarnsHoldersWhenMemoryIsCritical(t *testing.T) {
	w := newHookWorld(t)
	underPressure(t, pressure.Critical)
	text := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_1", w.a.Path, "ls"))
	if !strings.Contains(text, "memory pressure is critical") || !strings.Contains(text, "Pass what you are not testing with") {
		t.Fatalf("a holder should be warned: %q", text)
	}
	if again := callPost(t, bashHook(t, "PostToolUse", "s-a", "toolu_2", w.a.Path, "ls")); again != "" {
		t.Fatalf("the warning is not repeated on every call: %q", again)
	}
	if other := callPost(t, bashHook(t, "PostToolUse", "s-b", "toolu_3", w.b.Path, "ls")); other != "" {
		t.Fatalf("a session holding nothing is not nagged: %q", other)
	}
}

func TestPassClosesTheBrowserTabs(t *testing.T) {
	d := newDevtoolsStub(t, "https://app.local/result", "about:blank")
	w := browserWorld(t, d.port, "", true)
	var out bytes.Buffer
	if code := Run([]string{"pass", "--tree", w.a.Path}, strings.NewReader(""), &out, &out); code != exitOK {
		t.Fatalf("pass: %d %s", code, out.String())
	}
	if _, closed := d.snapshot(); len(closed) != 1 || !strings.Contains(out.String(), "closed 1 tab(s) you had open") {
		t.Fatalf("passing the browser closes its tabs: %v\n%s", closed, out.String())
	}
}
