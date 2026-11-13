package collector

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// squatted is a share of a second member's object under a storage index
// another member uses: its own owner, its own descriptor.
func squatted(si byte, n uint8, owner identity.Key) wire.ShareEntry {
	e := share(si, n, owner)
	e.DescriptorHash[1] = 0xbb
	return e
}

func objectsOf(t *testing.T, w *world, si byte) map[identity.Key]Object {
	t.Helper()
	objs, _, err := w.c.Objects()
	if err != nil {
		t.Fatal(err)
	}
	out := map[identity.Key]Object{}
	for _, o := range objs {
		if o.StorageIndex[0] == si {
			if _, twice := out[o.Owner]; twice {
				t.Fatalf("two objects of %s under index %d", o.Owner.Short(), si)
			}
			out[o.Owner] = o
		}
	}
	return out
}

// Two members' rows under one storage index are two objects: each placed,
// deleted and counted on its own, and neither a conflict of the other.
func TestTwoOwnersUnderOneIndexAreTwoObjects(t *testing.T) {
	w := newWorld(t, "")
	w.heartbeat(falcon)
	w.heartbeat(spare)
	// Alice's object 9 is fully placed over both servers. Bob holds two
	// numbers of an object of his own under the same index on both.
	w.report(falcon, 1, share(9, 0, alice), share(9, 1, alice), squatted(9, 0, bob))
	w.report(spare, 1, share(9, 2, alice), share(9, 3, alice), squatted(9, 1, bob))
	objs := objectsOf(t, w, 9)
	a, b := objs[alice], objs[bob]
	if len(objs) != 2 || !a.Placed || a.Conflict || len(a.Shares) != 4 || a.Surplus != 0 || a.Hash[1] != 0 {
		t.Fatalf("alice's object: %+v", a)
	}
	if b.Placed || b.Conflict || len(b.Shares) != 2 || b.Hash[1] != 0xbb || !b.Readable || b.Need != "quiescing" {
		t.Fatalf("bob's object: %+v", b)
	}
	_, grid, _ := w.c.Objects()
	if grid.Objects != 2 || grid.Placed != 1 || grid.UnderPlaced != 1 || grid.Conflicts != 0 {
		t.Fatalf("grid: %+v", grid)
	}
	for _, m := range w.c.Status().Members {
		if (m.Name == "alice" && (m.Objects != 1 || m.UnderPlaced != 0)) || (m.Name == "bob" && (m.Objects != 1 || m.UnderPlaced != 1)) {
			t.Fatalf("member %s: %+v", m.Name, m)
		}
	}
	// Bob's tombstone on both servers deletes bob's object and not
	// alice's.
	sq := squatted(9, 0, bob)
	tomb := wire.TombstoneEntry{StorageIndex: sq.StorageIndex, Owner: bob, DescriptorHash: sq.DescriptorHash}
	for _, dev := range []identity.Key{falcon, spare} {
		w.c.RecordShares(identity.Peer{Device: dev, Member: alice}, &wire.ShareCensus{Since: 1, Generation: 2, Tombstones: []wire.TombstoneEntry{tomb}})
	}
	objs = objectsOf(t, w, 9)
	if !objs[bob].Tombstoned || objs[alice].Tombstoned || !objs[alice].Placed {
		t.Fatalf("after bob's tombstone: alice %+v, bob %+v", objs[alice], objs[bob])
	}
	// A removal names its owner: bob's number 0 goes from falcon, and
	// alice's number 0 stays there.
	w.c.RecordShares(identity.Peer{Device: falcon, Member: alice}, &wire.ShareCensus{Since: 2, Generation: 3,
		Removed: []wire.ShareKey{{StorageIndex: sq.StorageIndex, Number: 0, Owner: bob}}})
	objs = objectsOf(t, w, 9)
	if len(objs[bob].Shares) != 1 || len(objs[alice].Shares) != 4 || !objs[alice].Placed {
		t.Fatalf("after bob's removal: alice %+v, bob %+v", objs[alice], objs[bob])
	}
	// A removal without an owner, from a node that does not send one, is
	// the share that node reported under that index and number.
	w.c.RecordShares(identity.Peer{Device: spare, Member: alice}, &wire.ShareCensus{Since: 2, Generation: 3,
		Removed: []wire.ShareKey{{StorageIndex: sq.StorageIndex, Number: 3}}})
	objs = objectsOf(t, w, 9)
	if len(objs[alice].Shares) != 3 || objs[alice].Placed || objs[alice].Need != "repair" {
		t.Fatalf("after a removal without an owner: alice %+v", objs[alice])
	}
	if len(objs[bob].Shares) != 1 {
		t.Fatalf("a removal of alice's share took one of bob's: %+v", objs[bob])
	}
}

