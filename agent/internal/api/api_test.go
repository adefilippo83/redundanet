package api

import (
	"context"
	"encoding/json"
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
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

type harness struct {
	revoked []*identity.SignedRevocation
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
			Revoke: func(_ context.Context, r *identity.SignedRevocation) (RevokeReport, error) {
				h.revoked = append(h.revoked, r)
				return RevokeReport{Servers: 2, ServersTotal: 2, Hubs: 1, HubsTotal: 1}, nil
			},
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
	// The link led to the session's own address, which carries its key.
	home := strings.TrimSuffix(resp.Request.URL.String(), "/")
	homePath := strings.TrimSuffix(resp.Request.URL.Path, "/")
	if !strings.HasPrefix(homePath, keyedPrefix) || len(homePath) != len(keyedPrefix)+32 {
		t.Fatalf("the page after login is at %s", resp.Request.URL)
	}
	get := func(path string) string {
		t.Helper()
		r, err := browser.Get(home + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != http.StatusOK || strings.Contains(string(b), "%!") {
			t.Fatalf("%s: %d %s", path, r.StatusCode, firstLines(string(b)))
		}
		return string(b)
	}
	want := func(where, text string, all ...string) {
		t.Helper()
		for _, w := range all {
			if !strings.Contains(text, w) {
				t.Fatalf("%s lacks %q: %s", where, w, firstLines(text))
			}
		}
	}
	// The overview says how the backup stands and what needs the member:
	// the folder macOS guards, the update to install, the failed one.
	if resp.StatusCode != http.StatusOK || strings.Contains(string(page), "%!") {
		t.Fatalf("page after login: %d %s", resp.StatusCode, firstLines(string(page)))
	}
	want("overview", string(page), "testnet", "Your backup needs attention", "grant Full Disk Access", "Install 3.0.1", "Version 3.0.1 could not be installed",
		"Weeks in a row passing: 2", "20 of 20 sample files read back", `lang="en"`, `aria-current="page"`)
	want("backups", get("/backups"), "/home/x", "(1 not on this computer)", "20 of 20", "Failed (1)", `action="action/backup-add"`, `action="action/backup-remove"`, "settings-excludes")
	want("network", get("/network"), "peerpeer", "relay connection up")
	want("settings", get("/settings"), "settings-schedule", `<option value="24h0m0s" selected>every day</option>`, `<option value="168h0m0s" selected>every week</option>`, "Try 3.0.1 again")
	want("advanced", get("/advanced"), "repairing abcdefgh", "surplus pieces released: 3", "hello from the test", `href="diagnostics.zip"`)
	if r, _ := browser.Get(home + "/nowhere"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown path: %d", r.StatusCode)
	}
	// The policy lets the page load its own stylesheet and script and
	// nothing else, inline included; both are served without a session.
	if csp := resp.Header.Get("Content-Security-Policy"); csp != contentPolicy || strings.Contains(csp, "unsafe") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("content policy: %q", csp)
	}
	for _, tag := range []string{"<style", " style=", "<script>", "onclick=", "javascript:"} {
		for _, path := range []string{"/", "/backups", "/network", "/settings", "/advanced"} {
			if strings.Contains(strings.ToLower(get(path)), tag) {
				t.Fatalf("%s has %q, which the content policy forbids", path, tag)
			}
		}
	}
	for path, typ := range map[string]string{"/static/app.css": "text/css", "/static/app.js": "text/javascript"} {
		r := h.req("GET", path, nil, "")
		if b, _ := io.ReadAll(r.Body); r.StatusCode != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), typ) || len(b) < 500 {
			t.Fatalf("%s: %d %s", path, r.StatusCode, r.Header.Get("Content-Type"))
		}
	}
	if r := h.req("GET", "/static/../server.go", nil, ""); r.StatusCode == http.StatusOK {
		t.Fatal("the static handler served something else")
	}
	// The link is single use.
	if r, _ := http.Get(link); r.StatusCode != http.StatusForbidden {
		t.Fatalf("reused link: %d", r.StatusCode)
	}
	// The cookie is HttpOnly and SameSite=Strict.
	u, _ := url.Parse(home + "/")
	cookies := jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("cookies: %v", cookies)
	}
	// The browser sends the cookie to the session's address only: not to
	// the root of this port, and so not to any other port of this host,
	// which it would reach with a cookie for the whole host.
	root, _ := url.Parse("http://" + h.srv.Addr() + "/")
	if got := jar.Cookies(root); len(got) != 0 {
		t.Fatalf("cookies sent to the root: %v", got)
	}
	// The cookie alone is no session: at the root, and under another key,
	// it opens the page that says to run `redundanet open`, and no API.
	for _, path := range []string{"/", "/backups", "/api/status", "/diagnostics.zip", keyedPrefix + strings.Repeat("0", 32) + "/", keyedPrefix + strings.Repeat("0", 32) + "/api/status"} {
		req, _ := http.NewRequest("GET", "http://"+h.srv.Addr()+path, nil)
		req.AddCookie(cookies[0])
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != http.StatusUnauthorized || strings.Contains(string(b), `name="csrf"`) || strings.Contains(string(b), "testnet") {
			t.Fatalf("the cookie without the key at %s: %d %s", path, r.StatusCode, firstLines(string(b)))
		}
	}
	// The key alone is none either.
	if r, _ := http.Get(home + "/"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the address without the cookie: %d", r.StatusCode)
	}
	csrf := extract(string(page), `name="csrf" value="`, `"`)
	if csrf == "" {
		t.Fatal("no csrf token in the page")
	}
	// A form post without the CSRF token is refused; with it, the action runs.
	if r, _ := browser.PostForm(home+"/action/update-check", url.Values{}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("post without csrf: %d", r.StatusCode)
	}
	r, _ := browser.PostForm(home+"/action/update-check", url.Values{"csrf": {csrf}})
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || h.checks != 1 || !strings.Contains(string(body), "Version 3.0.1 is available.") {
		t.Fatalf("post with csrf: %d checks=%d", r.StatusCode, h.checks)
	}
	// The message of an action shows once; the page's own refresh, which
	// reads the view again every few seconds, does not use it up.
	r, _ = browser.PostForm(home+"/action/update-check", url.Values{"csrf": {csrf}, "back": {"/settings"}})
	if body, _ = io.ReadAll(r.Body); r.Request.URL.Path != homePath+"/settings" || !strings.Contains(string(body), "Version 3.0.1 is available.") {
		t.Fatalf("an action returns to its view: %s", r.Request.URL.Path)
	}
	r, _ = browser.PostForm(home+"/action/update-check", url.Values{"csrf": {csrf}, "back": {"https://attacker.example/"}})
	if r.Request.URL.Host != h.srv.Addr() || r.Request.URL.Path != homePath+"/" {
		t.Fatalf("an action redirected off the page: %s", r.Request.URL)
	}
	stay := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r, _ = stay.PostForm(home+"/action/update-check", url.Values{"csrf": {csrf}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("an action answers with a redirect: %d", r.StatusCode)
	}
	live, _ := http.NewRequest("GET", home+"/", nil)
	live.Header.Set(liveHeader, "1")
	r, _ = browser.Do(live)
	if body, _ = io.ReadAll(r.Body); !strings.Contains(string(body), "Version 3.0.1 is available.") {
		t.Fatal("the refresh did not show the message")
	}
	if !strings.Contains(get("/"), "Version 3.0.1 is available.") || strings.Contains(get("/"), "Version 3.0.1 is available.") {
		t.Fatal("the message of an action is shown once, after any refresh")
	}
	// The language is the browser's, or the one chosen on the page.
	de, _ := http.NewRequest("GET", home+"/", nil)
	de.Header.Set("Accept-Language", "de-CH,de;q=0.9,en;q=0.8")
	r, _ = browser.Do(de)
	if body, _ = io.ReadAll(r.Body); !strings.Contains(string(body), `<html lang="de">`) {
		t.Fatalf("Accept-Language: %s", firstLines(string(body)))
	}
	r, _ = browser.PostForm(home+"/action/language", url.Values{"csrf": {csrf}, "lang": {"it"}, "back": {"/backups"}})
	if body, _ = io.ReadAll(r.Body); r.Request.URL.Path != homePath+"/backups" || !strings.Contains(string(body), `<html lang="it">`) {
		t.Fatalf("language form: %s %s", r.Request.URL.Path, firstLines(string(body)))
	}
	browser.PostForm(home+"/action/language", url.Values{"csrf": {csrf}, "lang": {"xx"}})
	r, _ = browser.PostForm(home+"/action/language", url.Values{"csrf": {csrf}, "lang": {"en"}})
	if body, _ = io.ReadAll(r.Body); !strings.Contains(string(body), `<html lang="en">`) {
		t.Fatal("back to English")
	}
	// What a browser sends with the page's own form depends on the page's
	// referrer policy: under no-referrer it is "Origin: null", which the
	// origin check refuses, so the policy must not be that, and a post
	// that says null stays refused.
	if rp := resp.Header.Get("Referrer-Policy"); rp != "same-origin" {
		t.Fatalf("referrer policy %q: the page's own forms need same-origin", rp)
	}
	req0, _ := http.NewRequest("POST", home+"/action/update-apply", strings.NewReader("csrf="+csrf))
	req0.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req0.Header.Set("Origin", "null")
	if r, _ := browser.Do(req0); r.StatusCode != http.StatusForbidden || h.applies != 0 {
		t.Fatalf("a post with a null origin: %d applies=%d", r.StatusCode, h.applies)
	}
	same, _ := http.NewRequest("POST", home+"/action/update-check", strings.NewReader("csrf="+csrf))
	same.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	same.Header.Set("Origin", "http://"+h.srv.Addr())
	if r, _ := browser.Do(same); r.StatusCode != http.StatusOK {
		t.Fatalf("a post from the page's own origin: %d", r.StatusCode)
	}
	// A cross-origin post is refused before anything runs.
	req, _ := http.NewRequest("POST", home+"/action/update-apply", strings.NewReader("csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://attacker.example")
	if r, _ := browser.Do(req); r.StatusCode != http.StatusForbidden || h.applies != 0 {
		t.Fatalf("cross-origin post: %d applies=%d", r.StatusCode, h.applies)
	}
	// The JSON API with a session needs the CSRF header on a POST.
	req, _ = http.NewRequest("POST", home+"/api/update/apply", nil)
	if r, _ := browser.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("api post with session but no csrf header: %d", r.StatusCode)
	}
	req, _ = http.NewRequest("POST", home+"/api/update/apply", nil)
	req.Header.Set(headerCSRF, csrf)
	if r, _ := browser.Do(req); r.StatusCode != http.StatusBadGateway || h.applies != 1 {
		t.Fatalf("api post with csrf header: %d applies=%d", r.StatusCode, h.applies)
	}
	// The page's retry form carries the version and the CSRF token.
	r, _ = browser.PostForm(home+"/action/update-retry", url.Values{"csrf": {csrf}, "version": {"3.0.1"}})
	body, _ = io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || h.retries != 1 || !strings.Contains(string(body), "Version 3.0.1 is tried again at the next check.") {
		t.Fatalf("retry form: %d retries=%d", r.StatusCode, h.retries)
	}
	// GET never changes anything.
	before := h.checks
	browser.Get(home + "/action/update-check")
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

// TestRevokeThroughTheAPI: the CLI hands the running agent a signed record;
// the action applies and spreads it and the report comes back. A record
// that does not verify never reaches the action.
func TestRevokeThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	c := NewClient(h.srv.Addr(), h.token)
	var seed identity.Seed
	seed[0] = 5
	dev, _ := identity.NewDeviceKey()
	rv, err := identity.Revoke(seed, identity.Revocation{Network: "testnet", Device: dev.ID()})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := c.Revoke(context.Background(), rv)
	if err != nil || rep.Servers != 2 || rep.Hubs != 1 || len(h.revoked) != 1 || h.revoked[0].Revocation.Device != dev.ID() {
		t.Fatalf("revoke: %v %+v, %d records", err, rep, len(h.revoked))
	}
	bad := *rv
	bad.Signature = append([]byte{}, rv.Signature...)
	bad.Signature[0] ^= 1
	if _, err := c.Revoke(context.Background(), &bad); err == nil || len(h.revoked) != 1 {
		t.Fatalf("a tampered record was accepted: %v", err)
	}
}

