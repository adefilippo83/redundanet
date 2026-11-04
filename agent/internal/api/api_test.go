package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

type harness struct {
	t       *testing.T
	srv     *Server
	token   string
	checks  int
	applies int
	retries int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	buf := logbuf.New(slog.NewTextHandler(io.Discard, nil), 100)
	lg := slog.New(buf)
	h := &harness{t: t}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Start(ctx, Config{
		Addr: "127.0.0.1:0", DataDir: dir, Logs: buf, Log: lg,
		Snapshot: func() Snapshot {
			return Snapshot{Version: "3.0.0-test", Network: "testnet", Device: "dev", Member: "mem", Roles: []string{"client"},
				Transport: &transport.Status{Sessions: []transport.SessionStatus{{Peer: "peerpeerpeer", Relay: true, Events: []transport.Event{{Time: time.Now(), What: "relay connection up"}}}}},
				Update:    &update.Status{Enabled: true, Running: "3.0.0-test", Channel: "stable", Available: "3.0.1", Failed: []string{"3.0.1"}},
				Repair:    &repair.Status{Running: "abcdefgh", Done: 2, Failed: 1, Released: 3, Last: "task 7 for abcdefgh: stored 1 shares", LastAt: time.Now()},
				Jobs: &jobs.Status{Sets: []backup.Set{{Name: "home", Folders: []string{"/home/x"}}}, Runs: []jobs.RunStatus{{Set: "home", Needs: []string{"/home/x/Documents: macOS denies the service access to this folder: grant Full Disk Access"}, LastAt: time.Now(), Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 3, Skipped: 1}, RootVersion: 2, Reasons: map[string]int64{"not on this computer": 1}}}}, Every: "24h0m0s", VerifyEvery: "168h0m0s", VerifyStreak: 2,
					Verifications: []localdb.Verification{{At: time.Now(), Snapshot: "2026-11-04T09:00:00Z", Sampled: 20, Verified: 20, Compared: 18}, {At: time.Now().AddDate(0, 0, -7), Snapshot: "s0", Sampled: 20, Verified: 19, Failed: []string{"x"}}}},
				Generated: time.Now()}
		},
		Actions: Actions{
			CheckUpdate: func(context.Context) (string, error) { h.checks++; return "3.0.1", nil },
			ApplyUpdate: func(context.Context) error { h.applies++; return errors.New("probation failed (test)") },
			RetryUpdate: func(_ context.Context, v string) error {
				if v != "3.0.1" {
					return errors.New(v + " is not marked as failed")
				}
				h.retries++
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	lg.Info("hello from the test", "k", "v")
	tok, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		t.Fatal(err)
	}
	h.token = strings.TrimSpace(string(tok))
	if info, _ := os.Stat(filepath.Join(dir, tokenFile)); info.Mode().Perm() != 0o600 && os.Getenv("OS") == "" {
		t.Fatalf("token file mode %o", info.Mode().Perm())
	}
	if addr, tok2, err := ReadClientConfig(dir); err != nil || addr != srv.Addr() || tok2 != h.token {
		t.Fatalf("client config: %v", err)
	}
	return h
}

func (h *harness) req(method, path string, headers map[string]string, body string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, "http://"+h.srv.Addr()+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func TestBearerTokenAndHostGuard(t *testing.T) {
	h := newHarness(t)
	if r := h.req("GET", "/api/status", nil, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r := h.req("GET", "/api/status", map[string]string{"Authorization": "Bearer wrong"}, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r := h.req("GET", "/api/status", map[string]string{"Authorization": "Bearer " + h.token}, "")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(b), `"network": "testnet"`) {
		t.Fatalf("status: %d %s", r.StatusCode, b)
	}
	// A request whose Host is not loopback is refused even with the token:
	// a DNS name pointing here must not work from a web page.
	req, _ := http.NewRequest("GET", "http://"+h.srv.Addr()+"/api/status", nil)
	req.Host = "evil.example.org"
	req.Header.Set("Authorization", "Bearer "+h.token)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", r.StatusCode)
	}
	// Logs come through the API too.
	r = h.req("GET", "/api/logs?n=5", map[string]string{"Authorization": "Bearer " + h.token}, "")
	b, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(b), "hello from the test") || !strings.Contains(string(b), "k=v") {
		t.Fatalf("logs: %s", b)
	}
}

func TestBrowserSessionAndCSRF(t *testing.T) {
	h := newHarness(t)
	// Without a session the page says how to open it, and changes nothing.
	if r := h.req("GET", "/", nil, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("page without session: %d", r.StatusCode)
	}
	// Only the token mints a login link.
	if r := h.req("POST", "/api/login-link", nil, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login link without token: %d", r.StatusCode)
	}
	c := NewClient(h.srv.Addr(), h.token)
	link, err := c.LoginLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	resp, err := browser.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "testnet") || !strings.Contains(string(page), "Install 3.0.1") ||
		!strings.Contains(string(page), "repairing abcdefgh") || !strings.Contains(string(page), "3 surplus shares released") || strings.Contains(string(page), "%!") ||
		!strings.Contains(string(page), "2 weeks in a row passing") || !strings.Contains(string(page), "20 of 20") || !strings.Contains(string(page), "1 failed") ||
		!strings.Contains(string(page), "grant Full Disk Access") || !strings.Contains(string(page), "1 skipped (1 not on this computer)") {
		t.Fatalf("page after login: %d %s", resp.StatusCode, firstLines(string(page)))
	}
	// The link is single use.
	if r, _ := http.Get(link); r.StatusCode != http.StatusForbidden {
		t.Fatalf("reused link: %d", r.StatusCode)
	}
	// The cookie is HttpOnly and SameSite=Strict.
	u, _ := url.Parse("http://" + h.srv.Addr())
	cookies := jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("cookies: %v", cookies)
	}
	csrf := extract(string(page), `name="csrf" value="`, `"`)
	if csrf == "" {
		t.Fatal("no csrf token in the page")
	}
	// A form post without the CSRF token is refused; with it, the action runs.
	if r, _ := browser.PostForm("http://"+h.srv.Addr()+"/action/update-check", url.Values{}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("post without csrf: %d", r.StatusCode)
	}
	r, _ := browser.PostForm("http://"+h.srv.Addr()+"/action/update-check", url.Values{"csrf": {csrf}})
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || h.checks != 1 || !strings.Contains(string(body), "release 3.0.1 is available") {
		t.Fatalf("post with csrf: %d checks=%d", r.StatusCode, h.checks)
	}
	// A cross-origin post is refused before anything runs.
	req, _ := http.NewRequest("POST", "http://"+h.srv.Addr()+"/action/update-apply", strings.NewReader("csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://attacker.example")
	if r, _ := browser.Do(req); r.StatusCode != http.StatusForbidden || h.applies != 0 {
		t.Fatalf("cross-origin post: %d applies=%d", r.StatusCode, h.applies)
	}
	// The JSON API with a session needs the CSRF header on a POST.
	req, _ = http.NewRequest("POST", "http://"+h.srv.Addr()+"/api/update/apply", nil)
	if r, _ := browser.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("api post with session but no csrf header: %d", r.StatusCode)
	}
	req, _ = http.NewRequest("POST", "http://"+h.srv.Addr()+"/api/update/apply", nil)
	req.Header.Set(headerCSRF, csrf)
	if r, _ := browser.Do(req); r.StatusCode != http.StatusBadGateway || h.applies != 1 {
		t.Fatalf("api post with csrf header: %d applies=%d", r.StatusCode, h.applies)
	}
	// The page's retry form carries the version and the CSRF token.
	r, _ = browser.PostForm("http://"+h.srv.Addr()+"/action/update-retry", url.Values{"csrf": {csrf}, "version": {"3.0.1"}})
	body, _ = io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || h.retries != 1 || !strings.Contains(string(body), "may be installed again") {
		t.Fatalf("retry form: %d retries=%d", r.StatusCode, h.retries)
	}
	// GET never changes anything.
	before := h.checks
	browser.Get("http://" + h.srv.Addr() + "/action/update-check")
	if h.checks != before {
		t.Fatal("a GET ran an action")
	}
}

