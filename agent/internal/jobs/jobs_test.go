package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

func TestRunnerRunsVerifiesAndRestores(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	fake := engine.NewFake(member, device, 2, 4)
	dir := t.TempDir()
	db, err := localdb.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	src := filepath.Join(t.TempDir(), "docs")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "a.bin"), make([]byte, 5000), 0o644)
	_ = os.WriteFile(filepath.Join(src, "b.txt"), []byte("hello"), 0o644)
	r, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if err := r.AddSet(backup.Set{Name: "bad", Folders: []string{filepath.Join(src, "missing")}}); err == nil {
		t.Fatal("a missing folder was accepted")
	}
	again, _ := Load(dir)
	if len(again.Sets) != 1 || again.Sets[0].Folders[0] != src || again.Every != 24*time.Hour {
		t.Fatalf("saved file: %+v", again)
	}
	rs := r.RunSet(context.Background(), again.Sets[0], snapshot.DefaultRetention)
	if rs == nil || rs.LastErr != "" || rs.Last.Snapshot.Files != 2 || rs.Running {
		t.Fatalf("run: %+v", rs)
	}
	st := r.Status()
	if len(st.Runs) != 1 || st.Runs[0].Last == nil || st.Sets[0].Name != "docs" {
		t.Fatalf("status: %+v", st)
	}
	at, partial := r.LastBackup()
	if !at.Equal(now) || partial {
		t.Fatalf("last backup %v partial %v", at, partial)
	}
	v := r.Verify(context.Background())
	if v == nil || v.Sampled != 2 || v.Verified != 2 || v.Compared != 2 {
		t.Fatalf("verify: %+v", v)
	}
	target := filepath.Join(t.TempDir(), "out")
	if err := r.StartRestore(device, rs.Last.Snapshot.ID, src, target, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if s := r.Status().Restore; s != nil && !s.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := r.Status().Restore
	if s == nil || s.Running || s.Err != "" || s.Report == nil || s.Report.Files != 2 {
		t.Fatalf("restore: %+v", s)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "b.txt")); string(b) != "hello" {
		t.Fatal("restored content differs")
	}
	if err := r.RemoveSet("docs"); err != nil || len(r.Status().Sets) != 0 {
		t.Fatalf("remove: %v", err)
	}
}

func TestStreakCountsWeeksInARow(t *testing.T) {
	pass := func(d time.Time) localdb.Verification { return localdb.Verification{At: d, Sampled: 20, Verified: 20} }
	fail := func(d time.Time) localdb.Verification {
		return localdb.Verification{At: d, Sampled: 20, Verified: 19, Failed: []string{"x"}}
	}
	wed := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) // ISO week 45
	week := func(n int) time.Time { return wed.AddDate(0, 0, -7*n) }
	if got := streak(nil, wed); got != 0 {
		t.Fatalf("no tests: %d", got)
	}
	// Four weeks in a row ending this week.
	if got := streak([]localdb.Verification{pass(week(0)), pass(week(1)), pass(week(2)), pass(week(3))}, wed); got != 4 {
		t.Fatalf("four weeks: %d", got)
	}
	// This week has no test yet: the streak counts from last week.
	if got := streak([]localdb.Verification{pass(week(1)), pass(week(2))}, wed); got != 2 {
		t.Fatalf("this week pending: %d", got)
	}
	// A failed week breaks it; a passing test later in the same week mends it.
	if got := streak([]localdb.Verification{pass(week(0)), fail(week(1)), pass(week(2))}, wed); got != 1 {
		t.Fatalf("a failed week: %d", got)
	}
	if got := streak([]localdb.Verification{pass(week(0)), fail(week(1)), pass(week(1).Add(time.Hour)), pass(week(2))}, wed); got != 3 {
		t.Fatalf("a mended week: %d", got)
	}
	// A gap of a week breaks it.
	if got := streak([]localdb.Verification{pass(week(0)), pass(week(2)), pass(week(3))}, wed); got != 1 {
		t.Fatalf("a gap: %d", got)
	}
	// Two tests in one week count once.
	if got := streak([]localdb.Verification{pass(week(0)), pass(week(0).Add(time.Hour)), pass(week(1))}, wed); got != 2 {
		t.Fatalf("two in a week: %d", got)
	}
}

func TestLoadMovesTheMonthlyDefaultToWeekly(t *testing.T) {
	dir := t.TempDir()
	// A file an earlier version saved with its monthly default.
	old := "sets: []\nevery: 24h0m0s\nverify_every: 720h0m0s\nretention:\n    days: 7\n    daily: 30\n    weekly: 12\n    monthly: 12\n"
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(dir)
	if err != nil || f.VerifyEvery != 7*24*time.Hour || f.Version != fileVersion {
		t.Fatalf("old default: %+v %v", f, err)
	}
	if err := f.Save(dir); err != nil {
		t.Fatal(err)
	}
	// Saved as version 2: a monthly test set on purpose now stays.
	f.VerifyEvery = 30 * 24 * time.Hour
	if err := f.Save(dir); err != nil {
		t.Fatal(err)
	}
	if g, err := Load(dir); err != nil || g.VerifyEvery != 30*24*time.Hour {
		t.Fatalf("explicit monthly on version 2: %+v %v", g, err)
	}
	// A missing file has the weekly default.
	if g, err := Load(t.TempDir()); err != nil || g.VerifyEvery != 7*24*time.Hour || g.Version != fileVersion {
		t.Fatalf("defaults: %+v %v", g, err)
	}
}

// The restore test is recorded in the local database: the status lists
// it, the streak counts it, and a new runner on the same database still
// knows the last one.
func TestRestoreTestsAreRecordedAndSurviveARestart(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	fake := engine.NewFake(member, device, 2, 4)
	dir := t.TempDir()
	db, err := localdb.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	src := filepath.Join(t.TempDir(), "docs")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "a.bin"), make([]byte, 5000), 0o644)
	clock := func() time.Time { return now }
	r, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status().LastVerify != nil || r.VerifyStreak() != 0 {
		t.Fatal("a fresh runner has a restore test")
	}
	if err := r.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if rs := r.RunSet(context.Background(), backup.Set{Name: "docs", Folders: []string{src}}, snapshot.DefaultRetention); rs == nil || rs.LastErr != "" {
		t.Fatalf("run: %+v", rs)
	}
	// Two weekly tests.
	if v := r.Verify(context.Background()); v == nil || v.Verified != 1 {
		t.Fatalf("verify: %+v", v)
	}
	now = now.AddDate(0, 0, 7)
	if v := r.Verify(context.Background()); v == nil || v.Verified != 1 {
		t.Fatalf("second verify: %+v", v)
	}
	st := r.Status()
	if len(st.Verifications) != 2 || !st.Verifications[0].At.Equal(now) || !st.Verifications[0].Passed() || st.VerifyStreak != 2 || st.VerifyEvery != "168h0m0s" {
		t.Fatalf("status: %+v", st)
	}
	// A new runner on the same database: the last test is still known.
	r2, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if v := r2.LastVerify(); v == nil || !v.At.Equal(now) || v.Verified != 1 {
		t.Fatalf("after a restart: %+v", v)
	}
	if r2.VerifyStreak() != 2 {
		t.Fatalf("streak after a restart: %d", r2.VerifyStreak())
	}
}
