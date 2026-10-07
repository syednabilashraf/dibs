package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status)
}

type Client struct {
	http    *http.Client
	base    string
	Host    string
	Version string
}

func New(ctx context.Context) (*Client, error) {
	host := ResolveHost()
	c := &Client{Host: host}
	switch {
	case strings.HasPrefix(host, "unix://"):
		socket := strings.TrimPrefix(host, "unix://")
		c.http = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}}
		c.base = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		if os.Getenv("DOCKER_TLS_VERIFY") != "" {
			return nil, fmt.Errorf("docker over TLS (%s) is not supported", host)
		}
		c.http = &http.Client{}
		c.base = "http://" + strings.TrimPrefix(host, "tcp://")
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q", host)
	}

	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, &version); err != nil {
		return nil, fmt.Errorf("docker daemon unreachable at %s: %w", host, err)
	}
	c.Version = version.APIVersion
	if c.Version != "" {
		c.base += "/v" + c.Version
	}
	return c, nil
}

func ResolveHost() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err == nil {
		if host := strings.TrimSpace(string(out)); host != "" {
			return host
		}
	}
	return "unix:///var/run/docker.sock"
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Message string `json:"message"`
		}
		data, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(data, &apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		e := &APIError{Status: resp.StatusCode, Message: apiErr.Message}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, e.Message)
		}
		return e
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err := io.Copy(w, resp.Body)
		return err
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	return decoder.Decode(out)
}

func (c *Client) Inspect(ctx context.Context, name string) (map[string]any, error) {
	var raw map[string]any
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) Create(ctx context.Context, name string, body map[string]any) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	query := url.Values{"name": {name}}
	if err := c.do(ctx, http.MethodPost, "/containers/create", query, body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	query := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", query, nil, nil)
}

func (c *Client) Remove(ctx context.Context, id string) error {
	query := url.Values{"v": {"0"}, "force": {"1"}}
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), query, nil, nil)
}

func (c *Client) Connect(ctx context.Context, network, id string, endpoint map[string]any) error {
	body := map[string]any{"Container": id, "EndpointConfig": endpoint}
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}

func (c *Client) Logs(ctx context.Context, id string, since time.Time, tty bool) (string, error) {
	query := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if !since.IsZero() {
		query.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	var buf bytes.Buffer
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", query, nil, &buf); err != nil {
		return "", err
	}
	if tty {
		return buf.String(), nil
	}
	return Demux(&buf)
}

func (c *Client) WritableSize(ctx context.Context, id string) (int64, error) {
	var sized struct {
		SizeRw int64 `json:"SizeRw"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", url.Values{"size": {"1"}}, nil, &sized)
	return sized.SizeRw, err
}

type FileChange struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"`
}

func (c *Client) Changes(ctx context.Context, id string) ([]FileChange, error) {
	var changes []FileChange
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/changes", nil, nil, &changes)
	return changes, err
}

type Exec struct {
	Running       bool `json:"Running"`
	ProcessConfig struct {
		Entrypoint string   `json:"entrypoint"`
		Arguments  []string `json:"arguments"`
	} `json:"ProcessConfig"`
}

func (e Exec) Command() string {
	return strings.TrimSpace(e.ProcessConfig.Entrypoint + " " + strings.Join(e.ProcessConfig.Arguments, " "))
}

func (c *Client) ExecInspect(ctx context.Context, id string) (Exec, error) {
	var ex Exec
	err := c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil, nil, &ex)
	return ex, err
}

func Demux(r io.Reader) (string, error) {
	var out strings.Builder
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out.String(), nil
			}
			return out.String(), err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if _, err := io.CopyN(&out, r, int64(size)); err != nil {
			return out.String(), nil
		}
	}
}