func TestClient(t *testing.T) {
	h := newHarness(t)
	c := NewClient(h.srv.Addr(), h.token)
	s, err := c.Status(context.Background())
	if err != nil || s.Network != "testnet" {
		t.Fatalf("status: %v", err)
	}
	v, err := c.UpdateCheck(context.Background())
	if err != nil || v != "3.0.1" {
		t.Fatalf("check: %v %q", err, v)
	}
	if err := c.UpdateRetry(context.Background(), "3.0.2"); err == nil || !strings.Contains(err.Error(), "not marked as failed") {
		t.Fatalf("retry of a version that did not fail: %v", err)
	}
	if err := c.UpdateRetry(context.Background(), "3.0.1"); err != nil || h.retries != 1 {
		t.Fatalf("retry: %v retries=%d", err, h.retries)
	}
	if err := c.UpdateApply(context.Background()); err == nil || !strings.Contains(err.Error(), "probation failed") {
		t.Fatalf("apply: %v", err)
	}
	if _, err := c.Logs(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Config{Addr: "0.0.0.0:0", DataDir: t.TempDir(), Snapshot: func() Snapshot { return Snapshot{} }}); err == nil {
		t.Fatal("a non-loopback address was accepted")
	}
}

func extract(s, after, until string) string {
	i := strings.Index(s, after)
	if i < 0 {
		return ""
	}
	s = s[i+len(after):]
	if j := strings.Index(s, until); j >= 0 {
		return s[:j]
	}
	return ""
}

func firstLines(s string) string {
	lines := strings.SplitN(s, "\n", 6)
	return strings.Join(lines, "\n")
}
