package collector

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// share is one complete share of a 2-of-4 object as a node reports it.
func share(si byte, n uint8, owner identity.Key) wire.ShareEntry {
	var e wire.ShareEntry
	e.StorageIndex[0], e.Number, e.Owner, e.Size, e.K, e.N = si, n, owner, 4096, 2, 4
	e.DescriptorHash[0] = si
	return e
}

func tombstone(si byte, owner identity.Key) wire.TombstoneEntry {
	t := wire.TombstoneEntry{Owner: owner}
	t.StorageIndex[0], t.DescriptorHash[0] = si, si
	return t
}

func (w *world) heartbeat(dev identity.Key) {
	w.c.Record(identity.Peer{Device: dev, Member: alice}, "udp", wire.Census{Roles: []string{identity.RoleStorage}})
}

func (w *world) report(dev identity.Key, gen uint64, entries ...wire.ShareEntry) {
	w.c.RecordShares(identity.Peer{Device: dev, Member: alice}, &wire.ShareCensus{Since: 0, Generation: gen, Added: entries})
}

func objectsBySI(t *testing.T, w *world) (map[byte]Object, *Grid) {
	t.Helper()
	objs, grid, err := w.c.Objects()
	if err != nil {
		t.Fatal(err)
	}
	out := map[byte]Object{}
	for _, o := range objs {
		out[o.StorageIndex[0]] = o
	}
	return out, grid
}

func TestPlacementRules(t *testing.T) {
	w := newWorld(t, "", "")
	w.heartbeat(falcon)
	w.heartbeat(spare)
	// 1: fully placed, two shares per server. 2: three of four numbers on
	// one server. 3: fully placed with one share number held twice. 4: one
	// share only. 5: every number on one server, the other holds nothing.
	// 6: one share on falcon and a tombstone on both servers: deleted.
	w.report(falcon, 1, share(1, 0, alice), share(1, 1, alice), share(2, 0, alice), share(2, 1, alice), share(2, 2, alice),
		share(3, 0, alice), share(3, 1, alice), share(3, 2, alice), share(4, 0, bob), share(5, 0, alice), share(5, 1, alice), share(5, 2, alice), share(5, 3, alice), share(6, 0, alice))
	w.report(spare, 1, share(1, 2, alice), share(1, 3, alice), share(3, 2, alice), share(3, 3, alice))
	for _, dev := range []identity.Key{falcon, spare} {
		w.c.RecordShares(identity.Peer{Device: dev, Member: alice}, &wire.ShareCensus{Since: 1, Generation: 2, Tombstones: []wire.TombstoneEntry{tombstone(6, alice)}})
	}

	objs, grid := objectsBySI(t, w)
	if o := objs[1]; !o.Placed || len(o.Shares) != 4 || o.Spread != 2 || o.Target != 2 || o.Need != "" || !o.Readable {
		t.Fatalf("object 1: %+v", o)
	}
	if o := objs[2]; o.Placed || len(o.Missing) != 1 || o.Missing[0] != 3 || o.Need != "quiescing" || !o.Readable {
		t.Fatalf("object 2: %+v", o)
	}
	if o := objs[3]; !o.Placed || o.Surplus != 1 {
		t.Fatalf("object 3: %+v", o)
	}
	if o := objs[4]; o.Readable || o.Placed || o.Owner != bob {
		t.Fatalf("object 4: %+v", o)
	}
	if o := objs[5]; o.Placed || len(o.Shares) != 4 || o.Spread != 1 || o.Need != "quiescing" {
		t.Fatalf("object 5: %+v", o)
	}
	if o := objs[6]; !o.Tombstoned || o.Need != "" {
		t.Fatalf("object 6: %+v", o)
	}
	if grid.Servers != 2 || grid.Objects != 6 || grid.Placed != 2 || grid.UnderPlaced != 3 || grid.Unreadable != 1 || grid.Surplus != 1 || grid.Tombstoned != 1 || grid.Quiescing != 3 || grid.Repair != 0 {
		t.Fatalf("grid: %+v", grid)
	}
	if grid.Copies["4"] != 3 || grid.Copies["3"] != 1 || grid.Copies["1"] != 1 {
		t.Fatalf("copies: %v", grid.Copies)
	}
	st := w.c.Status()
	for _, m := range st.Members {
		switch m.Name {
		case "alice":
			if m.UnderPlaced != 2 || m.Unreadable != 0 {
				t.Fatalf("alice: %+v", m)
			}
		case "bob":
			if m.UnderPlaced != 1 || m.Unreadable != 1 {
				t.Fatalf("bob: %+v", m)
			}
		}
	}

	// The quiescence window passes: the short objects become repair work.
	w.advance(25 * time.Hour)
	w.heartbeat(falcon)
	w.heartbeat(spare)
	objs, grid = objectsBySI(t, w)
	if objs[2].Need != "repair" || objs[5].Need != "repair" || grid.Repair != 2 || grid.BeyondRepair != 1 || grid.Quiescing != 0 {
		t.Fatalf("after quiescence: %+v %+v", objs[2], grid)
	}

	// spare goes quiet: down after ten minutes, its shares still count.
	w.advance(11 * time.Minute)
	w.heartbeat(falcon)
	objs, grid = objectsBySI(t, w)
	if !objs[1].Placed || grid.Servers != 2 {
		t.Fatalf("spare down: %+v %+v", objs[1], grid)
	}
	// Dead after repair_after_hours: object 1 lost half its shares and
	// needs repair at once, because it had been fully placed.
	w.advance(73 * time.Hour)
	w.heartbeat(falcon)
	objs, grid = objectsBySI(t, w)
	if o := objs[1]; o.Placed || len(o.Shares) != 2 || !o.Readable || !o.Reached || o.Need != "repair" || o.Target != 1 {
		t.Fatalf("spare dead: %+v", o)
	}
	// Object 6 stays deleted on the word of the one server left.
	if grid.Servers != 1 || grid.Tombstoned != 1 {
		t.Fatalf("spare dead: %+v", grid)
	}
	// The dead server's bytes are still on its disk: usage counts them.
	st = w.c.Status()
	for _, m := range st.Members {
		if m.Name == "alice" && m.StoredBytes != 17*4096 {
			t.Fatalf("usage with a dead server: %+v", m)
		}
	}
	// The page says it in words, with no template slip.
	rec := httptest.NewRecorder()
	w.c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	page := rec.Body.String()
	for _, want := range []string{"6 objects", "under-placed", "unreadable", "objects due", "beyond repair", "checked since"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "%!") {
		t.Fatal("page has a formatting error")
	}
}

func TestPlacementSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	census, table := filepath.Join(dir, "census.json"), filepath.Join(dir, "placement.sqlite")
	w := newWorldWith(t, demoManifest(), census, table, "")
	w.heartbeat(falcon)
	w.heartbeat(spare)
	w.report(falcon, 1, share(1, 0, alice), share(1, 1, alice))
	w.report(spare, 1, share(1, 2, alice), share(1, 3, alice))
	if objs, _ := objectsBySI(t, w); !objs[1].Placed {
		t.Fatalf("before: %+v", objs[1])
	}
	if err := w.c.Save(); err != nil {
		t.Fatal(err)
	}
	_ = w.c.Close()

	// The hub restarts while spare is down: spare's shares still count,
	// and the object's history says it was placed, so a removal on
	// falcon is a repair at once, not a quiescence.
	w2 := newWorldWith(t, demoManifest(), census, table, "")
	w2.advance(20 * time.Minute)
	w2.heartbeat(falcon)
	objs, grid := objectsBySI(t, w2)
	if o := objs[1]; !o.Placed || len(o.Shares) != 4 || grid.Servers != 2 {
		t.Fatalf("after restart: %+v %+v", o, grid)
	}
	w2.c.RecordShares(identity.Peer{Device: falcon, Member: alice}, &wire.ShareCensus{Since: 1, Generation: 2, Removed: []wire.ShareKey{{StorageIndex: [16]byte{1}, Number: 0}}})
	objs, _ = objectsBySI(t, w2)
	if o := objs[1]; o.Placed || o.Need != "repair" || len(o.Missing) != 1 || o.Missing[0] != 0 {
		t.Fatalf("after a loss: %+v", o)
	}
	// A census from zero under a new generation replaces the table: falcon
	// was rebuilt and holds one share now.
	w2.report(falcon, 1, share(1, 1, alice))
	objs, _ = objectsBySI(t, w2)
	if o := objs[1]; len(o.Shares) != 3 || o.Holders[falcon][0] != 1 || len(o.Holders[falcon]) != 1 {
		t.Fatalf("after a rebuild: %+v", o)
	}
}

