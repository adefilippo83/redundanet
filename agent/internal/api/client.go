package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
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

// doJSON sends a JSON body.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// Backups is the jobs' status.
func (c *Client) Backups(ctx context.Context) (*jobs.Status, error) {
	var st jobs.Status
	return &st, c.do(ctx, http.MethodGet, "/api/backups", &st)
}

// BackupAdd creates or replaces a set.
func (c *Client) BackupAdd(ctx context.Context, set backup.Set) error {
	return c.doJSON(ctx, http.MethodPost, "/api/backup/add", set, nil)
}

// BackupRemove deletes a set.
func (c *Client) BackupRemove(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/api/backup/remove?set="+url.QueryEscape(name), nil)
}

// BackupRun asks for a run of one set, or all with "".
func (c *Client) BackupRun(ctx context.Context, set string) error {
	return c.do(ctx, http.MethodPost, "/api/backup/run?set="+url.QueryEscape(set), nil)
}

// BackupVerify asks for a restore test.
func (c *Client) BackupVerify(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/backup/verify", nil)
}

// RestoreList is what the member can restore from.
func (c *Client) RestoreList(ctx context.Context) (*restore.Listing, error) {
	var l restore.Listing
	return &l, c.do(ctx, http.MethodGet, "/api/restore/list", &l)
}

// RestoreBrowse walks a snapshot down a slash-separated path.
func (c *Client) RestoreBrowse(ctx context.Context, device, snap, path string) (*snapshot.Entry, *snapshot.Directory, error) {
	var out struct {
		Entry     *snapshot.Entry     `json:"entry"`
		Directory *snapshot.Directory `json:"directory"`
	}
	q := url.Values{"device": {device}, "snapshot": {snap}, "path": {path}}
	err := c.do(ctx, http.MethodGet, "/api/restore/browse?"+q.Encode(), &out)
	return out.Entry, out.Directory, err
}

// RestoreStart starts a restore in the agent.
func (c *Client) RestoreStart(ctx context.Context, req RestoreRequest) error {
	return c.doJSON(ctx, http.MethodPost, "/api/restore/start", req, nil)
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
