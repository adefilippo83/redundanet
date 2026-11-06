package jobs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
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

// A folder the service cannot read is refused when the set is added, and
// one that becomes unreadable later shows on the status with what to do.
func TestSetsReportFoldersTheServiceCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("an unreadable folder needs a non-root unix user")
	}
	var member, device identity.Key
	member[0], device[0] = 1, 2
	fake := engine.NewFake(member, device, 2, 4)
	dir := t.TempDir()
	db, err := localdb.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device})
	if err != nil {
		t.Fatal(err)
	}
	guarded := filepath.Join(t.TempDir(), "guarded")
	_ = os.Mkdir(guarded, 0o000)
	t.Cleanup(func() { _ = os.Chmod(guarded, 0o755) })
	if err := r.AddSet(backup.Set{Name: "g", Folders: []string{guarded}}); err == nil || !strings.Contains(err.Error(), guarded) {
		t.Fatalf("an unreadable folder was accepted: %v", err)
	}
	open := filepath.Join(t.TempDir(), "open")
	_ = os.Mkdir(open, 0o755)
	if err := r.AddSet(backup.Set{Name: "o", Folders: []string{open}}); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); len(st.Runs) != 1 || len(st.Runs[0].Needs) != 0 {
		t.Fatalf("a readable set needs nothing: %+v", st.Runs)
	}
	_ = os.Chmod(open, 0o000)
	t.Cleanup(func() { _ = os.Chmod(open, 0o755) })
	if st := r.Status(); len(st.Runs[0].Needs) != 1 || !strings.Contains(st.Runs[0].Needs[0], open) {
		t.Fatalf("a folder that became unreadable: %+v", st.Runs)
	}
}

// TestLastRunSurvivesARestart: the last run of a set is kept on disk, so
// a restart, a self-update most often, does not read as a set that never
// ran.
func TestLastRunSurvivesARestart(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
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
	if err := r.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if rs := r.RunSet(context.Background(), backup.Set{Name: "docs", Folders: []string{src}}, snapshot.DefaultRetention); rs == nil || rs.LastErr != "" {
		t.Fatalf("run: %+v", rs)
	}
	r2, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	st := r2.Status()
	if len(st.Runs) != 1 || st.Runs[0].Set != "docs" || !st.Runs[0].LastAt.Equal(now) || st.Runs[0].Last == nil || st.Runs[0].Last.Snapshot.Files != 1 || st.Runs[0].LastErr != "" || st.Runs[0].Running {
		t.Fatalf("after a restart: %+v", st.Runs)
	}
}

// TestLimitsAreSavedAndApplied: the bandwidth caps live in the jobs file,
// reach the limiters on load and on every change, and refuse nonsense.
func TestLimitsAreSavedAndApplied(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	dir := t.TempDir()
	db, err := localdb.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got [][2]int
	cfg := Config{DataDir: dir, Engine: engine.NewFake(member, device, 2, 4), DB: db, Member: member, Device: device,
		OnLimits: func(up, down int) { got = append(got, [2]int{up, down}) }}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != [2]int{0, 0} {
		t.Fatalf("limits on load: %v", got)
	}
	if err := r.SetLimits(500, 0); err != nil || len(got) != 2 || got[1] != [2]int{500, 0} {
		t.Fatalf("set: %v %v", err, got)
	}
	if err := r.SetLimits(-1, 0); err == nil {
		t.Fatal("a negative cap was accepted")
	}
	st := r.Status()
	if st.UploadKBps != 500 || st.DownloadKBps != 0 {
		t.Fatalf("status: %d %d", st.UploadKBps, st.DownloadKBps)
	}
	r2, err := New(cfg)
	if err != nil || len(got) != 3 || got[2] != [2]int{500, 0} || r2.Status().UploadKBps != 500 {
		t.Fatalf("after a restart: %v %v", err, got)
	}
}

// TestSettingsPersistAndReconfigure: schedule, retention and a set's
// excludes change through the runner, are checked, saved, and come back
// after a restart.
func TestSettingsPersistAndReconfigure(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	dir := t.TempDir()
	db, err := localdb.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := Config{DataDir: dir, Engine: engine.NewFake(member, device, 2, 4), DB: db, Member: member, Device: device}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "docs")
	_ = os.MkdirAll(src, 0o755)
	if err := r.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSchedule(30*time.Minute, 48*time.Hour, 48*time.Hour); err == nil {
		t.Fatal("a half-hourly backup was accepted")
	}
	if err := r.SetSchedule(2*time.Hour, 48*time.Hour, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRetention(snapshot.Retention{Days: 0}); err == nil {
		t.Fatal("a zero-day retention was accepted")
	}
	if err := r.SetRetention(snapshot.Retention{Days: 3, Daily: 10, Weekly: 4, Monthly: 6}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetExcludes("nope", []string{"*.iso"}); err == nil {
		t.Fatal("excludes of a missing set were accepted")
	}
	if err := r.SetExcludes("docs", []string{"*.iso", "cache/"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.reconf:
	default:
		t.Fatal("the scheduler was not woken")
	}
	r2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := r2.Status()
	if st.Every != "2h0m0s" || st.VerifyEvery != "48h0m0s" || st.CleanupEvery != "72h0m0s" || st.Retention != (snapshot.Retention{Days: 3, Daily: 10, Weekly: 4, Monthly: 6}) || len(st.Sets) != 1 || len(st.Sets[0].Excludes) != 2 {
		t.Fatalf("after a restart: %+v", st)
	}
}

// TestBudgetRefusesARunWhenUsedUp: with the servers holding more than the
// member's allocation stands for, a run is refused and says so; below it
// the run goes ahead and the budget shows the use.
func TestBudgetRefusesARunWhenUsedUp(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	dir := t.TempDir()
	db, err := localdb.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fake := engine.NewFake(member, device, 2, 4)
	// 100 GB contributed, 15% reserve, 2-of-4: 42 GB of data allowed.
	man := &manifest.Manifest{Network: manifest.Network{Name: "testnet", Encoding: manifest.Encoding{K: 2, N: 4}, ReservePercent: 15},
		Members: []manifest.Member{{Name: "m", Key: member, ContributionGB: 100, Status: manifest.StatusActive}}}
	r, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device, Manifest: func() *manifest.Manifest { return man }})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "docs")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "a.bin"), make([]byte, 5000), 0o644)
	if err := r.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	fake.UsageBytes = 100e9 // share bytes: 50 GB of data, above 42
	rs := r.RunSet(context.Background(), backup.Set{Name: "docs", Folders: []string{src}}, snapshot.DefaultRetention)
	if rs == nil || !strings.Contains(rs.LastErr, "allocation is used up") || fake.Puts != 0 {
		t.Fatalf("over the allocation: %+v, %d puts", rs, fake.Puts)
	}
	if b := r.Status().Budget; b == nil || !b.Exhausted || b.AllowedGB != 42 || b.UsedGB < 49.9 {
		t.Fatalf("budget: %+v", b)
	}
	fake.UsageBytes = 10e9 // 5 GB of data
	rs = r.RunSet(context.Background(), backup.Set{Name: "docs", Folders: []string{src}}, snapshot.DefaultRetention)
	if rs == nil || rs.LastErr != "" || rs.Last == nil {
		t.Fatalf("under the allocation: %+v", rs)
	}
	if b := r.Status().Budget; b == nil || b.Exhausted || b.UsedGB > 5.1 {
		t.Fatalf("budget after: %+v", b)
	}
}
