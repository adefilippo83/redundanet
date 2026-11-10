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
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

var verb = regexp.MustCompile(`%(\[(\d+)\])?([a-zA-Z%])`)

// verbs lists a text's format verbs by argument position, so a translation
// may reorder them with %[2]s but not drop, add or retype one.
func verbs(t *testing.T, where, text string) string {
	t.Helper()
	var out []string
	next := 1
	for _, m := range verb.FindAllStringSubmatch(text, -1) {
		if m[3] == "%" {
			continue
		}
		pos := next
		if m[2] != "" {
			pos = int(m[2][0] - '0')
		}
		next = pos + 1
		out = append(out, string(rune('0'+pos))+m[3])
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// Every language of the menu has a dictionary with exactly the English
// keys, each with the English format verbs; every key the page uses is in
// the dictionary and the dictionary holds nothing the page does not use.
func TestDictionaries(t *testing.T) {
	en := catalogs["en"]
	for _, l := range Languages {
		cat, ok := catalogs[l.Code]
		if !ok {
			t.Errorf("%s (%s) has no dictionary under locales/", l.Name, l.Code)
			continue
		}
		for k, text := range en {
			got, ok := cat[k]
			switch {
			case !ok:
				t.Errorf("%s lacks %q", l.Code, k)
			case strings.TrimSpace(got) == "":
				t.Errorf("%s has an empty %q", l.Code, k)
			case verbs(t, k, got) != verbs(t, k, text):
				t.Errorf("%s %q has the verbs [%s], English has [%s]", l.Code, k, verbs(t, k, got), verbs(t, k, text))
			case strings.Contains(got, "—"):
				t.Errorf("%s %q has an em dash", l.Code, k)
			}
		}
		for k := range cat {
			if _, ok := en[k]; !ok {
				t.Errorf("%s has %q, which English does not", l.Code, k)
			}
		}
	}
	used := map[string]bool{}
	for _, pat := range []string{"templates/*.html", "*.go"} {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range regexp.MustCompile(`(?:\.T |\.T\(|fail\()"([a-z0-9.]+)"`).FindAllStringSubmatch(string(b), -1) {
				used[m[1]] = true
			}
		}
	}
	for _, v := range views {
		used["nav."+v.Name] = true
	}
	for k := range used {
		if _, ok := en[k]; !ok && k != "nav." {
			t.Errorf("the page uses %q, which the English dictionary lacks", k)
		}
	}
	for k := range en {
		if !used[k] {
			t.Errorf("the English dictionary has %q, which nothing uses", k)
		}
	}
}

func TestLanguageOfARequest(t *testing.T) {
	for header, want := range map[string]string{"": "en", "it-IT,it;q=0.9": "it", "nl,pt-BR;q=0.8": "pt", "zh_CN": "zh", "xx": "en", "*": "en"} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", header)
		if got := langOf(r); got != want {
			t.Errorf("Accept-Language %q: %s, want %s", header, got, want)
		}
	}
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "it")
	r.AddCookie(&http.Cookie{Name: langCookie, Value: "ja"})
	if got := langOf(r); got != "ja" {
		t.Errorf("the page's own choice comes first: %s", got)
	}
	r, _ = http.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: langCookie, Value: "../../etc"})
	if got := langOf(r); got != "en" {
		t.Errorf("a made-up cookie: %s", got)
	}
	if got := tr("en").T("no.such.key"); got != "no.such.key" {
		t.Errorf("an unknown key: %q", got)
	}
	if got := tr("en").Bytes(1_219_734); got != "1.2 MB" {
		t.Errorf("bytes: %q", got)
	}
}

