package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the CLI's side of the API.
type Client struct {
	addr, token string
	http        *http.Client
}

// NewClient talks to the agent at addr with the bearer token.
func NewClient(addr, token string) *Client {
	return &Client{addr: addr, token: token, http: &http.Client{Timeout: 20 * time.Minute}}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.addr+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// Status fetches the snapshot.
func (c *Client) Status(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	if err := c.do(ctx, http.MethodGet, "/api/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Logs fetches the last n lines.
func (c *Client) Logs(ctx context.Context, n int) ([]json.RawMessage, error) {
	var lines []json.RawMessage
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/logs?n=%d", n), &lines)
	return lines, err
}

// LoginLink mints the one-time link for the browser.
func (c *Client) LoginLink(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/login-link", &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// UpdateCheck asks the agent to check for a release.
func (c *Client) UpdateCheck(ctx context.Context) (string, error) {
	var out struct {
		Available string `json:"available"`
	}
	err := c.do(ctx, http.MethodPost, "/api/update/check", &out)
	return out.Available, err
}

// UpdateApply asks the agent to install the available release.
func (c *Client) UpdateApply(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/update/apply", nil)
}

// UpdateRetry forgets a failed probation so the version may be installed again.
func (c *Client) UpdateRetry(ctx context.Context, version string) error {
	return c.do(ctx, http.MethodPost, "/api/update/retry?version="+url.QueryEscape(version), nil)
}