// Each of two objects under one index gets its own repair task and its
// own release hint, each naming its owner.
func TestTheSchedulerKeepsTwoOwnersApart(t *testing.T) {
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	sw.online(100<<30, falcon, spare, third)
	// Under index 9, alice's object has three of four numbers and bob's
	// has two of four. Under index 8, alice's is placed with number 0
	// twice, and bob's is placed exactly.
	sw.report(falcon, 1, share(9, 0, alice), squatted(9, 0, bob), share(8, 0, alice), share(8, 1, alice), squatted(8, 0, bob), squatted(8, 3, bob))
	sw.report(spare, 1, share(9, 1, alice), squatted(9, 1, bob), share(8, 0, alice), share(8, 2, alice), squatted(8, 1, bob))
	sw.report(third, 1, share(9, 2, alice), share(8, 3, alice), squatted(8, 2, bob))
	sw.advance(25 * time.Hour)
	sw.online(100<<30, falcon, spare, third)
	sw.c.Schedule(context.Background())
	tasks := map[identity.Key]*wire.RepairTask{}
	for _, tk := range sw.tasks() {
		if tk.StorageIndex[0] != 9 {
			t.Fatalf("a task for index %d", tk.StorageIndex[0])
		}
		if tasks[tk.Owner] != nil {
			t.Fatalf("two tasks for %s's object", tk.Owner.Short())
		}
		tasks[tk.Owner] = tk
	}
	if len(tasks) != 2 || len(tasks[alice].Shares) != 1 || tasks[alice].DescriptorHash[1] != 0 || len(tasks[bob].Shares) != 2 || tasks[bob].DescriptorHash[1] != 0xbb {
		t.Fatalf("tasks: alice %+v, bob %+v", tasks[alice], tasks[bob])
	}
	hints := sw.hints()
	if len(hints) != 1 {
		t.Fatalf("hints: %+v", hints)
	}
	if h := hints[0].v.(*wire.ReleaseHint); h.StorageIndex[0] != 8 || h.Owner != alice || h.Number != 0 || h.DescriptorHash[1] != 0 {
		t.Fatalf("hint: %+v", h)
	}
	// Alice's task fails: alice's object waits, bob's does not wait with
	// it, and is not offered twice while its own task runs.
	sw.c.RecordTaskReport(identity.Peer{Device: tasks[alice].Worker}, &wire.TaskReport{ID: tasks[alice].ID, Failed: "no"})
	sw.c.RecordTaskReport(identity.Peer{Device: tasks[bob].Worker}, &wire.TaskReport{ID: tasks[bob].ID, Failed: "no"})
	sw.advance(retryAfterFailure + time.Minute)
	sw.online(100<<30, falcon, spare, third)
	sw.c.Schedule(context.Background())
	if got := sw.tasks(); len(got) != 4 {
		t.Fatalf("%d tasks after both failed and the wait ran", len(got))
	}
}