// The overview's one sentence, case by case, and what it lists as needing
// the member.
func TestOverviewVerdict(t *testing.T) {
	now := time.Now()
	done := jobs.RunStatus{Set: "docs", LastAt: now.Add(-2 * time.Hour), Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 10, Bytes: 2000}}}
	set := []backup.Set{{Name: "docs", Folders: []string{"/d"}}}
	titles := func(o overviewData) string {
		var out []string
		for _, i := range o.Issues {
			out = append(out, i.Kind+":"+i.Title)
		}
		return strings.Join(out, "; ")
	}
	for name, c := range map[string]struct {
		snap   Snapshot
		online string
		state  string
		issues string
	}{
		"a hub or a node without jobs": {Snapshot{}, "", "node", ""},
		"no folder yet":                {Snapshot{Jobs: &jobs.Status{}}, "up", "empty", ""},
		"first backup still to run":    {Snapshot{Jobs: &jobs.Status{Sets: set}}, "up", "waiting", ""},
		"backed up":                    {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{done}}}, "up", "protected", ""},
		"running wins":                 {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{{Set: "docs", Running: true, LastErr: "x"}}}}, "up", "running", "bad:The last backup of docs failed"},
		"last run failed":              {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{{Set: "docs", LastAt: now, LastErr: "no share could be stored"}}}}, "up", "attention", "bad:The last backup of docs failed"},
		"offline":                      {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{done}}}, "down", "attention", "bad:No connection to the network"},
		"space used up":                {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{done}, Budget: &jobs.Budget{AllowedGB: 5, Exhausted: true}}}, "up", "attention", "bad:Your space is used up"},
		"restore test failed":          {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{done}, Verifications: []localdb.Verification{{At: now, Sampled: 20, Verified: 19, Failed: []string{"x"}}}}}, "up", "attention", "bad:The last restore test failed"},
		"not listed yet, nothing run":  {Snapshot{Admission: "waiting", Jobs: &jobs.Status{Sets: set}}, "up", "waiting", "warn:You are not a listed member yet"},
		"an update does not alarm": {Snapshot{Jobs: &jobs.Status{Sets: set, Runs: []jobs.RunStatus{done}}, Update: &update.Status{Available: "3.1.0", Failed: []string{"3.0.9"}},
			Membership: &membership.Status{Expired: true}}, "up", "protected", "warn:The member list's signature has expired; warn:Version 3.0.9 could not be installed; info:Version 3.1.0 is ready"},
	} {
		o := buildOverview(c.snap, c.online, tr("en"))
		if o.State != c.state || titles(o) != c.issues || o.Title == "" || o.Sub == "" {
			t.Errorf("%s: state %s issues [%s], want %s [%s]", name, o.State, titles(o), c.state, c.issues)
		}
	}
	// Two sets: each card shows its own run, and the overview adds them up.
	d := buildPage(Snapshot{Jobs: &jobs.Status{Every: "24h0m0s", VerifyEvery: "90h0m0s", CleanupEvery: "168h0m0s",
		Sets: []backup.Set{{Name: "a", Folders: []string{"/a"}, Excludes: []string{"*.iso", "tmp/"}}, {Name: "b", Folders: []string{"/b"}}},
		Runs: []jobs.RunStatus{{Set: "b", LastAt: now, Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 5, Bytes: 10}}}, {Set: "a", LastAt: now.Add(-time.Hour), Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 7, Bytes: 20}}}}},
		Transport: &transport.Status{Hubs: []transport.HubStatus{{Hub: "h", Up: false}, {Hub: "i", Up: true}}}}, views[1], tr("en"))
	if len(d.Sets) != 2 || d.Sets[0].Run == nil || d.Sets[0].Run.Last.Snapshot.Files != 7 || d.Sets[1].Run.Last.Snapshot.Files != 5 || d.Sets[0].Excluded != "*.iso, tmp/" {
		t.Fatalf("sets and their runs: %+v", d.Sets)
	}
	if d.Overview.Files != 12 || d.Overview.Bytes != 30 || !d.Overview.LastBackup.Equal(now) || d.Online != "up" {
		t.Fatalf("overview totals: %+v online %s", d.Overview, d.Online)
	}
	// A schedule set from the command line to something the menu lacks stays selectable.
	if n := len(d.Verify); n != 4 || !d.Verify[3].Selected || d.Verify[3].Value != "90h0m0s" || !d.Every[3].Selected || d.Every[3].Label != "every day" {
		t.Fatalf("schedule menus: %+v %+v", d.Every, d.Verify)
	}
}