func TestRetiredServerIsForgotten(t *testing.T) {
	m := demoManifest()
	w := newWorldWith(t, m, "", "", "")
	w.heartbeat(falcon)
	w.heartbeat(spare)
	w.report(falcon, 1, share(1, 0, alice), share(1, 1, alice))
	w.report(spare, 1, share(1, 2, alice), share(1, 3, alice))
	m.Storage = m.Storage[:1] // spare retired
	objs, grid := objectsBySI(t, w)
	if o := objs[1]; len(o.Shares) != 2 || grid.Servers != 1 {
		t.Fatalf("retired: %+v %+v", o, grid)
	}
	st := w.c.Status()
	for _, mm := range st.Members {
		if mm.Name == "alice" && mm.StoredBytes != 2*4096 {
			t.Fatalf("usage after retirement: %+v", mm)
		}
	}
}

func TestDescriptorConflictFollowsTheMajority(t *testing.T) {
	w := newWorld(t, "", "")
	w.heartbeat(falcon)
	w.heartbeat(spare)
	odd := share(1, 3, alice)
	odd.DescriptorHash[1] = 0xff
	w.report(falcon, 1, share(1, 0, alice), share(1, 1, alice))
	w.report(spare, 1, share(1, 2, alice), odd)
	objs, grid := objectsBySI(t, w)
	if o := objs[1]; !o.Conflict || len(o.Shares) != 3 || o.Placed || grid.Conflicts != 1 {
		t.Fatalf("conflict: %+v %+v", o, grid)
	}
}

// A tombstone is one server's word. It takes an object out of repair
// only when most of the servers concerned report it, for the object's own
// owner and descriptor; one server alone cannot switch repair off for an
// object the others hold.
func TestATombstoneNeedsMostOfTheServersConcerned(t *testing.T) {
	third := bobPC
	m := demoManifest()
	m.Storage = append(m.Storage, manifest.StorageDevice{Member: "bob", Device: third, Name: "third"})
	w := newWorldWith(t, m, "", "", "")
	for _, dev := range []identity.Key{falcon, spare, third} {
		w.heartbeat(dev)
	}
	// 1: held by falcon and spare, a tombstone from third only.
	// 2: held by all three, tombstones from two of them.
	// 3: held by falcon, a tombstone from spare only: a tie is not most.
	// 4: held by falcon and spare, tombstones from both for another owner.
	// 5: held by falcon and spare, tombstones from both for another descriptor.
	// 6: held by falcon, tombstones from spare and third, which hold nothing.
	w.report(falcon, 1, share(1, 0, alice), share(2, 0, alice), share(3, 0, alice), share(4, 0, alice), share(5, 0, alice), share(6, 0, alice))
	w.report(spare, 1, share(1, 1, alice), share(2, 1, alice), share(4, 1, alice), share(5, 1, alice))
	w.report(third, 1, share(2, 2, alice))
	other := tombstone(5, alice)
	other.DescriptorHash[1] = 0xff
	said := map[identity.Key][]wire.TombstoneEntry{
		falcon: {tombstone(2, alice), tombstone(4, bob), other},
		spare:  {tombstone(2, alice), tombstone(3, alice), tombstone(4, bob), other, tombstone(6, alice)},
		third:  {tombstone(1, alice), tombstone(6, alice)},
	}
	for dev, ts := range said {
		w.c.RecordShares(identity.Peer{Device: dev, Member: alice}, &wire.ShareCensus{Since: 1, Generation: 2, Tombstones: ts})
	}
	objs, grid := objectsBySI(t, w)
	for si, want := range map[byte]bool{1: false, 2: true, 3: false, 4: false, 5: false, 6: true} {
		o := objs[si]
		if o.Tombstoned != want {
			t.Errorf("object %d: tombstoned %v, want %v", si, o.Tombstoned, want)
		}
		if !want && o.Need == "" {
			t.Errorf("object %d: out of repair on a tombstone that does not count", si)
		}
	}
	if grid.Tombstoned != 2 {
		t.Errorf("grid: %d tombstoned", grid.Tombstoned)
	}
}

