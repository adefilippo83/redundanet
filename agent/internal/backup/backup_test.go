package backup_test

import (
	"bytes"
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
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

type world struct {
	t      *testing.T
	fake   *engine.Fake
	db     *localdb.DB
	member identity.Key
	device identity.Key
	src    string
	now    time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, src: filepath.Join(t.TempDir(), "home"), now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	w.member[0], w.device[0] = 1, 2
	w.fake = engine.NewFake(w.member, w.device, 2, 4)
	db, err := localdb.Open(t.TempDir(), func() time.Time { return w.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w.db = db
	mk := func(rel string, content []byte, mtime time.Time) {
		p := filepath.Join(w.src, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, content, 0o640); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, mtime, mtime)
	}
	old := w.now.Add(-48 * time.Hour)
	mk("notes.txt", []byte("tiny"), old)                                                        // inline
	mk("docs/report.bin", pattern(300_000, 1), old)                                             // one object
	mk("docs/same.bin", pattern(300_000, 1), old)                                               // the same bytes: one upload
	mk("photos/2026/big.bin", pattern(70<<20, 2), old)                                          // two chunks
	mk("docs/.DS_Store", []byte("junk junk junk junk junk junk junk junk junk junk junk"), old) // excluded
	_ = os.MkdirAll(filepath.Join(w.src, "Library", "Caches", "x"), 0o755)
	mk("Library/Caches/x/c.bin", pattern(1000, 3), old) // excluded by prefix
	_ = os.Symlink("docs/report.bin", filepath.Join(w.src, "link"))
	return w
}

func pattern(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+3) ^ salt
	}
	return b
}