// Adding and removing a folder from the page, in another language, with a
// real runner behind it.
func TestFoldersFromThePage(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Start(ctx, Config{Addr: "127.0.0.1:0", DataDir: dir, Jobs: runner, Snapshot: func() Snapshot { return Snapshot{Jobs: ptr(runner.Status())} }})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, tokenFile))
	link, err := NewClient(srv.Addr(), strings.TrimSpace(string(tok))).LoginLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	var base string // the session's own address, known after the login
	read := func(r *http.Response, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "%!") {
			t.Fatalf("a text was formatted with the wrong arguments: %s", b)
		}
		return string(b)
	}
	page, home := openPage(t, browser, link)
	base = home
	if !strings.Contains(page, "Nothing is backed up yet") || !strings.Contains(page, `href="backups"`) {
		t.Fatalf("overview without a folder: %s", firstLines(page))
	}
	csrf := extract(page, `name="csrf" value="`, `"`)
	post := func(action string, v url.Values) string {
		t.Helper()
		v.Set("csrf", csrf)
		v.Set("back", "/backups")
		return read(browser.PostForm(base+"/action/"+action, v))
	}
	src := filepath.Join(t.TempDir(), "photos")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if page = post("backup-add", url.Values{"name": {"photos"}, "folder": {filepath.Join(src, "missing")}}); !strings.Contains(page, "The folder was not added") || len(runner.Status().Sets) != 0 {
		t.Fatalf("a folder that does not exist: %s", firstLines(page))
	}
	if page = post("backup-add", url.Values{"name": {" "}, "folder": {src}}); !strings.Contains(page, "A name and a folder are both needed.") {
		t.Fatalf("a set without a name: %s", firstLines(page))
	}
	if page = post("backup-add", url.Values{"name": {"photos"}, "folder": {src}}); !strings.Contains(page, "photos is backed up from now on") || !strings.Contains(page, src) {
		t.Fatalf("adding a folder: %s", firstLines(page))
	}
	// The form adds; it never replaces a set of the same name.
	other := t.TempDir()
	if page = post("backup-add", url.Values{"name": {"photos"}, "folder": {other}}); !strings.Contains(page, "is backed up already") {
		t.Fatalf("the same name twice: %s", firstLines(page))
	}
	if sets := runner.Status().Sets; len(sets) != 1 || sets[0].Folders[0] != src {
		t.Fatalf("sets after the forms: %+v", sets)
	}
	if page = post("settings-excludes", url.Values{"set": {"photos"}, "excludes": {" *.raw , cache/ ,"}}); !strings.Contains(page, "Saved.") || strings.Join(runner.Status().Sets[0].Excludes, "|") != "*.raw|cache/" {
		t.Fatalf("excludes: %+v", runner.Status().Sets)
	}
	if page = post("settings-schedule", url.Values{"every": {"6h0m0s"}, "verify_every": {"24h0m0s"}, "cleanup_every": {"720h0m0s"}}); !strings.Contains(page, "Saved.") || runner.Status().Every != "6h0m0s" || runner.Status().CleanupEvery != "720h0m0s" {
		t.Fatalf("schedule from the menus: %+v", runner.Status())
	}
	// In Italian from here: the message and the page follow the cookie.
	post("language", url.Values{"lang": {"it"}})
	page = post("backup-remove", url.Values{"set": {"photos"}})
	if it := catalogs["it"]; it != nil && !strings.Contains(page, strings.SplitN(it["msg.remove.done"], "%s", 2)[1]) {
		t.Fatalf("removing a folder, in Italian: %s", firstLines(page))
	}
	if len(runner.Status().Sets) != 0 {
		t.Fatalf("sets after the removal: %+v", runner.Status().Sets)
	}
	if page = post("no-such-action", url.Values{}); !strings.Contains(page, "404") {
		t.Fatalf("an unknown action: %s", firstLines(page))
	}
	var st Snapshot
	req, _ := http.NewRequest("GET", base+"/api/status", nil)
	if err := json.Unmarshal([]byte(read(browser.Do(req))), &st); err != nil || st.Jobs == nil {
		t.Fatalf("the JSON API with the page's session: %v", err)
	}
}

