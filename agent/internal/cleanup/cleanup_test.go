package cleanup

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// world is a fake grid with a member's roots: this device's and another
// device's, each with an index, snapshots and directories.
type world struct {
	t      *testing.T
	f      *engine.Fake
	member identity.Key
	device identity.Key
	now    time.Time
}

func newWorld(t *testing.T) *world {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	return &world{t: t, f: engine.NewFake(member, device, 2, 4), member: member, device: device, now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
}

func (w *world) put(b []byte) object.ReadCap {
	w.t.Helper()
	rc, err := w.f.Put(context.Background(), bytes.NewReader(b), int64(len(b)))
	if err != nil {
		w.t.Fatal(err)
	}
	return rc
}

// tree stores a directory with the given files and subdirectories.
func (w *world) tree(files map[string][]byte, subs map[string]string) string {
	var entries []snapshot.Entry
	for name, b := range files {
		rc := w.put(b)
		entries = append(entries, snapshot.Entry{Name: []byte(name), UTF8: true, Kind: snapshot.KindFile, Size: int64(len(b)), Cap: rc.String()})
	}
	for name, capStr := range subs {
		entries = append(entries, snapshot.Entry{Name: []byte(name), UTF8: true, Kind: snapshot.KindDir, Cap: capStr})
	}
	d := snapshot.NewDirectory(entries)
	b, _ := d.Encode()
	return w.put(b).String()
}

// index stores an index with one snapshot per top directory and returns
// its capability.
func (w *world) index(device identity.Key, tops ...string) string {
	x := &snapshot.Index{Version: snapshot.IndexVersion, Device: device.String()}
	for i, top := range tops {
		x.Snapshots = append(x.Snapshots, snapshot.Snapshot{ID: string(rune('a' + i)), Started: w.now, Top: top})
	}
	b, _ := x.Encode()
	return w.put(b).String()
}

func (w *world) run(dry bool) (*Report, error) {
	return Run(context.Background(), Config{Grid: w.f, Member: w.member, DryRun: dry, Now: func() time.Time { return w.now }})
}

func TestCleanupDeletesOnlyTheUnreferenced(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// This device: a snapshot of two files in a subfolder. Another device:
	// one file, in a chunked form. Two objects nothing references.
	sub := w.tree(map[string][]byte{"a.txt": []byte("alpha"), "b.txt": []byte("bravo")}, nil)
	top := w.tree(nil, map[string]string{"folder": sub})
	chunk1, chunk2 := w.put(bytes.Repeat([]byte("c"), 300)), w.put(bytes.Repeat([]byte("d"), 300))
	otherTop := func() string {
		d := snapshot.NewDirectory([]snapshot.Entry{{Name: []byte("big"), UTF8: true, Kind: snapshot.KindFile, Size: 600, Chunks: []string{chunk1.String(), chunk2.String()}}})
		b, _ := d.Encode()
		return w.put(b).String()
	}()
	if _, err := w.f.PutRoot(ctx, engine.Root{IndexCap: w.index(w.device, top)}); err != nil {
		t.Fatal(err)
	}
	var other identity.Key
	other[0] = 3
	w.f.AddRoot(other, engine.Root{IndexCap: w.index(other, otherTop)})
	garbage1, garbage2 := w.put([]byte("old chunk nobody references")), w.put([]byte("another"))

	// A dry run counts and sends nothing.
	rep, err := w.run(true)
	if err != nil {
		t.Fatal(err)
	}
	// Reachable: 2 indexes, 3 directories (top, sub, otherTop), 2 files, 2 chunks.
	if rep.Roots != 2 || rep.Reachable != 9 || rep.Listed != 11 || rep.Garbage != 2 || rep.Tombstoned != 0 || !rep.DryRun || w.f.Deletes != 0 {
		t.Fatalf("dry run: %+v", rep)
	}
	rep, err = w.run(false)
	if err != nil || rep.Tombstoned != 2 || rep.Garbage != 2 || w.f.Deletes != 2 {
		t.Fatalf("run: %+v %v", rep, err)
	}
	if !w.f.Tombstoned(garbage1) || !w.f.Tombstoned(garbage2) {
		t.Fatal("the garbage is not tombstoned")
	}
	for _, rc := range []object.ReadCap{chunk1, chunk2} {
		if w.f.Tombstoned(rc) {
			t.Fatal("a referenced chunk was tombstoned")
		}
	}
	// The next run finds the tombstoned ones already done.
	rep, _ = w.run(false)
	if rep.Garbage != 0 || rep.Tombstoned != 0 || w.f.Deletes != 2 {
		t.Fatalf("second run: %+v", rep)
	}
	// The other device's root goes: its objects become garbage, except the
	// ones this device's root shares.
	w.f.DropRoot(other)
	rep, _ = w.run(false)
	if rep.Roots != 1 || rep.Garbage != 4 || rep.Tombstoned != 4 || !w.f.Tombstoned(chunk1) {
		t.Fatalf("after dropping a root: %+v", rep)
	}
}

func TestCleanupRefusesWhenUnsure(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// A root whose index cannot be read: nothing is deleted.
	w.put([]byte("garbage"))
	var bogus object.ReadCap
	bogus.Size = 10
	if _, err := w.f.PutRoot(ctx, engine.Root{IndexCap: bogus.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.run(false); !errors.Is(err, ErrUnreadable) || w.f.Deletes != 0 {
		t.Fatalf("unreadable index: %v, %d deletes", err, w.f.Deletes)
	}
	// A root that does not open with the member's key: the same.
	w2 := newWorld(t)
	w2.put([]byte("garbage"))
	var other identity.Key
	other[0] = 9
	w2.f.AddRoot(other, engine.Root{})
	w2.f.SetUnopenable(other)
	if _, err := w2.run(false); !errors.Is(err, ErrUnreadable) || w2.f.Deletes != 0 {
		t.Fatalf("unopenable root: %v", err)
	}
	// Too much garbage at once is refused without force.
	w3 := newWorld(t)
	for i := 0; i < 150; i++ {
		w3.put([]byte{byte(i), byte(i >> 8), 7})
	}
	if _, err := w3.f.PutRoot(ctx, engine.Root{IndexCap: w3.index(w3.device)}); err != nil {
		t.Fatal(err)
	}
	if _, err := w3.run(false); !errors.Is(err, ErrTooMuch) || w3.f.Deletes != 0 {
		t.Fatalf("mass deletion: %v", err)
	}
	rep, err := Run(ctx, Config{Grid: w3.f, Member: w3.member, Force: true, Now: func() time.Time { return w3.now }})
	if err != nil || rep.Tombstoned != 150 {
		t.Fatalf("forced: %+v %v", rep, err)
	}
	// Too few servers answering the roots query.
	w4 := newWorld(t)
	w4.put([]byte("garbage"))
	w4.f.RootsAnswered = 0
	if _, err := w4.run(false); !errors.Is(err, ErrQuorum) {
		t.Fatalf("quorum: %v", err)
	}
}

func TestRevokedRootsStopCountingAfterTheWindow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var old identity.Key
	old[0] = 5
	lost := w.put([]byte("only the old laptop referenced this"))
	oldTop := func() string {
		d := snapshot.NewDirectory([]snapshot.Entry{{Name: []byte("f"), UTF8: true, Kind: snapshot.KindFile, Size: 5, Cap: lost.String()}})
		b, _ := d.Encode()
		return w.put(b).String()
	}()
	w.f.AddRoot(old, engine.Root{IndexCap: w.index(old, oldTop)})
	if _, err := w.f.PutRoot(ctx, engine.Root{IndexCap: w.index(w.device)}); err != nil {
		t.Fatal(err)
	}
	var seed identity.Seed
	seed[0] = 1
	rev, err := identity.Revoke(seed, identity.Revocation{Network: "testnet", Device: old, Serial: 1, RevokedAt: w.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	rev.Revocation.Member = w.member // the fake member key is not derived from a seed
	revs := func() []*identity.SignedRevocation { return []*identity.SignedRevocation{rev} }
	cfg := Config{Grid: w.f, Member: w.member, Revocations: revs, Now: func() time.Time { return w.now }}
	rep, err := Run(ctx, cfg)
	if err != nil || rep.Roots != 2 || rep.Garbage != 0 {
		t.Fatalf("within the window the revoked root still counts: %+v %v", rep, err)
	}
	w.now = w.now.Add(31 * 24 * time.Hour)
	rep, err = Run(ctx, cfg)
	if err != nil || rep.Roots != 1 || rep.RootsSkipped != 1 || rep.Garbage != 3 || !w.f.Tombstoned(lost) {
		t.Fatalf("past the window: %+v %v", rep, err)
	}
}

// A conversion leaves every object once more under the old encoding, which
// is more than half of what the grid holds, with a reachable set as large
// as before. The bound is against a wrong reachable set, so it holds a run
// only when the set shrank, or when no earlier run says how large it was.
func TestTheBoundLetsARunGoOnWhenTheReachableSetIsWhatItWas(t *testing.T) {
	ctx := context.Background()
	garbageWorld := func() (*world, int) {
		w := newWorld(t)
		for i := 0; i < 150; i++ {
			w.put([]byte{byte(i), byte(i >> 8), 7})
		}
		if _, err := w.f.PutRoot(ctx, engine.Root{IndexCap: w.index(w.device)}); err != nil {
			t.Fatal(err)
		}
		rep, err := Run(ctx, Config{Grid: w.f, Member: w.member, DryRun: true, Force: true, Now: func() time.Time { return w.now }})
		if err != nil || rep.Reachable == 0 || rep.Garbage != 150 {
			t.Fatalf("dry run: %+v %v", rep, err)
		}
		if rep.LastReachable != 0 {
			t.Fatalf("a dry run set the size of the last completed run: %+v", rep)
		}
		return w, rep.Reachable
	}
	run := func(w *world, last int) (*Report, error) {
		return Run(ctx, Config{Grid: w.f, Member: w.member, LastReachable: last, Now: func() time.Time { return w.now }})
	}

	// No earlier run: held, and the report carries nothing on.
	w, reachable := garbageWorld()
	rep, err := run(w, 0)
	if !errors.Is(err, ErrTooMuch) || w.f.Deletes != 0 || rep.LastReachable != 0 || rep.Baseline() != 0 {
		t.Fatalf("no earlier run: %+v %v, %d deletes", rep, err, w.f.Deletes)
	}
	// The set was ten times larger at the last completed run: held, and
	// the report carries that size on, so the next run is held as well.
	rep, err = run(w, reachable*10)
	if !errors.Is(err, ErrTooMuch) || w.f.Deletes != 0 || rep.Baseline() != reachable*10 {
		t.Fatalf("a shrunken set: %+v %v, %d deletes", rep, err, w.f.Deletes)
	}
	// The set is what it was: the run goes on, and its own size is what
	// the next run compares with.
	rep, err = run(w, reachable)
	if err != nil || rep.Tombstoned != 150 || rep.Baseline() != reachable {
		t.Fatalf("an intact set: %+v %v", rep, err)
	}

	// A report written before reports carried the size counts when it
	// completed, and not when it failed or was a dry run.
	if n := (&Report{Reachable: 7}).Baseline(); n != 7 {
		t.Fatalf("a completed report of an older version: %d", n)
	}
	if n := (&Report{Reachable: 7, Error: "held"}).Baseline(); n != 0 {
		t.Fatalf("a failed report of an older version: %d", n)
	}
	if n := (&Report{Reachable: 7, DryRun: true}).Baseline(); n != 0 {
		t.Fatalf("a dry run of an older version: %d", n)
	}
	if n := (*Report)(nil).Baseline(); n != 0 {
		t.Fatalf("no report: %d", n)
	}
	// A forced run completes and sets the size, whatever it was before.
	w2, reachable2 := garbageWorld()
	rep, err = Run(ctx, Config{Grid: w2.f, Member: w2.member, Force: true, LastReachable: reachable2 * 10, Now: func() time.Time { return w2.now }})
	if err != nil || rep.Baseline() != reachable2 {
		t.Fatalf("forced: %+v %v", rep, err)
	}
}