// TestSettingsThroughTheAPI: the settings endpoint changes the schedule,
// the retention, the caps and a set's excludes on a real runner, refuses
// what the runner refuses, and the channel choice reaches the action.
func TestSettingsThroughTheAPI(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	dir := t.TempDir()
	db, err := localdb.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner, err := jobs.New(jobs.Config{DataDir: dir, Engine: engine.NewFake(member, device, 2, 4), DB: db, Member: member, Device: device})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "docs")
	_ = os.MkdirAll(src, 0o755)
	if err := runner.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	var channels []string
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Start(ctx, Config{Addr: "127.0.0.1:0", DataDir: dir, Jobs: runner, Snapshot: func() Snapshot { return Snapshot{Jobs: ptr(runner.Status())} },
		Actions: Actions{SetChannel: func(c string) error { channels = append(channels, c); return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, tokenFile))
	c := NewClient(srv.Addr(), strings.TrimSpace(string(tok)))
	every, verify, up := "6h", "48h", 1500
	if err := c.Settings(ctx, Settings{Every: &every, VerifyEvery: &verify, UploadKBps: &up, Retention: &snapshot.Retention{Days: 2, Daily: 5, Weekly: 2, Monthly: 1}}); err != nil {
		t.Fatal(err)
	}
	st := runner.Status()
	if st.Every != "6h0m0s" || st.VerifyEvery != "48h0m0s" || st.CleanupEvery != "168h0m0s" || st.UploadKBps != 1500 || st.Retention.Days != 2 {
		t.Fatalf("settings: %+v", st)
	}
	bad := "10m"
	if err := c.Settings(ctx, Settings{Every: &bad}); err == nil || !strings.Contains(err.Error(), "hourly") {
		t.Fatalf("a ten-minute schedule: %v", err)
	}
	if err := c.BackupExcludes(ctx, "docs", []string{"*.iso"}); err != nil || len(runner.Status().Sets[0].Excludes) != 1 {
		t.Fatalf("excludes: %v %+v", err, runner.Status().Sets)
	}
	if err := c.UpdateChannel(ctx, "early"); err != nil || len(channels) != 1 || channels[0] != "early" {
		t.Fatalf("channel: %v %v", err, channels)
	}
}

func ptr[T any](v T) *T { return &v }

// Setup mode: before the device has an identity the page is the wizard,
// the API answers with the wizard's state, and once the runtime is
// attached the same browser session sees the running agent.
func TestSetupModeThenAttach(t *testing.T) {
	dir := t.TempDir()
	buf := logbuf.New(slog.NewTextHandler(io.Discard, nil), 10)
	lg := slog.New(buf)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	phase, errMsg := "choose", ""
	var confirmed []string
	srv, err := Start(ctx, Config{Addr: "127.0.0.1:0", DataDir: dir, Version: "3.0.9-test", Log: lg, Setup: &SetupActions{
		Begin: func(storage bool) ([]string, []int, error) {
			phase = "confirm"
			return []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}, []int{2, 5, 11}, nil
		},
		Confirm: func(answers []string) error {
			confirmed = answers
			if answers[0] != "bravo" {
				return errors.New("word 2 is not what the phrase says")
			}
			phase = "done"
			return nil
		},
		Restore: func(string, bool) error { return nil },
		Pair:    func() (string, error) { phase = "pairing"; return "rn3:pair:abc", nil },
		Cancel:  func() { phase = "choose" },
		Status: func() SetupStatus {
			s := SetupStatus{Phase: phase, Error: errMsg}
			if phase == "confirm" {
				s.Check = []int{2, 5, 11}
			}
			if phase == "pairing" {
				s.Code = "rn3:pair:abc"
			}
			return s
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, tokenFile))
	token := strings.TrimSpace(string(tok))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + srv.Addr()
	do := func(req *http.Request) *http.Response {
		t.Helper()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	body := func(resp *http.Response) string {
		t.Helper()
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// The CLI's status in setup mode says where the wizard stands.
	req, _ := http.NewRequest("GET", base+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	st := body(do(req))
	if !strings.Contains(st, `"phase": "choose"`) || !strings.Contains(st, `"version": "3.0.9-test"`) {
		t.Fatalf("setup status: %s", st)
	}
	// A runtime endpoint before attach says so instead of panicking.
	req, _ = http.NewRequest("GET", base+"/api/backups", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp := do(req); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("backups before attach: %d", resp.StatusCode)
	}

	// Log in through the one-time link, as `redundanet open` does.
	req, _ = http.NewRequest("POST", base+"/api/login-link", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var link struct{ URL string }
	if err := jsonDecode(body(do(req)), &link); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("GET", link.URL, nil)
	resp := do(req)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	// From here on the page is at the session's own address.
	home := base + strings.TrimSuffix(resp.Header.Get("Location"), "/")
	req, _ = http.NewRequest("GET", home+"/", nil)
	page := body(do(req))
	if !strings.Contains(page, "I am new here") || !strings.Contains(page, `action="setup/begin"`) {
		t.Fatalf("setup page: %s", page)
	}
	csrf := page[strings.Index(page, `name="csrf" value="`)+len(`name="csrf" value="`):]
	csrf = csrf[:strings.Index(csrf, `"`)]

	// Begin: the words show once, then not again.
	form := url.Values{"csrf": {csrf}, "storage": {"on"}}
	req, _ = http.NewRequest("POST", home+"/setup/begin", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp := do(req); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("begin: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", home+"/", nil)
	page = body(do(req))
	if !strings.Contains(page, "foxtrot") || !strings.Contains(page, "Word 2") || !strings.Contains(page, "Word 11") {
		t.Fatalf("confirm page with words: %s", page)
	}
	req, _ = http.NewRequest("GET", home+"/", nil)
	page = body(do(req))
	if strings.Contains(page, "foxtrot") || !strings.Contains(page, "shown once") {
		t.Fatalf("confirm page after reload still shows the phrase: %s", page)
	}
	// A wrong word is reported; the right ones end the wizard.
	for _, words := range [][]string{{"wrong", "foxtrot", "lima"}, {"bravo", "foxtrot", "lima"}} {
		form = url.Values{"csrf": {csrf}, "w1": {words[0]}, "w2": {words[1]}, "w3": {words[2]}}
		req, _ = http.NewRequest("POST", home+"/setup/confirm", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		do(req)
	}
	if len(confirmed) != 3 || confirmed[0] != "bravo" {
		t.Fatalf("confirmed %v", confirmed)
	}
	req, _ = http.NewRequest("GET", home+"/", nil)
	if page = body(do(req)); !strings.Contains(page, "Identity written") {
		t.Fatalf("done page: %s", page)
	}
	// Without the CSRF token nothing moves.
	req, _ = http.NewRequest("POST", home+"/setup/cancel", strings.NewReader("csrf=nope"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp := do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cancel without csrf: %d", resp.StatusCode)
	}

	// The runtime arrives; the session from setup still works and the
	// wizard's forms are gone.
	srv.Attach(Runtime{Snapshot: func() Snapshot {
		return Snapshot{Version: "3.0.9-test", Network: "testnet", Device: "dev", Member: "mem", Admission: "waiting", Generated: time.Now()}
	}})
	req, _ = http.NewRequest("GET", home+"/", nil)
	page = body(do(req))
	if !strings.Contains(page, "You are not a listed member yet") || strings.Contains(page, `action="setup/begin"`) {
		t.Fatalf("page after attach: %s", page)
	}
	form = url.Values{"csrf": {csrf}}
	req, _ = http.NewRequest("POST", home+"/setup/begin", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp := do(req); resp.StatusCode != http.StatusConflict {
		t.Fatalf("begin after attach: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", base+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if st = body(do(req)); strings.Contains(st, `"setup"`) || !strings.Contains(st, `"admission": "waiting"`) {
		t.Fatalf("status after attach: %s", st)
	}
}

func jsonDecode(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// openPage follows a login link as the browser does and returns the page
// and the session's own address, under which the page lives.
func openPage(t *testing.T, browser *http.Client, link string) (page, home string) {
	t.Helper()
	r, err := browser.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || strings.Contains(string(b), "%!") {
		t.Fatalf("the page after login: %d %s", r.StatusCode, firstLines(string(b)))
	}
	return string(b), strings.TrimSuffix(r.Request.URL.String(), "/")
}
