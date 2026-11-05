package diag

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/service"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

func TestRedactHidesPathsAndAddresses(t *testing.T) {
	cases := map[string]string{
		`open /Users/alice/Documents/tax 2026.pdf: permission denied`:      `open <path:` + Hash("/Users/alice/Documents/tax") + `> 2026.pdf: permission denied`,
		`folder=/home/bob/photos err="read /home/bob/photos/a.jpg: eof"`:   `folder=<path:` + Hash("/home/bob/photos") + `> err="read <path:` + Hash("/home/bob/photos/a.jpg") + `>: eof"`,
		`path C:\Users\carol\Desktop\notes.txt skipped`:                    `path <path:` + Hash(`C:\Users\carol\Desktop\notes.txt`) + `> skipped`,
		`hub channel up observed=203.0.113.5:4433 from=[2001:db8::1]:8443`: `hub channel up observed=<addr> from=<addr>`,
		`share stored si=unq2wo4l n=3 owner=au2u2666 from=rsi2hi76`:        `share stored si=unq2wo4l n=3 owner=au2u2666 from=rsi2hi76`,
		`version 3.0.0-alpha.17 on darwin/arm64`:                           `version 3.0.0-alpha.17 on darwin/arm64`,
		`fetch https://example.invalid/manifests/manifest.yaml: 503`:       `fetch <url>: 503`,
		`dial hub.example.org:8443: timeout`:                               `dial <addr>: timeout`,
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	if Hash("/a") == Hash("/b") || len(Hash("/a")) != 8 {
		t.Fatal("hash")
	}
}

func TestBundleCarriesTheAllowlistOnly(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	folder := "/Users/alice/Documents/work"
	in := Input{Now: now, Version: "3.0.0-test", Network: "testnet", DataDir: "/Users/alice/Library/Application Support/redundanet",
		Device: "devkey", Member: "mem12345", Roles: []string{"client"}, AgentUp: true, Uptime: "3h",
		Service: &service.Status{Installed: true, Running: true, Path: "/Users/alice/Library/LaunchAgents/org.redundanet.redundanet.plist"},
		Transport: &transport.Status{UDP: "192.0.2.1:5000", Hubs: []transport.HubStatus{{Hub: "bm4ld4ez", Up: true, Carrier: "udp", Observed: "203.0.113.5:4433", Reconnects: 2, LastError: "dial 203.0.113.5:4433: timeout"}},
			Sessions: []transport.SessionStatus{{Peer: "p1", Direct: "198.51.100.7:4000", Candidates: []string{"198.51.100.7:4000"}}, {Peer: "p2", Relay: true}, {Peer: "p3"}}},
		Membership: &membership.Status{Revision: 4, Members: 1, Hubs: 1, LastError: "fetch https://example.invalid/manifests: 503"},
		Jobs: &jobs.Status{Sets: []backup.Set{{Name: "documents", Folders: []string{folder}, Excludes: []string{"node_modules"}}}, VerifyEvery: "168h0m0s", VerifyStreak: 2,
			Runs:       []jobs.RunStatus{{Set: "documents", LastAt: now, LastErr: "open " + folder + "/x: permission denied", Needs: []string{folder + ": grant Full Disk Access"}, Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 113, Bytes: 1219734, Skipped: 1}}}},
			LastVerify: &restore.Verification{At: now, Sampled: 20, Verified: 20}},
		Lines: []logbuf.Line{{Time: now, Level: "WARN", Msg: "backup failed", Attrs: "set=documents err=\"open " + folder + "/x: permission denied\""},
			{Time: now, Level: "WARN", Msg: "backup failed", Attrs: "set=documents"}, {Time: now, Level: "INFO", Msg: "hub channel up", Attrs: "observed=203.0.113.5:4433"}},
		LogText: "time=2026-11-04T11:00:00Z level=ERROR msg=\"storage unavailable\" disk=/Volumes/Big\n" + "time=2026-11-04T11:01:00Z level=INFO msg=status snapshot=\"" + strings.Repeat("x", 2000) + "\"\n"}
	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	if len(files) != 3 || files["summary.txt"] == "" || files["summary.json"] == "" || files["log.txt"] == "" {
		t.Fatalf("files: %v", files)
	}
	all := files["summary.txt"] + files["summary.json"] + files["log.txt"]
	for _, secret := range []string{"alice", "Documents/work", "203.0.113.5", "198.51.100.7", "192.0.2.1", "/Volumes/Big", "example.invalid"} {
		if strings.Contains(all, secret) {
			t.Errorf("the bundle leaks %q", secret)
		}
	}
	for _, line := range strings.Split(files["log.txt"], "\n") {
		if len(line) > maxLine+10 {
			t.Fatalf("a log line of %d bytes was not clipped", len(line))
		}
	}
	for _, want := range []string{"documents:" + Hash(folder), "3.0.0-test", "revision 4", "2 weeks in a row", "1 direct, 1 relayed, 1 wanted", "113 files", "grant Full Disk Access", "backup failed", "storage unavailable", "2 reconnects", "installed true"} {
		if !strings.Contains(all, want) {
			t.Errorf("the bundle lacks %q", want)
		}
	}
	var s Summary
	if err := json.Unmarshal([]byte(files["summary.json"]), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Errors) != 1 || s.Errors[0].Count != 2 || s.Errors[0].Msg != "backup failed" || s.Peers.Direct != 1 || len(s.Sets) != 1 || s.Sets[0].Folders[0] != "documents:"+Hash(folder) {
		t.Fatalf("summary: %+v", s)
	}
	// With the agent down, the errors come from the log text.
	down := Input{Now: now, Version: "3.0.0-test", LogText: in.LogText + in.LogText}
	if s := Summarise(down); s.AgentUp || len(s.Errors) != 1 || s.Errors[0].Level != "ERROR" || s.Errors[0].Count != 2 || s.Errors[0].Msg != "storage unavailable" {
		t.Fatalf("down: %+v", s.Errors)
	}
}
