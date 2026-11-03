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
	w.report(falcon, 1, share(1, 0, alice), share(1, 1, alice), share(2, 0, alice), share(2, 1, alice), share(2, 2, alice),
		share(3, 0, alice), share(3, 1, alice), share(3, 2, alice), share(4, 0, bob), share(5, 0, alice), share(5, 1, alice), share(5, 2, alice), share(5, 3, alice))
	w.report(spare, 1, share(1, 2, alice), share(1, 3, alice), share(3, 2, alice), share(3, 3, alice))
	w.c.RecordShares(identity.Peer{Device: spare, Member: alice}, &wire.ShareCensus{Since: 1, Generation: 2, Tombstones: []wire.TombstoneEntry{{StorageIndex: [16]byte{6}, Owner: alice}}})

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
	if grid.Servers != 2 || grid.Objects != 5 || grid.Placed != 2 || grid.UnderPlaced != 3 || grid.Unreadable != 1 || grid.Surplus != 1 || grid.Tombstoned != 1 || grid.Quiescing != 3 || grid.Repair != 0 {
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
	if objs[2].Need != "repair" || objs[5].Need != "repair" || grid.Repair != 3 || grid.Quiescing != 0 {
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
	if grid.Servers != 1 || grid.Tombstoned != 0 {
		t.Fatalf("spare dead: %+v", grid)
	}
	// The dead server's bytes are still on its disk: usage counts them.
	st = w.c.Status()
	for _, m := range st.Members {
		if m.Name == "alice" && m.StoredBytes != 16*4096 {
			t.Fatalf("usage with a dead server: %+v", m)
		}
	}
	// The page says it in words, with no template slip.
	rec := httptest.NewRecorder()
	w.c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	page := rec.Body.String()
	for _, want := range []string{"5 objects", "under-placed", "unreadable", "objects due", "checked since"} {
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

var _ = manifest.Manifest{}
