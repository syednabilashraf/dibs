package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func frame(stream byte, payload string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, payload...)
}

func TestDemux(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hello "))
	buf.Write(frame(2, "world\n"))
	buf.Write(frame(1, "done"))
	got, err := Demux(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world\ndone" {
		t.Fatalf("Demux = %q", got)
	}
}

func TestDemuxTruncated(t *testing.T) {
	data := frame(1, "complete")
	data = append(data, frame(1, "cut off")[:10]...)
	got, err := Demux(bytes.NewReader(data))
	if err != nil || got != "completecu" {
		t.Fatalf("a truncated final frame should keep what arrived: %q, %v", got, err)
	}
}

func TestResolveHostPrefersEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/custom.sock")
	if got := ResolveHost(); got != "unix:///tmp/custom.sock" {
		t.Fatalf("ResolveHost = %q", got)
	}
}

func TestSummarize(t *testing.T) {
	raw := map[string]any{
		"Id":   "abc123",
		"Name": "/web",
		"State": map[string]any{
			"Running": true, "Status": "running", "StartedAt": "2026-01-01T12:00:00.123456789Z",
			"Health": map[string]any{"Status": "healthy"},
		},
		"Config": map[string]any{
			"Image":       "web:dev",
			"Labels":      map[string]any{"com.docker.compose.service": "web"},
			"Healthcheck": map[string]any{"Test": []any{"CMD", "true"}},
		},
		"HostConfig": map[string]any{
			"PortBindings": map[string]any{
				"3000/tcp": []any{map[string]any{"HostIp": "", "HostPort": "3000"}},
				"9229/tcp": []any{map[string]any{"HostPort": "9229"}, map[string]any{"HostPort": "3000"}},
			},
		},
		"ExecIDs": []any{"e1"},
	}
	s := Summarize(raw)
	if s.Name != "web" || !s.Running || s.Health != "healthy" || !s.HasHealthcheck {
		t.Fatalf("summary = %+v", s)
	}
	if s.Label("com.docker.compose.service") != "web" {
		t.Fatal("labels not read")
	}
	if len(s.HostPorts) != 2 || s.HostPorts[0] != 3000 || s.HostPorts[1] != 9229 {
		t.Fatalf("host ports = %v", s.HostPorts)
	}
	if s.StartedAt.IsZero() || len(s.ExecIDs) != 1 {
		t.Fatalf("started/exec ids missing: %+v", s)
	}
}

func TestLiveDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx)
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}
	if c.Version == "" {
		t.Fatal("api version should be negotiated")
	}
	if _, err := c.Inspect(ctx, "dibs-definitely-missing-container"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing container should be ErrNotFound, got %v", err)
	}
}
