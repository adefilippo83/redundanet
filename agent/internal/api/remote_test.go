package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

func testDeviceKey(t *testing.T) string {
	t.Helper()
	k, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.ID().String()
}

// The view of another device of the member shows what that device said
// and offers its three actions, and nothing of this device's own forms:
// no restore, no setting, no pairing, no phrase.
func TestTheRemoteViewShowsAnotherDeviceAndOffersThreeActions(t *testing.T) {
	far, silent := testDeviceKey(t), testDeviceKey(t)
	var asked int
	var acts []string
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	srv, err := Start(ctx, Config{Addr: "127.0.0.1:0", DataDir: dir, Log: lg,
		Snapshot: func() Snapshot {
			return Snapshot{Version: "3.0.0-near", Network: "testnet", Device: "near", Member: "mem", Roles: []string{"client"},
				Devices: []Device{{Key: far, Name: "falcon", Roles: []string{"storage"}}}, Generated: time.Now()}
		},
		Actions: Actions{
			RemoteView: func(_ context.Context, device string) (*RemoteView, error) {
				if device != far {
					return nil, errors.New("no connection (test)")
				}
				asked++
				return &RemoteView{
					Snapshot: Snapshot{Version: "9.9.9-far", Uptime: "5h", Device: far, Roles: []string{"client", "storage"},
						Update: &update.Status{Enabled: true, Running: "9.9.9-far"},
						Jobs:   &jobs.Status{Sets: []backup.Set{{Name: "docs", Folders: []string{"/srv/docs"}}}, Every: "24h0m0s", VerifyEvery: "168h0m0s"}},
					Logs: []logbuf.Line{{Time: time.Now(), Level: "INFO", Msg: "a line from afar"}},
				}, nil
			},
			RemoteAct: func(_ context.Context, device, action string) error {
				acts = append(acts, device+" "+action)
				return nil
			},
		}})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := ReadClientConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := func(d string) string {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://"+srv.Addr()+"/api/login-link?d="+url.QueryEscape(d), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		var out struct {
			URL string `json:"url"`
		}
		if err := jsonDecode(string(b), &out); err != nil || out.URL == "" {
			t.Fatalf("login link: %s", b)
		}
		return out.URL
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	// The CLI's signed request asks for the same link.
	if l, err := NewClient(srv.Addr(), token).LoginLinkTo(context.Background(), far); err != nil || !strings.HasSuffix(l, "&d="+far) {
		t.Fatalf("the CLI's link for a device: %q %v", l, err)
	}

	// A link made for a device lands on its view.
	r, err := browser.Get(link(far))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	page := string(b)
	at := r.Request.URL
	if r.StatusCode != http.StatusOK || !strings.HasSuffix(at.Path, "/device") || at.Query().Get("d") != far {
		t.Fatalf("the link landed on %s (%d)", at, r.StatusCode)
	}
	home := strings.TrimSuffix(at.Scheme+"://"+at.Host+at.Path, "/device")
	for _, want := range []string{"falcon", "9.9.9-far", "/srv/docs", "a line from afar", `action="action/remote-act"`, `value="backup-run"`, `value="backup-verify"`, `value="update-check"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the remote view lacks %q", want)
		}
	}
	for _, not := range []string{"restore-start", "device-pair", `name="phrase"`, "backup-add", "backup-remove", "action/settings", "update-apply", "diagnostics.zip", "%!"} {
		if strings.Contains(page, not) {
			t.Errorf("the remote view offers %q", not)
		}
	}
	// The page's own refresh within a few seconds does not ask again.
	if r, err := browser.Get(at.String()); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("second read: %v", err)
	}
	if asked != 1 {
		t.Fatalf("the device was asked %d times for two reads", asked)
	}

	csrf := extract(page, `name="csrf" value="`, `"`)
	post := func(v url.Values) *http.Response {
		t.Helper()
		v.Set("csrf", csrf)
		r, err := browser.PostForm(home+"/action/remote-act", v)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r = post(url.Values{"device": {far}, "act": {RemoteBackup}, "back": {"/device?d=" + far}})
	b, _ = io.ReadAll(r.Body)
	if len(acts) != 1 || acts[0] != far+" "+RemoteBackup {
		t.Fatalf("acts: %v", acts)
	}
	if r.Request.URL.Query().Get("d") != far || !strings.Contains(string(b), "falcon") {
		t.Fatalf("after the action the page is at %s", r.Request.URL)
	}
	// Anything but the three actions is not an action, and a back that is
	// not one of the page's own addresses goes to the overview.
	for _, act := range []string{"restore-start", "update-apply", "", "backup-remove"} {
		if r := post(url.Values{"device": {far}, "act": {act}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("act %q: %d", act, r.StatusCode)
		}
	}
	if r := post(url.Values{"device": {"not-a-key"}, "act": {RemoteBackup}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("a device that is no key: %d", r.StatusCode)
	}
	if len(acts) != 1 {
		t.Fatalf("something else was started: %v", acts)
	}
	r = post(url.Values{"device": {far}, "act": {RemoteVerify}, "back": {"/device?d=" + far + "&x=//evil.example"}})
	if got := strings.TrimPrefix(r.Request.URL.String(), home); got != "/" {
		t.Errorf("a back that is not the page's own led to %q", got)
	}

	// A device that does not answer says so, and offers nothing.
	r, _ = browser.Get(home + "/device?d=" + silent)
	b, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(b), "no connection (test)") || strings.Contains(string(b), "remote-act") {
		t.Errorf("the view of a silent device: %s", firstLines(string(b)))
	}
	if r, _ := browser.Get(home + "/device?d=nonsense"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a view without a device key: %d", r.StatusCode)
	}
	// The devices list links to the view.
	r, _ = browser.Get(home + "/network")
	b, _ = io.ReadAll(r.Body)
	if !strings.Contains(string(b), `href="device?d=`+far+`"`) {
		t.Error("the devices list does not link to the device")
	}
	// Without a session the view shows nothing of the device.
	if r, _ := http.Get("http://" + srv.Addr() + "/device?d=" + far); r != nil {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "9.9.9-far") {
			t.Error("the remote view is shown without a session")
		}
	}
	// A link for something that is no device key lands on the overview.
	r, _ = browser.Get(link("../../etc"))
	if strings.Contains(r.Request.URL.Path, "device") {
		t.Errorf("a link for no device landed on %s", r.Request.URL)
	}
}