// A server has one vote on an object's descriptor however many rows it
// reports under another one.
func TestOneServerDoesNotOutvoteTheOthers(t *testing.T) {
	third := bobPC
	m := demoManifest()
	m.Storage = append(m.Storage, manifest.StorageDevice{Member: "bob", Device: third, Name: "third"})
	w := newWorldWith(t, m, "", "", "")
	for _, dev := range []identity.Key{falcon, spare, third} {
		w.heartbeat(dev)
	}
	var loud []wire.ShareEntry
	for n := uint8(0); n < 4; n++ {
		e := share(1, n, alice)
		e.DescriptorHash[1] = 0xff
		loud = append(loud, e)
	}
	w.report(falcon, 1, share(1, 0, alice))
	w.report(spare, 1, share(1, 1, alice))
	w.report(third, 1, loud...)
	objs, _ := objectsBySI(t, w)
	if o := objs[1]; !o.Conflict || o.Hash[1] != 0 || len(o.Shares) != 2 || o.Placed {
		t.Fatalf("object: %+v", o)
	}
}

var _ = manifest.Manifest{}

// A transition in the manifest: the page shows the conversion, the old
// encoding's bytes are accounted at zero until the deadline, and per
// member the objects still to convert.
func TestConversionProgress(t *testing.T) {
	m := demoManifest()
	m.Network.Encoding = manifest.Encoding{K: 2, N: 3}
	m.Network.EncodingTransition = &manifest.Transition{From: manifest.Encoding{K: 2, N: 4}, To: manifest.Encoding{K: 2, N: 3}, Deadline: "2026-11-10"}
	w := newWorldWith(t, m, "", "", "")
	w.heartbeat(falcon)
	w.heartbeat(spare)
	narrow := func(si byte, n uint8) wire.ShareEntry {
		e := share(si, n, alice)
		e.N = 3
		return e
	}
	// 1: converted, three shares of 2-of-3 over two servers. 2: still
	// 2-of-4. 3: 2-of-4, bob's.
	w.report(falcon, 1, narrow(1, 0), narrow(1, 1), share(2, 0, alice), share(2, 1, alice), share(3, 0, bob), share(3, 1, bob))
	w.report(spare, 1, narrow(1, 2), share(2, 2, alice), share(2, 3, alice), share(3, 2, bob), share(3, 3, bob))
	st := w.c.Status()
	c := st.Grid.Conversion
	if c == nil || c.From != "2-of-4" || c.To != "2-of-3" || c.Deadline != "2026-11-10" || c.Counting || c.Objects != 3 || c.Converted != 1 || c.OldBytes != 8*4096 {
		t.Fatalf("conversion: %+v", c)
	}
	for _, mm := range st.Members {
		switch mm.Name {
		case "alice":
			if mm.OldEncoding != 1 || mm.OldBytes != 4*4096 || mm.StoredBytes != 3*4096 || mm.Objects != 2 {
				t.Fatalf("alice: %+v", mm)
			}
		case "bob":
			if mm.OldEncoding != 1 || mm.StoredBytes != 0 || mm.Objects != 1 {
				t.Fatalf("bob before the deadline: %+v", mm)
			}
		}
	}
	rec := httptest.NewRecorder()
	w.c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	page := rec.Body.String()
	for _, want := range []string{"1 of 3 objects converted", "not counted yet", `value="1" max="3"`, "1 to convert"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q:\n%s", want, page)
		}
	}
	// Past the deadline, the end of that day, the old shares count again.
	w.advance(9 * 24 * time.Hour)
	w.heartbeat(falcon)
	w.heartbeat(spare)
	st = w.c.Status()
	if !st.Grid.Conversion.Counting {
		t.Fatalf("past the deadline: %+v", st.Grid.Conversion)
	}
	for _, mm := range st.Members {
		if mm.Name == "bob" && mm.StoredBytes != 4*4096 {
			t.Fatalf("bob past the deadline: %+v", mm)
		}
	}
	// Without a transition there is no conversion to show.
	m.Network.EncodingTransition = nil
	if st := w.c.Status(); st.Grid.Conversion != nil {
		t.Fatal("a conversion shown without a transition")
	}
}