// The statements of the version before the wider key, as that version has
// them: what a hub that fails its probation goes back to.
var before = struct {
	schema                                                               []string
	insertShare, unstale, removeShare, staleGone, touch, tombstone, read string
	history, reached                                                     string
}{
	schema: []string{
		`CREATE TABLE IF NOT EXISTS shares (device BLOB NOT NULL, si BLOB NOT NULL, n INTEGER NOT NULL, owner BLOB NOT NULL,
			size INTEGER NOT NULL, k INTEGER NOT NULL, nn INTEGER NOT NULL, dhash BLOB NOT NULL, PRIMARY KEY (device, si, n))`,
		`CREATE INDEX IF NOT EXISTS shares_si ON shares(si)`,
		`CREATE TABLE IF NOT EXISTS tombstones (device BLOB NOT NULL, si BLOB NOT NULL, owner BLOB NOT NULL, dhash BLOB NOT NULL,
			purged INTEGER NOT NULL, PRIMARY KEY (device, si))`,
		`CREATE TABLE IF NOT EXISTS devices (device BLOB PRIMARY KEY, generation INTEGER NOT NULL, updated INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS objects (si BLOB PRIMARY KEY, first_seen INTEGER NOT NULL, last_change INTEGER NOT NULL, reached INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, si BLOB NOT NULL, worker BLOB NOT NULL, issued INTEGER NOT NULL,
			expires INTEGER NOT NULL, bytes INTEGER NOT NULL, task BLOB NOT NULL, state TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '', ended INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS tasks_state ON tasks(state)`,
	},
	insertShare: `INSERT OR REPLACE INTO shares (device, si, n, owner, size, k, nn, dhash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	unstale:     `UPDATE shares SET stale = 0 WHERE device = ? AND si = ? AND n = ? AND dhash = ?`,
	removeShare: `DELETE FROM shares WHERE device = ? AND si = ? AND n = ?`,
	staleGone:   `SELECT DISTINCT si FROM shares WHERE device = ? AND stale = 1`,
	touch: `INSERT INTO objects (si, first_seen, last_change, reached) VALUES (?, ?, ?, 0)
			ON CONFLICT (si) DO UPDATE SET last_change = excluded.last_change`,
	tombstone: `INSERT OR REPLACE INTO tombstones (device, si, owner, dhash, purged) VALUES (?, ?, ?, ?, ?)`,
	read:      `SELECT device, si, n, owner, size, k, nn, dhash FROM shares ORDER BY si, n, device`,
	history:   `SELECT si, last_change, reached FROM objects`,
	reached:   `UPDATE objects SET reached = 1 WHERE si = ?`,
}

// oldPlacement makes a table file as the version before the wider key
// left it, stale column included: objects of two members on two devices,
// with their history, and a tombstone.
func oldPlacement(t *testing.T, path string, objects int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range append(append([]string{}, before.schema...), `ALTER TABLE shares ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < objects; i++ {
		si := make([]byte, 16)
		si[0], si[1], si[2] = byte(i), byte(i>>8), byte(i>>16)
		dh := make([]byte, 32)
		dh[0] = byte(i)
		owner := alice
		if i%2 == 1 {
			owner = bob
		}
		for n, dev := range []identity.Key{falcon, spare} {
			if _, err := tx.Exec(before.insertShare, dev[:], si, n, owner[:], 4096, 2, 4, dh); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(before.touch, si, 1000+i, 2000+i); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, err := tx.Exec(before.reached, si); err != nil {
				t.Fatal(err)
			}
		}
		if i == 4 {
			if _, err := tx.Exec(before.tombstone, falcon[:], si, owner[:], dh, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, dev := range []identity.Key{falcon, spare} {
		if _, err := tx.Exec(`INSERT INTO devices (device, generation, updated) VALUES (?, 7, 1)`, dev[:]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO tasks (si, worker, issued, expires, bytes, task, state) VALUES (x'00', x'01', 1, 2, 3, x'', 'done')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type placementFacts struct {
	shares, bytes, tombstones, devices, tasks, objects, reached int64
	lastChange                                                  int64 // summed
}

func placementFactsOf(t *testing.T, db *sql.DB, history string) placementFacts {
	t.Helper()
	var f placementFacts
	for q, dst := range map[string][]any{
		`SELECT COUNT(*), COALESCE(SUM(size + n + k + nn), 0) FROM shares`:                          {&f.shares, &f.bytes},
		`SELECT COUNT(*) FROM tombstones`:                                                           {&f.tombstones},
		`SELECT COUNT(*) FROM devices`:                                                              {&f.devices},
		`SELECT COUNT(*) FROM tasks`:                                                                {&f.tasks},
		`SELECT COUNT(*), COALESCE(SUM(reached), 0), COALESCE(SUM(last_change), 0) FROM ` + history: {&f.objects, &f.reached, &f.lastChange},
	} {
		if err := db.QueryRow(q).Scan(dst...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return f
}

// The hub's table file from before the wider key opens with everything it
// held, the history of every object included, and the version before
// still works on it.
func TestPlacementKeepsAnOlderFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placement.sqlite")
	const objects = 500
	oldPlacement(t, path, objects)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	was := placementFactsOf(t, raw, "objects")
	_ = raw.Close()
	if was.shares != 2*objects || was.objects != objects || was.reached == 0 || was.tombstones != 1 {
		t.Fatalf("the old file is not what the test means: %+v", was)
	}
	for i := 0; i < 2; i++ { // the second start finds nothing to do
		p, err := openPlacement(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if is := placementFactsOf(t, p.db, "objects_owned"); is != was {
			t.Fatalf("open %d changed the file:\nwas %+v\nis  %+v", i, was, is)
		}
		// The history is each owner's: object 3 is bob's and was reached.
		var owner []byte
		var reached bool
		var last int64
		if err := p.db.QueryRow(`SELECT owner, reached, last_change FROM objects_owned WHERE si = ?`, append([]byte{3}, make([]byte, 15)...)).Scan(&owner, &reached, &last); err != nil {
			t.Fatal(err)
		}
		if identity.Key(owner) != bob || !reached || last != 2003 {
			t.Fatalf("object 3's history: owner %x, reached %v, last change %d", owner[:4], reached, last)
		}
		objs, grid, err := p.compute(time.Unix(5000, 0), map[identity.Key]bool{falcon: true, spare: true}, 2, manifest.Encoding{K: 2, N: 4}, nil)
		if err != nil || len(objs) != objects || grid.Tombstoned != 0 {
			t.Fatalf("open %d: %d objects, %v", i, len(objs), err)
		}
		for _, o := range objs {
			if o.Conflict || len(o.Shares) != 2 || o.Reached != ((int(o.StorageIndex[0])+int(o.StorageIndex[1])<<8)%3 == 0) {
				t.Fatalf("open %d: object %x: %+v", i, o.StorageIndex[:3], o)
			}
		}
		if err := p.close(); err != nil {
			t.Fatal(err)
		}
	}

	// The version before, back on this file.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range before.schema {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("the version before, opening the widened file: %v", err)
		}
	}
	if is := placementFactsOf(t, old, "objects"); is != was {
		t.Fatalf("the version before reads other facts:\nwas %+v\nis  %+v", was, is)
	}
	si := append([]byte{0xee, 0xee}, make([]byte, 14)...)
	dh := make([]byte, 32)
	for _, step := range []struct {
		what, q string
		args    []any
	}{
		{"a share named again", before.unstale, []any{falcon[:], make([]byte, 16), 0, dh}},
		{"a new share", before.insertShare, []any{falcon[:], si, 0, alice[:], 4096, 2, 4, dh}},
		{"the same share again", before.insertShare, []any{falcon[:], si, 0, alice[:], 4096, 2, 4, dh}},
		{"an object's history, new", before.touch, []any{si, 9000, 9000}},
		{"an object's history, changed", before.touch, []any{append([]byte{3}, make([]byte, 15)...), 9500, 9500}},
		{"reached", before.reached, []any{si}},
		{"a tombstone", before.tombstone, []any{falcon[:], si, alice[:], dh, 0}},
		{"the same tombstone again", before.tombstone, []any{falcon[:], si, alice[:], dh, 1}},
	} {
		if _, err := old.Exec(step.q, step.args...); err != nil {
			t.Fatalf("the version before, %s: %v", step.what, err)
		}
	}
	for what, q := range map[string]string{"shares": before.read, "history": before.history} {
		rows, err := old.Query(q)
		if err != nil {
			t.Fatalf("the version before, reading %s: %v", what, err)
		}
		rows.Close()
	}
	rows, err := old.Query(before.staleGone, falcon[:])
	if err != nil {
		t.Fatalf("the version before, the end of a whole list: %v", err)
	}
	rows.Close()
	var count int
	if err := old.QueryRow(`SELECT COUNT(*) FROM shares WHERE si = ?`, si).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d rows for one share the version before wrote twice: %v", count, err)
	}
	if err := old.QueryRow(`SELECT COUNT(*) FROM tombstones WHERE si = ?`, si).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d rows for one tombstone the version before wrote twice: %v", count, err)
	}
	if res, err := old.Exec(before.removeShare, spare[:], make([]byte, 16), 1); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("the version before removed %d rows", n)
	}
	_ = old.Close()

	// This version again: what that one wrote in between is there, the
	// history it kept in its own table included.
	p, err := openPlacement(path)
	if err != nil {
		t.Fatalf("this version, after the version before ran: %v", err)
	}
	defer p.close()
	var reached bool
	var last int64
	if err := p.db.QueryRow(`SELECT reached, last_change FROM objects_owned WHERE owner = ? AND si = ?`, alice[:], si).Scan(&reached, &last); err != nil || !reached || last != 9000 {
		t.Fatalf("the history of an object the version before first saw: %v %v %d", err, reached, last)
	}
	if err := p.db.QueryRow(`SELECT last_change FROM objects_owned WHERE owner = ? AND si = ?`, bob[:], append([]byte{3}, make([]byte, 15)...)).Scan(&last); err != nil || last != 9500 {
		t.Fatalf("the history the version before changed: %v %d", err, last)
	}
	if is := placementFactsOf(t, p.db, "objects_owned"); is.shares != was.shares || is.objects != was.objects+1 {
		t.Fatalf("after the version before ran: %+v", is)
	}
}

// TestPlacementCopy widens a copy of a live hub's placement table: the
// check asked for before a release that changes the tables. It runs only
// with RN3_PLACEMENT_COPY set to a copy of placement.sqlite, which it
// does not change: it works on a copy of its own.
func TestPlacementCopy(t *testing.T) {
	src := os.Getenv("RN3_PLACEMENT_COPY")
	if src == "" {
		t.Skip("RN3_PLACEMENT_COPY is not set")
	}
	path := filepath.Join(t.TempDir(), "placement.sqlite")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	_ = out.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	was := placementFactsOf(t, raw, "objects")
	var owned int64
	if err := raw.QueryRow(`SELECT COUNT(*) FROM (SELECT owner, si FROM shares UNION SELECT owner, si FROM tombstones) p JOIN objects o ON o.si = p.si`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	start := time.Now()
	p, err := openPlacement(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	took := time.Since(start)
	is := placementFactsOf(t, p.db, "objects_owned")
	t.Logf("widened in %s: %d shares, %d tombstones, %d tasks, %d devices; %d objects with a history before, %d after", took.Round(time.Millisecond), is.shares, is.tombstones, is.tasks, is.devices, was.objects, is.objects)
	if is.shares != was.shares || is.bytes != was.bytes || is.tombstones != was.tombstones || is.tasks != was.tasks || is.devices != was.devices {
		t.Fatalf("the rows changed:\nwas %+v\nis  %+v", was, is)
	}
	// Every object the rows name keeps its history; a history with no row
	// left to say whose it is has no owner to go to.
	if is.objects != owned {
		t.Fatalf("%d objects have a history after, %d of those the rows name had one before", is.objects, owned)
	}
	alive := map[identity.Key]bool{}
	for _, d := range p.devices() {
		alive[d] = true
	}
	objs, grid, err := p.compute(time.Now(), alive, len(alive), manifest.Encoding{K: 2, N: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d objects on %d servers: %d placed, %d under-placed, %d unreadable, %d conflicts, %d surplus", len(objs), grid.Servers, grid.Placed, grid.UnderPlaced, grid.Unreadable, grid.Conflicts, grid.Surplus)
}
