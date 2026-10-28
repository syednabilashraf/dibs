package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Options struct {
	Port       int
	Profile    string
	Chrome     string
	MCPCommand []string
	StateDir   string
}

func Endpoint(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

func Ready(ctx context.Context, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint(port)+"/json/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func Ensure(ctx context.Context, opts Options) error {
	if Ready(ctx, opts.Port) {
		return nil
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(opts.StateDir, "browser.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if Ready(ctx, opts.Port) {
		return nil
	}
	chrome := opts.Chrome
	if chrome == "" {
		if chrome, err = Detect(); err != nil {
			return err
		}
	}
	if pid := ProfileOwner(opts.Profile); pid != 0 {
		return fmt.Errorf("the Chrome profile %s is already open in another Chrome (pid %d) without remote debugging on port %d; close that Chrome or point browser.profile at a different directory", opts.Profile, pid, opts.Port)
	}
	clearStaleLock(opts.Profile)
	if err := os.MkdirAll(opts.Profile, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(opts.StateDir, "chrome.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	if err := launch(chrome, LaunchArgs(opts), logFile); err != nil {
		return fmt.Errorf("launch %s: %w", chrome, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if Ready(ctx, opts.Port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("Chrome did not open its debugging port %d within 15s; see %s", opts.Port, filepath.Join(opts.StateDir, "chrome.log"))
}

func markerPath(stateDir string) string { return filepath.Join(stateDir, "browser-mcp") }

func MarkManaged(opts Options) {
	os.WriteFile(markerPath(opts.StateDir), []byte(Endpoint(opts.Port)+"\n"), 0o644)
}

func Managed(opts Options) bool {
	_, err := os.Stat(markerPath(opts.StateDir))
	return err == nil
}

func LaunchArgs(opts Options) []string {
	return []string{
		"--remote-debugging-port=" + strconv.Itoa(opts.Port),
		"--user-data-dir=" + opts.Profile,
		"--restore-last-session",
		"--hide-crash-restore-bubble",
		"--no-first-run",
		"--no-default-browser-check",
		"about:blank",
	}
}

func launch(chrome string, args []string, logFile *os.File) error {
	if bundle := appBundle(chrome); bundle != "" {
		openArgs := append([]string{"-n", "-g", "--stdout", logFile.Name(), "--stderr", logFile.Name(), bundle, "--args"}, args...)
		if out, err := exec.Command("open", openArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	cmd := exec.Command(chrome, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func appBundle(binary string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	i := strings.Index(binary, ".app/Contents/MacOS/")
	if i < 0 {
		return ""
	}
	return binary[:i+len(".app")]
}

func MCPArgs(opts Options, passthrough []string) []string {
	argv := append([]string{}, opts.MCPCommand...)
	argv = append(argv, "--browserUrl="+Endpoint(opts.Port))
	return append(argv, passthrough...)
}

func Exec(opts Options, passthrough []string) error {
	if len(opts.MCPCommand) == 0 {
		return errors.New("browser.mcp_command is empty")
	}
	path, err := exec.LookPath(opts.MCPCommand[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, MCPArgs(opts, passthrough), os.Environ())
}

func Detect() (string, error) {
	candidates := []string{}
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		for _, app := range []string{"Google Chrome.app/Contents/MacOS/Google Chrome", "Chromium.app/Contents/MacOS/Chromium", "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"} {
			candidates = append(candidates, filepath.Join("/Applications", app), filepath.Join(home, "Applications", app))
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
			if path, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, path)
			}
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found; set browser.chrome in the dibs config")
}

func ProfileOwner(profile string) int {
	target, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil {
		return 0
	}
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return 0
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return 0
	}
	if zombie(pid) {
		return 0
	}
	return pid
}

func zombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func clearStaleLock(profile string) {
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		os.Remove(filepath.Join(profile, name))
	}
}
