package pressure

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	os.WriteFile(path, []byte(body), 0o644)
	return path
}

func TestPSI(t *testing.T) {
	cases := map[string]Level{
		"some avg10=1.00 avg60=0.50 avg300=0.10 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n":    Normal,
		"some avg10=35.00 avg60=20.00 avg300=5.00 total=9\nfull avg10=4.00 avg60=1.00 avg300=0.10 total=2\n":  Warning,
		"some avg10=80.00 avg60=60.00 avg300=30.00 total=9\nfull avg10=25.00 avg60=9.00 avg300=2.0 total=5\n": Critical,
	}
	for body, want := range cases {
		if got := psi(write(t, body)); got != want {
			t.Fatalf("psi(%q) = %s, want %s", body, got, want)
		}
	}
	if psi(filepath.Join(t.TempDir(), "missing")) != Unknown {
		t.Fatal("no pressure file, unknown")
	}
}

func TestMeminfo(t *testing.T) {
	cases := map[string]Level{
		"MemTotal: 1000 kB\nMemAvailable: 500 kB\n": Normal,
		"MemTotal: 1000 kB\nMemAvailable: 80 kB\n":  Warning,
		"MemTotal: 1000 kB\nMemAvailable: 30 kB\n":  Critical,
		"Nothing: 1\n": Unknown,
	}
	for body, want := range cases {
		if got := meminfo(write(t, body)); got != want {
			t.Fatalf("meminfo(%q) = %s, want %s", body, got, want)
		}
	}
}

func TestReadOnThisMachine(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no reading on this platform")
	}
	if level := Current(context.Background()); level == Unknown {
		t.Fatal("this platform should report a memory pressure level")
	}
}
