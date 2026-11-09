package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/cleanup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/service"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

// TestPreview serves the page with made-up data, for working on its look
// without an agent or a network:
//
//	REDUNDANET_PAGE_PREVIEW=127.0.0.1:4499 go test -v ./internal/api -run TestPreview -timeout 0
//
// prints a login link and serves until interrupted. REDUNDANET_PAGE_STATE
// picks the case: protected (the default), running, attention, empty or
// setup. Without the variable the test does nothing.
func TestPreview(t *testing.T) {
	addr := os.Getenv("REDUNDANET_PAGE_PREVIEW")
	if addr == "" {
		t.Skip("set REDUNDANET_PAGE_PREVIEW=127.0.0.1:PORT to serve the page with sample data")
	}
	state := os.Getenv("REDUNDANET_PAGE_STATE")
	buf := logbuf.New(slog.NewTextHandler(io.Discard, nil), 100)
	lg := slog.New(buf)
	now := time.Now()
	snap := func() Snapshot {
		j := &jobs.Status{Every: "24h0m0s", VerifyEvery: "168h0m0s", CleanupEvery: "168h0m0s", Retention: snapshot.DefaultRetention, VerifyStreak: 5,
			Sets: []backup.Set{{Name: "documents", Folders: []string{"/Users/sample/Documents"}, Excludes: []string{"*.iso"}}, {Name: "photos", Folders: []string{"/Users/sample/Pictures"}}},
			Runs: []jobs.RunStatus{
				{Set: "documents", LastAt: now.Add(-2 * time.Hour), Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 3204, Bytes: 18_400_000_000, Skipped: 2}, RootVersion: 41, Reasons: map[string]int64{"not on this computer": 2}}},
				{Set: "photos", LastAt: now.Add(-2 * time.Hour), Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 1608, Bytes: 193_600_000_000}, RootVersion: 41}}},
			Budget:        &jobs.Budget{UsedGB: 212, AllowedGB: 1020, Servers: 3, Answered: 3, At: now},
			Verifications: []localdb.Verification{{At: now.Add(-72 * time.Hour), Snapshot: "2026-11-06T16:00:00Z", Sampled: 40, Verified: 40, Compared: 38}, {At: now.Add(-240 * time.Hour), Snapshot: "2026-10-30T16:00:00Z", Sampled: 40, Verified: 40, Compared: 40}},
			LastCleanup:   &cleanup.Report{At: now.Add(-50 * time.Hour), Reachable: 4930, Tombstoned: 12}}
		s := Snapshot{Version: "3.0.12", Uptime: "4h46m", DataDir: "/Users/sample/Library/Application Support/redundanet", Network: "redundanet", Admission: "listed",
			Device: "rsi2hi76ducd5jvicnnzl4zblv32fv2vaxlukho2lnvnowgdkhgq", Member: "wekrreaqlio4j3m4vzxo6hn5be2sa2gxxv7gx3mq54m4xffztv2a", MemberCode: "C5ZV E7XX VXNS KSUG QI3I OZ7R XRVN 673X V4OS DLA7", Roles: []string{"client"},
			Transport: &transport.Status{Hubs: []transport.HubStatus{{Hub: "bm4ld4ez", Up: true, Carrier: "udp", Observed: "203.0.113.7:36641", Since: now.Add(-3 * time.Hour), Reconnects: 2}},
				Sessions: []transport.SessionStatus{{Peer: "ne6ftzx2aaaaaaaa", Member: "wekrreaqaaaaaaaa", Relay: true, Direct: "192.168.1.48:53990", Events: []transport.Event{{Time: now, What: "direct connection up"}}}, {Peer: "ikpijbsqaaaaaaaa", Member: "wsxr2cvmaaaaaaaa", Relay: true, Events: []transport.Event{{Time: now, What: "relay connection up"}}}}},
			Membership: &membership.Status{Network: "redundanet", Revision: 6, SignedAt: now.Add(-50 * time.Hour), Members: 2, Hubs: 1, LastFetch: now.Add(-2 * time.Minute)},
			Update:     &update.Status{Enabled: true, Running: "3.0.12", Channel: "stable", LastCheck: now.Add(-12 * time.Minute)},
			Repair:     &repair.Status{Done: 9}, Service: &service.Status{Installed: true, Running: true, Path: "/Users/sample/Library/LaunchAgents/org.redundanet.redundanet.plist"},
			Placement: &Placement{K: 2, N: 3, Servers: 3}, Jobs: j, Generated: now,
			Devices: []Device{{Key: "ne6ftzx2aaaaaaaa", Name: "falcon", Roles: []string{"client", "storage"}, PairedAt: now.Add(-90 * time.Hour)}, {Key: "ikpijbsqaaaaaaaa", Name: "pi-attic", Roles: []string{"client", "storage"}, PairedAt: now.Add(-20 * time.Hour)}},
			JoinURL: "https://redundanet.com/join.html?mode=device&devicename=pi-attic", JoinDevice: "pi-attic"}
		switch state {
		case "running":
			j.Runs[0].Running, j.Runs[0].Progress = true, &backup.Progress{Phase: "publishing", Files: 1200 + int64(time.Since(now).Seconds())*37, Uploaded: 48_000_000 + int64(time.Since(now).Seconds())*2_000_000}
		case "attention":
			j.Runs[0].Needs = []string{"/Users/sample/Documents: macOS denies the service access to this folder: grant Full Disk Access to RedundaNet in System Settings, Privacy & Security"}
			j.Runs[1].LastErr = "snapshot: no share could be stored: 1 of 3 storage devices answered"
			s.Update.Available, s.Update.Failed, s.Admission = "3.0.13", []string{"3.0.13-rc.1"}, "waiting"
		case "empty":
			j.Sets, j.Runs, j.Verifications, j.Budget, j.VerifyStreak, j.LastCleanup = nil, nil, nil, nil, 0, nil
		}
		return s
	}
	// The restore view reads a real backup: a small tree backed up on the
	// engine fake, which the runner behind the page lists and restores.
	var member, device identity.Key
	member[0], device[0] = 1, 2
	dataDir := t.TempDir()
	db, err := localdb.Open(dataDir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := jobs.New(jobs.Config{DataDir: dataDir, Engine: engine.NewFake(member, device, 2, 3), DB: db, Member: member, Device: device})
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(t.TempDir(), "Documents")
	for name, size := range map[string]int{"notes.txt": 4000, "budget-2026.ods": 38000, "Taxes/2025-return.pdf": 1200000, "Taxes/receipts.zip": 840000, "Letters/landlord.odt": 22000} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(tree, name)), 0o755)
		_ = os.WriteFile(filepath.Join(tree, name), make([]byte, size), 0o644)
	}
	if err := runner.AddSet(backup.Set{Name: "documents", Folders: []string{tree}}); err != nil {
		t.Fatal(err)
	}
	runner.RunSet(context.Background(), runner.Status().Sets[0], snapshot.DefaultRetention)
	base := snap
	snap = func() Snapshot {
		s := base()
		s.Device, s.Jobs.Restore = device.String(), runner.Status().Restore
		return s
	}
	cfg := Config{Addr: addr, DataDir: dataDir, Jobs: runner, Logs: buf, Log: lg, Version: "3.0.12", Snapshot: snap,
		Actions: Actions{CheckUpdate: func(context.Context) (string, error) { return "", nil }, ApplyUpdate: func(context.Context) error { return errors.New("this is the preview") },
			RetryUpdate: func(context.Context, string) error { return nil }, SetChannel: func(string) error { return nil },
			PairDevice: func(context.Context, string, string, bool) (Device, error) {
				return Device{}, errors.New("this is the preview: nothing is paired and the words went nowhere")
			}}}
	if state == "setup" {
		phase := SetupStatus{Phase: "choose"}
		cfg.Snapshot = nil
		cfg.Setup = &SetupActions{
			Begin: func(bool) ([]string, []int, error) {
				phase = SetupStatus{Phase: "confirm", Check: []int{2, 7, 11}}
				return strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliett kilo lima"), phase.Check, nil
			},
			Confirm: func([]string) error { phase = SetupStatus{Phase: "done"}; return nil },
			Restore: func(string, bool) error { return errors.New("this is the preview") },
			Pair: func() (string, error) {
				phase = SetupStatus{Phase: "pairing", Code: "rn3:pair:AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"}
				return phase.Code, nil
			},
			Cancel: func() { phase = SetupStatus{Phase: "choose"} },
			Status: func() SetupStatus { return phase },
		}
	}
	if state == "setup" {
		cfg.Jobs = nil
	}
	srv, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("backup published", "set", "documents", "files", 3204)
	lg.Info("peer ne6ftzx2", "event", "direct connection up")
	tok, _ := os.ReadFile(filepath.Join(cfg.DataDir, tokenFile))
	link, err := NewClient(srv.Addr(), strings.TrimSpace(string(tok))).LoginLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\nthe page, with sample data (%s): %s\n(the link works once, for two minutes; interrupt to stop)\n\n", state, link)
	select {}
}