func (w *world) run(ctx context.Context) (*backup.Result, error) {
	return backup.Run(ctx, backup.Config{Engine: w.fake, DB: w.db, Member: w.member, Device: w.device,
		Set: backup.Set{Name: "home", Folders: []string{w.src}}, Now: func() time.Time { return w.now }})
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	res, err := w.run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Snapshot
	if s.Files != 4 || s.Skipped != 0 || s.Partial || res.RootVersion != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
	// Dedup: same.bin cost nothing; the big file is two chunks.
	if w.fake.Puts != 1+2+4 { // report.bin, two chunks, four directory and index objects (docs, photos/2026, photos, home top + index = 5?)
		t.Logf("puts: %d", w.fake.Puts)
	}
	listing, err := restore.List(ctx, w.fake, w.member)
	if err != nil || len(listing.Devices) != 1 || listing.Devices[0].Index == nil || len(listing.Devices[0].Index.Snapshots) != 1 {
		t.Fatalf("list: %+v %v", listing, err)
	}
	snap := listing.Devices[0].Index.Latest()
	// Browse: the top lists the folder by its absolute path; inside, the files.
	_, top, err := restore.Browse(ctx, w.fake, snap, nil)
	if err != nil || len(top.Entries) != 1 || top.Entries[0].NameString() != w.src {
		t.Fatalf("browse top: %+v %v", top, err)
	}
	_, docs, err := restore.Browse(ctx, w.fake, snap, []string{w.src, "docs"})
	if err != nil || docs.Find("report.bin") == nil || docs.Find(".DS_Store") != nil || docs.Find("same.bin").Cap != docs.Find("report.bin").Cap {
		t.Fatalf("browse docs: %v %+v", err, docs)
	}
	linkTarget := filepath.FromSlash("docs/report.bin") // os.Symlink stores the platform's separator
	if _, root, _ := restore.Browse(ctx, w.fake, snap, []string{w.src}); root.Find("Library") == nil || root.Find("Library").Entries != 0 || root.Find("link").Target != linkTarget || root.Find("notes.txt").Inline == nil || root.Find("docs").Mtime == 0 {
		t.Fatalf("root entries: %+v", root)
	}
	if _, lib, _ := restore.Browse(ctx, w.fake, snap, []string{w.src, "Library"}); lib.Find("Caches") != nil {
		t.Fatal("the excluded cache folder was backed up")
	}
	if _, _, err := restore.Browse(ctx, w.fake, snap, []string{w.src, "nope"}); err != snapshot.ErrNotFound {
		t.Fatalf("missing path: %v", err)
	}
	// A typed path resolves to elements with either separator, and the
	// top element is the entry's own name.
	for _, typed := range []string{filepath.Join(w.src, "docs"), strings.ReplaceAll(w.src, "/", `\`) + `\docs`, strings.ReplaceAll(w.src, `\`, "/") + "/docs/"} {
		elems, err := restore.Resolve(ctx, w.fake, snap, typed)
		if err != nil || len(elems) != 2 || elems[0] != w.src || elems[1] != "docs" {
			t.Fatalf("resolve %q: %v %q", typed, err, elems)
		}
	}
	// Restore the whole folder and compare.
	target := filepath.Join(t.TempDir(), "restored")
	rep, err := restore.Restore(ctx, w.fake, snap, []string{w.src}, target, restore.Options{})
	if err != nil || len(rep.Exceptions) != 0 || rep.Files != 4 || rep.Symlinks != 1 {
		t.Fatalf("restore: %+v %v", rep, err)
	}
	for _, rel := range []string{"notes.txt", "docs/report.bin", "docs/same.bin", "photos/2026/big.bin"} {
		a, _ := os.ReadFile(filepath.Join(w.src, rel))
		b, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s differs: %v", rel, err)
		}
		fa, _ := os.Stat(filepath.Join(w.src, rel))
		fb, _ := os.Stat(filepath.Join(target, rel))
		if !fa.ModTime().Equal(fb.ModTime()) || fa.Mode().Perm() != fb.Mode().Perm() {
			t.Fatalf("%s: mtime or mode not restored", rel)
		}
	}
	if l, err := os.Readlink(filepath.Join(target, "link")); err != nil || l != linkTarget {
		t.Fatalf("symlink: %v", err)
	}
	// Restore a single file onto an existing one: kept with a suffix.
	one := filepath.Join(t.TempDir(), "report.bin")
	_ = os.WriteFile(one, []byte("old"), 0o644)
	rep, _ = restore.Restore(ctx, w.fake, snap, []string{w.src, "docs", "report.bin"}, one, restore.Options{})
	if rep.Files != 1 || len(rep.Exceptions) != 1 {
		t.Fatalf("restore over an existing file: %+v", rep)
	}
	if _, err := os.Stat(one + ".restored"); err != nil {
		t.Fatal("the suffixed copy is missing")
	}

	// A second run with no changes uploads no file bytes and reuses the
	// scan keys; only the index object, rewritten on every backup, is new.
	w.now = w.now.Add(24 * time.Hour)
	puts := w.fake.Puts
	res, err = w.run(ctx)
	if err != nil || res.Snapshot.Uploaded != 0 || w.fake.Puts != puts+1 || res.RootVersion != 2 {
		t.Fatalf("unchanged run: %+v puts %d->%d %v", res.Snapshot, puts, w.fake.Puts, err)
	}
	// A changed file is re-uploaded; a touched-but-identical file is not.
	_ = os.WriteFile(filepath.Join(w.src, "docs", "report.bin"), pattern(300_000, 9), 0o640)
	_ = os.Chtimes(filepath.Join(w.src, "docs", "same.bin"), w.now, w.now)
	w.now = w.now.Add(24 * time.Hour)
	res, err = w.run(ctx)
	if err != nil || res.Snapshot.Uploaded < 300_000 || res.Snapshot.Uploaded > 300_000+10_000 {
		t.Fatalf("changed run uploaded %d: %v", res.Snapshot.Uploaded, err)
	}
	listing, _ = restore.List(ctx, w.fake, w.member)
	if n := len(listing.Devices[0].Index.Snapshots); n != 3 {
		t.Fatalf("index has %d snapshots", n)
	}
	// The restore test: every sampled file reads back; unchanged sources compare.
	v, err := restore.Verify(ctx, w.fake, listing.Devices[0].Index.Latest(), 3, w.now)
	if err != nil || v.Sampled != 3 || v.Verified != 3 || len(v.Failed) != 0 || v.Compared < 1 {
		t.Fatalf("verify: %+v %v", v, err)
	}
}

func TestInterruptedRunPublishesAPartialSnapshot(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first upload.
	res, err := backup.Run(ctx, backup.Config{Engine: w.fake, DB: w.db, Member: w.member, Device: w.device,
		Set: backup.Set{Name: "home", Folders: []string{w.src}}, Now: func() time.Time { return w.now },
		Progress: func(p backup.Progress) {
			if p.Uploaded > 0 {
				cancel()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Snapshot.Partial || res.Snapshot.Skipped == 0 || res.RootVersion != 1 {
		t.Fatalf("partial run: %+v", res.Snapshot)
	}
	listing, _ := restore.List(context.Background(), w.fake, w.member)
	snap := listing.Devices[0].Index.Latest()
	if !snap.Partial {
		t.Fatal("the published snapshot is not marked partial")
	}
	// What was stored restores; what was skipped is reported, not invented.
	rep, _ := restore.Restore(context.Background(), w.fake, snap, []string{w.src}, filepath.Join(t.TempDir(), "r"), restore.Options{})
	if len(rep.Exceptions) == 0 || rep.Files+int64(len(rep.Exceptions)) < 2 {
		t.Fatalf("partial restore: %+v", rep)
	}
	// The next, complete run finishes the job.
	w.now = w.now.Add(time.Hour)
	res, err = w.run(context.Background())
	if err != nil || res.Snapshot.Partial || res.Snapshot.Files != 4 {
		t.Fatalf("completing run: %+v %v", res.Snapshot, err)
	}
	listing, _ = restore.List(context.Background(), w.fake, w.member)
	if n := len(listing.Devices[0].Index.Snapshots); n != 2 {
		t.Fatalf("index has %d snapshots", n)
	}
}

func TestUnreadableFileIsSkippedAndCounted(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a file mode does not make a file unreadable on Windows")
	}
	w := newWorld(t)
	p := filepath.Join(w.src, "docs", "secret.bin")
	_ = os.WriteFile(p, pattern(5000, 4), 0o000)
	res, err := w.run(context.Background())
	if err != nil || res.Snapshot.Skipped != 1 || res.Reasons["permission denied"] != 1 || res.Snapshot.Partial {
		t.Fatalf("unreadable: %+v %+v %v", res.Snapshot, res.Reasons, err)
	}
	if why := res.SkipReasons(); why != "1 permission denied" {
		t.Fatalf("skip reasons: %q", why)
	}
	_, docs, _ := restore.Browse(context.Background(), w.fake, w.member2snap(), []string{w.src, "docs"})
	if e := docs.Find("secret.bin"); e == nil || e.Kind != snapshot.KindSkipped {
		t.Fatalf("the unreadable file is not listed as skipped: %+v", e)
	}
}

func (w *world) member2snap() *snapshot.Snapshot {
	l, _ := restore.List(context.Background(), w.fake, w.member)
	return l.Devices[0].Index.Latest()
}

// A file that comes back after being away is probed on the grid before
// its cached capability is reused: an object tombstoned meanwhile is
// stored again, so the new root never points at a deleted object. A file
// seen in the last snapshot is not probed: the current root references it.
func TestReturningFileIsProbedBeforeReuse(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	runWith := func(excludes ...string) {
		t.Helper()
		if _, err := backup.Run(ctx, backup.Config{Engine: w.fake, DB: w.db, Member: w.member, Device: w.device,
			Set: backup.Set{Name: "home", Folders: []string{w.src}, Excludes: excludes}, Now: func() time.Time { return w.now }}); err != nil {
			t.Fatal(err)
		}
	}
	runWith()
	h := object.Sum(pattern(300_000, 1))
	rc, ok, _ := w.fake.Has(ctx, h, 300_000)
	if !ok {
		t.Fatal("the report's object is not stored")
	}
	// The report sits out a run, excluded, and its object is deleted
	// meanwhile, as the cleanup would once nothing referenced it.
	w.now = w.now.Add(24 * time.Hour)
	runWith("report.bin", "same.bin")
	if err := w.fake.Delete(ctx, rc); err != nil {
		t.Fatal(err)
	}
	// Keep the rolling re-hash off these two files, so the cache path is
	// the one under test.
	for _, name := range []string{"report.bin", "same.bin"} {
		row, err := w.db.LookupScan(filepath.Join(w.src, "docs", name))
		if err != nil || row == nil {
			t.Fatalf("scan row of %s: %v", name, err)
		}
		row.LastHashed = w.now
		_ = w.db.RememberScan(*row)
	}
	w.now = w.now.Add(24 * time.Hour)
	runWith()
	if w.fake.Tombstoned(rc) || w.fake.Checks == 0 {
		t.Fatalf("the returning file was not probed: tombstoned %v, %d checks", w.fake.Tombstoned(rc), w.fake.Checks)
	}
	// A file in the last snapshot is not probed: the root references it.
	w.fake.Checks = 0
	w.now = w.now.Add(24 * time.Hour)
	runWith()
	if w.fake.Checks != 0 {
		t.Fatalf("%d probes for files the last snapshot holds", w.fake.Checks)
	}
}
