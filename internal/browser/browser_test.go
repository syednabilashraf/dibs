package browser

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeChrome(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			w.Write([]byte(`{"Browser":"Chrome/140"}`))
			return
		}
		http.NotFound(w, r)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return listener.Addr().(*net.TCPAddr).Port
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestEnsureAttachesToRunningChrome(t *testing.T) {
	port := fakeChrome(t)
	opts := Options{Port: port, Profile: t.TempDir(), Chrome: "/nonexistent/chrome", StateDir: t.TempDir()}
	if err := Ensure(context.Background(), opts); err != nil {
		t.Fatalf("an answering debugging port must be reused without launching: %v", err)
	}
}

func TestEnsureReportsLaunchFailure(t *testing.T) {
	opts := Options{Port: freePort(t), Profile: t.TempDir(), Chrome: "/nonexistent/chrome", StateDir: t.TempDir()}
	err := Ensure(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "launch") {
		t.Fatalf("expected a launch error, got %v", err)
	}
}

func TestEnsureRefusesProfileHeldByAnotherChrome(t *testing.T) {
	profile := t.TempDir()
	os.Symlink("somehost-"+strconv.Itoa(os.Getpid()), filepath.Join(profile, "SingletonLock"))
	opts := Options{Port: freePort(t), Profile: profile, Chrome: "/nonexistent/chrome", StateDir: t.TempDir()}
	err := Ensure(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "already open in another Chrome") {
		t.Fatalf("expected a profile-in-use error, got %v", err)
	}
}

func TestProfileOwner(t *testing.T) {
	profile := t.TempDir()
	if ProfileOwner(profile) != 0 {
		t.Fatal("no lock means no owner")
	}
	os.Symlink("host-999999", filepath.Join(profile, "SingletonLock"))
	if ProfileOwner(profile) != 0 {
		t.Fatal("a stale lock from a dead pid is not an owner")
	}
}

func TestArgs(t *testing.T) {
	opts := Options{Port: 9333, Profile: "/p", MCPCommand: []string{"npx", "-y", "chrome-devtools-mcp@latest"}}
	got := MCPArgs(opts, []string{"--no-usage-statistics"})
	want := []string{"npx", "-y", "chrome-devtools-mcp@latest", "--browserUrl=http://127.0.0.1:9333", "--no-usage-statistics"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MCPArgs = %v", got)
	}
	launch := LaunchArgs(opts)
	if launch[0] != "--remote-debugging-port=9333" || launch[1] != "--user-data-dir=/p" {
		t.Fatalf("LaunchArgs = %v", launch)
	}
}

func TestEnsureLaunchesARealChromeOutsideTheCallersProcessTree(t *testing.T) {
	if os.Getenv("DIBS_CHROME_INTEGRATION") == "" {
		t.Skip("set DIBS_CHROME_INTEGRATION=1 to launch a real Chrome")
	}
	chrome, err := Detect()
	if err != nil {
		t.Skip(err)
	}
	opts := Options{Port: freePort(t), Profile: filepath.Join(t.TempDir(), "profile"), Chrome: chrome, StateDir: t.TempDir()}
	if err := Ensure(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	pid := ProfileOwner(opts.Profile)
	t.Cleanup(func() {
		if pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if pid == 0 {
		t.Fatal("the launched Chrome should own its profile")
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if appBundle(chrome) != "" && strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("Chrome should be started by launchd, not by the caller: ppid %s", out)
	}
	if !Ready(context.Background(), opts.Port) {
		t.Fatal("the debugging port should answer")
	}
}

func TestAZombieChromeDoesNotOwnTheProfile(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !zombie(cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Skip("could not observe a zombie process")
		}
		time.Sleep(20 * time.Millisecond)
	}
	profile := t.TempDir()
	os.Symlink("host-"+strconv.Itoa(cmd.Process.Pid), filepath.Join(profile, "SingletonLock"))
	if pid := ProfileOwner(profile); pid != 0 {
		t.Fatalf("a Chrome that exited but was never reaped must not hold the profile: pid %d", pid)
	}
}

func TestEnsureClearsAStaleProfileLock(t *testing.T) {
	profile := t.TempDir()
	os.Symlink("host-999999", filepath.Join(profile, "SingletonLock"))
	os.Symlink("/tmp/gone", filepath.Join(profile, "SingletonSocket"))
	opts := Options{Port: freePort(t), Profile: profile, Chrome: "/nonexistent/chrome", StateDir: t.TempDir()}
	Ensure(context.Background(), opts)
	for _, name := range []string{"SingletonLock", "SingletonSocket"} {
		if _, err := os.Lstat(filepath.Join(profile, name)); !os.IsNotExist(err) {
			t.Fatalf("%s from a dead Chrome should be cleared before launching", name)
		}
	}
}