// Pairing a device from the page: the code and the words reach the action
// and nothing else; the words are in no page, no message and no log line.
func TestPairingFromThePage(t *testing.T) {
	const phrase = "alpha bravo charlie delta echo foxtrot golf hotel india juliett kilo lima"
	dir := t.TempDir()
	logs := logbuf.New(slog.NewTextHandler(io.Discard, nil), 200)
	var gotCode, gotPhrase string
	var gotStorage bool
	fail := true
	paired := []Device{{Key: "ne6ftzx2aaaaaaaaaaaa", Name: "falcon", Roles: []string{"client", "storage"}, PairedAt: time.Now().Add(-3 * time.Hour)}, {Key: "ikpijbsqaaaaaaaaaaaa", Roles: []string{"client"}, PairedAt: time.Now()}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Start(ctx, Config{Addr: "127.0.0.1:0", DataDir: dir, Logs: logs, Log: slog.New(logs),
		Snapshot: func() Snapshot {
			return Snapshot{Device: "dev", Member: "mem", Admission: "listed", Devices: paired, JoinURL: "https://redundanet.com/join.html?mode=device&devicename=falcon", JoinDevice: "falcon",
				Transport: &transport.Status{Sessions: []transport.SessionStatus{{Peer: "ne6ftzx2aaaaaaaaaaaa", Relay: true, Direct: "192.168.1.4:5"}}}}
		},
		Actions: Actions{PairDevice: func(_ context.Context, code, words string, storage bool) (Device, error) {
			gotCode, gotPhrase, gotStorage = code, words, storage
			if fail {
				return Device{}, errors.New("the new device did not answer")
			}
			return Device{Key: "abcdefghijklmnop", Roles: []string{"client"}}, nil
		}}})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, tokenFile))
	link, err := NewClient(srv.Addr(), strings.TrimSpace(string(tok))).LoginLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	var base string // the session's own address, known after the login
	read := func(r *http.Response, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "%!") || strings.Contains(string(b), "foxtrot") {
			t.Fatalf("a page with a broken text or with the words: %s", firstLines(string(b)))
		}
		return string(b)
	}
	page, home := openPage(t, browser, link)
	base = home
	csrf := extract(page, `name="csrf" value="`, `"`)
	// The overview says the paired storage node still needs its request.
	if !strings.Contains(page, "Storage device falcon is not in the member list yet") || !strings.Contains(page, "join.html?mode=device&amp;devicename=falcon") {
		t.Fatalf("overview: %s", firstLines(page))
	}
	page = read(browser.Get(base + "/network"))
	for _, want := range []string{"falcon", "client, storage", ">direct<", "ikpijbsq", ">not connected<", `action="action/device-pair"`, `name="phrase"`, `autocomplete="off"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the devices view lacks %q", want)
		}
	}
	post := func(v url.Values) string {
		t.Helper()
		v.Set("back", "/network")
		return read(browser.PostForm(base+"/action/device-pair", v))
	}
	// No token, no action; half a form, no action.
	if r, _ := browser.PostForm(base+"/action/device-pair", url.Values{"code": {"rn3:pair:x"}, "phrase": {phrase}}); r.StatusCode != http.StatusForbidden || gotPhrase != "" {
		t.Fatalf("pairing without the CSRF token: %d", r.StatusCode)
	}
	if p := post(url.Values{"csrf": {csrf}, "code": {"rn3:pair:x"}, "phrase": {"  "}}); !strings.Contains(p, "The code and the twelve words are both needed.") || gotCode != "" {
		t.Fatalf("half a form: %s", firstLines(p))
	}
	if p := post(url.Values{"csrf": {csrf}, "code": {" rn3:pair:x "}, "phrase": {phrase}, "storage": {"on"}}); !strings.Contains(p, "The device was not paired: the new device did not answer") {
		t.Fatalf("a failed pairing: %s", firstLines(p))
	}
	if gotCode != "rn3:pair:x" || gotPhrase != phrase || !gotStorage {
		t.Fatalf("what the action got: %q %v", gotCode, gotStorage)
	}
	fail = false
	if p := post(url.Values{"csrf": {csrf}, "code": {"rn3:pair:y"}, "phrase": {phrase}}); !strings.Contains(p, "abcdefgh is paired.") || gotStorage {
		t.Fatalf("a pairing: %s", firstLines(p))
	}
	for _, line := range logs.Last(200) {
		if strings.Contains(line.Msg+line.Attrs, "foxtrot") {
			t.Fatalf("the words are in the log: %s %s", line.Msg, line.Attrs)
		}
	}
	// A device without the action (a hub, a build without it) says so.
	srv.Attach(Runtime{Snapshot: func() Snapshot { return Snapshot{} }})
	if p := post(url.Values{"csrf": {csrf}, "code": {"c"}, "phrase": {phrase}}); !strings.Contains(p, "Pairing from the page is off on this device.") {
		t.Fatalf("pairing where it is off: %s", firstLines(p))
	}
}

// The page lives under the session's own address, so a link or a form of
// its own must be relative: one that starts with a slash would leave the
// session and reach the page that says to run `redundanet open`. Only the
// stylesheet and the script, served without a session, are absolute.
func TestThePagesOwnLinksAreRelative(t *testing.T) {
	re := regexp.MustCompile(`(?:href|action|src)="(/[^"]*)"`)
	for _, name := range []string{"templates/page.html", "templates/setup.html"} {
		b, err := templates.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if !strings.HasPrefix(m[1], "/static/") {
				t.Errorf("%s: %s is absolute", name, m[0])
			}
		}
	}
	if h := restoreHref("d", "s", "", ""); strings.HasPrefix(h, "/") {
		t.Errorf("restore link %s is absolute", h)
	}
}
