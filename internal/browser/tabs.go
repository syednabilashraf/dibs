package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type Target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func devtools(ctx context.Context, port int, method, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, Endpoint(port)+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func Pages(ctx context.Context, port int) ([]Target, error) {
	var targets []Target
	if err := devtools(ctx, port, http.MethodGet, "/json/list", &targets); err != nil {
		return nil, err
	}
	pages := []Target{}
	for _, t := range targets {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

func ResetTabs(ctx context.Context, port int) (int, error) {
	pages, err := Pages(ctx, port)
	if err != nil {
		return 0, err
	}
	keep := ""
	for _, p := range pages {
		if p.URL == "about:blank" {
			keep = p.ID
			break
		}
	}
	if keep == "" {
		var blank Target
		if err := devtools(ctx, port, http.MethodPut, "/json/new?about:blank", &blank); err != nil {
			return 0, err
		}
		keep = blank.ID
	}
	closed := 0
	for _, p := range pages {
		if p.ID != keep && devtools(ctx, port, http.MethodGet, "/json/close/"+url.PathEscape(p.ID), nil) == nil {
			closed++
		}
	}
	return closed, nil
}
