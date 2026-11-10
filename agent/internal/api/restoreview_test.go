package api

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// A restore from the page, link by link: the version, the backed up
// folder, a folder below it, one file; then the form, for the file and for
// the folder, and what lands on disk.
func TestRestoreFromThePage(t *testing.T) {
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
	for name, body := range map[string]string{"notes.txt": "hello", "taxes/2025 return.pdf": "pdf bytes", "taxes/receipts/a&b.txt": "receipt"} {
		p := filepath.Join(src, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	src = runner.Status().Sets[0].Folders[0]
	if rs := runner.RunSet(context.Background(), runner.Status().Sets[0], snapshot.DefaultRetention); rs == nil || rs.LastErr != "" {
		t.Fatalf("backup: %+v", rs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Start(ctx, Config{Addr: "127.0.0.1:0", DataDir: dir, Jobs: runner, Snapshot: func() Snapshot { return Snapshot{Device: device.String(), Jobs: ptr(runner.Status())} }})
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
	// follow finds the link whose text starts with name and opens it.
	follow := func(page, name string) string {
		t.Helper()
		m := regexp.MustCompile(`<a href="(restore\?[^"]+)"[^>]*>` + regexp.QuoteMeta(html.EscapeString(name))).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no link to %q in: %s", name, page[strings.Index(page, "<main>"):])
		}
		return read(browser.Get(base + "/" + html.UnescapeString(m[1])))
	}
	page, home := openPage(t, browser, link)
	base = home
	csrf := extract(page, `name="csrf" value="`, `"`)
	if !strings.Contains(page, `href="restore"`) {
		t.Fatal("no Restore in the navigation")
	}
	page = read(browser.Get(base + "/restore"))
	if !strings.Contains(page, "This device") || !strings.Contains(page, ">docs<") || strings.Contains(page, "restore-start") {
		t.Fatalf("the versions: %s", page[strings.Index(page, "<main>"):])
	}
	// The page's own refresh follows a restore and reads nothing else.
	live, _ := http.NewRequest("GET", base+"/restore", nil)
	live.Header.Set(liveHeader, "1")
	if p := read(browser.Do(live)); strings.Contains(p, ">docs<") || !strings.Contains(p, `id="restoring"`) {
		t.Fatal("the refresh of the restore view listed the versions again")
	}
	page = follow(page, time.Now().Format("2006-01-02"))
	if !strings.Contains(page, "Restore everything in this version") {
		t.Fatalf("the top of a version: %s", page[strings.Index(page, "<main>"):])
	}
	page = follow(page, src)
	if !strings.Contains(page, "notes.txt") || !strings.Contains(page, "Restore the folder "+html.EscapeString(src)) {
		t.Fatalf("the backed up folder: %s", page[strings.Index(page, "<main>"):])
	}
	taxes := follow(page, "taxes")
	if !strings.Contains(taxes, "2025 return.pdf") || !strings.Contains(taxes, "Restore the folder taxes") || !strings.Contains(taxes, "Files: 1") {
		t.Fatalf("a folder below: %s", taxes[strings.Index(taxes, "<main>"):])
	}
	file := follow(follow(taxes, "receipts"), "a&b.txt")
	if !strings.Contains(file, "Restore this file") || !strings.Contains(file, "one file, 7 B") {
		t.Fatalf("one file: %s", file[strings.Index(file, "<main>"):])
	}
	// form sends the restore form of a page as the browser would.
	form := func(page, folder string, more url.Values) string {
		t.Helper()
		v := url.Values{"csrf": {csrf}, "back": {"/restore"}, "folder": {folder}}
		for _, name := range []string{"device", "snapshot", "root", "sub"} {
			v.Set(name, html.UnescapeString(extract(page, `name="`+name+`" value="`, `"`)))
		}
		for k, vals := range more {
			v[k] = vals
		}
		return read(browser.PostForm(base+"/action/restore-start", v))
	}
	wait := func() *jobs.RestoreStatus {
		t.Helper()
		for i := 0; i < 250; i++ {
			if s := runner.Status().Restore; s != nil && !s.Running {
				return s
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the restore did not end")
		return nil
	}
	out := filepath.Join(t.TempDir(), "restored here")
	if p := form(file, "relative/folder", nil); !strings.Contains(p, "The restore did not start") || runner.Status().Restore != nil {
		t.Fatalf("a folder that is not a full path: %s", firstLines(p))
	}
	if p := form(file, out, url.Values{"sub": {"taxes/.."}}); !strings.Contains(p, "The restore did not start") || runner.Status().Restore != nil {
		t.Fatalf("a place named ..: %s", firstLines(p))
	}
	if p := form(file, out, nil); !strings.Contains(p, "The restore has started") {
		t.Fatalf("starting the restore of a file: %s", firstLines(p))
	}
	if s := wait(); s.Err != "" || s.Report == nil || s.Report.Files != 1 {
		t.Fatalf("the restore of a file: %+v", s)
	}
	if b, err := os.ReadFile(filepath.Join(out, "a&b.txt")); err != nil || string(b) != "receipt" {
		t.Fatalf("the restored file: %q %v", b, err)
	}
	// A folder keeps its name inside the folder given; a file that is
	// there already stays unless the box is ticked.
	if err := os.MkdirAll(filepath.Join(out, "taxes"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(out, "taxes", "2025 return.pdf")
	_ = os.WriteFile(mine, []byte("mine"), 0o644)
	form(taxes, out, nil)
	if s := wait(); s.Err != "" || s.Report == nil {
		t.Fatalf("the restore of a folder: %+v", s)
	}
	if b, _ := os.ReadFile(mine); string(b) != "mine" {
		t.Fatalf("a file that existed was replaced without the box: %q", b)
	}
	if b, _ := os.ReadFile(mine + ".restored"); string(b) != "pdf bytes" {
		t.Fatalf("the restored copy beside a file that existed: %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(out, "taxes", "receipts", "a&b.txt")); err != nil || string(b) != "receipt" {
		t.Fatalf("the restored folder: %q %v", b, err)
	}
	form(taxes, out, url.Values{"overwrite": {"on"}})
	if s := wait(); s.Err != "" {
		t.Fatalf("the restore over existing files: %+v", s)
	}
	if b, _ := os.ReadFile(mine); string(b) != "pdf bytes" {
		t.Fatalf("with the box ticked the file is replaced: %q", b)
	}
	if p := read(browser.Get(base + "/restore")); !strings.Contains(p, "Restore finished") || !strings.Contains(p, "Destination: "+html.EscapeString(filepath.Join(out, "taxes"))) {
		t.Fatalf("the outcome on the page: %s", p[strings.Index(p, "<main>"):])
	}
	// What does not exist is said, not a crash; no GET started anything.
	if p := read(browser.Get(base + "/restore?device=" + device.String() + "&snapshot=nope")); !strings.Contains(p, "The network could not be read") {
		t.Fatalf("an unknown version: %s", firstLines(p))
	}
	if p := read(browser.Get(base + "/restore?device=x&snapshot=y&root=z")); !strings.Contains(p, "bad device key") {
		t.Fatalf("a bad device: %s", firstLines(p))
	}
}

func TestRestorePlaces(t *testing.T) {
	for _, c := range [][3]string{{"", "", ""}, {"/home/x/docs", "", "/home/x/docs"}, {"/home/x/docs", "a/b", "/home/x/docs/a/b"}, {`C:\Users\x`, "a", `C:\Users\x/a`}, {"", "a", ""}} {
		if got := restorePath(c[0], c[1]); got != c[2] {
			t.Errorf("restorePath(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	for p, want := range map[string]string{"/home/x/docs": "docs", `C:\Users\x\My Docs\`: "My Docs", "a": "a", "/": "", "a/..": ".."} {
		if got := lastName(p); got != want {
			t.Errorf("lastName(%q) = %q, want %q", p, got, want)
		}
	}
	if h := restoreHref("d", "s", "/a b", "c&d"); h != "restore?device=d&root=%2Fa+b&snapshot=s&sub=c%26d" {
		t.Errorf("href: %s", h)
	}
}
