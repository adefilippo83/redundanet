package collector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

func key(b byte) identity.Key {
	var k identity.Key
	k[0], k[31] = b, b
	return k
}

type world struct {
	c    *Collector
	mu   sync.Mutex
	now  time.Time
	live hub.Status
}

func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	w.now = w.now.Add(d)
	w.mu.Unlock()
}

func (w *world) setLive(s hub.Status) {
	w.mu.Lock()
	w.live = s
	w.mu.Unlock()
}

var (
	alice, bob, hubKey   = key(1), key(2), key(9)
	falcon, bobPC, spare = key(11), key(12), key(13)
)

func demoManifest() *manifest.Manifest {
	five := 5
	return &manifest.Manifest{Revision: 7, Network: manifest.Network{Name: "demo", RepairAfterHours: 72},
		Hubs:    []manifest.Hub{{Name: "hub", Key: hubKey}},
		Members: []manifest.Member{{Name: "alice", Key: alice, ContributionGB: 100, Status: "active"}, {Name: "bob", Key: bob, AllocationGB: &five, Status: "active"}},
		Storage: []manifest.StorageDevice{{Member: "alice", Device: falcon, Name: "falcon"}, {Member: "alice", Device: spare, Name: "spare"}}}
}

func newWorld(t *testing.T, path string) *world {
	t.Helper()
	return newWorldWith(t, demoManifest(), path, "")
}

func newWorldWith(t *testing.T, m *manifest.Manifest, path, placement string) *world {
	t.Helper()
	w := &world{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	c, err := New(Config{Device: hubKey, Manifest: func() *manifest.Manifest { return m }, Path: path, Placement: placement,
		Started: w.now.Add(-time.Hour), Now: func() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.now }})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLive(func() hub.Status { w.mu.Lock(); defer w.mu.Unlock(); return w.live })
	w.c = c
	t.Cleanup(func() { _ = c.Close() })
	return w
}

func TestStatusPageAndJSON(t *testing.T) {
	w := newWorld(t, "")
	w.setLive(hub.Status{Clients: []hub.ClientStatus{{Device: falcon.String(), Member: alice.String(), Carrier: "udp", Observed: "203.0.113.5:4000", Since: w.now.Add(-time.Minute), BytesIn: 2048, BytesOut: 4096}}})
	w.c.Record(identity.Peer{Device: falcon, Member: alice}, "udp", wire.Census{Version: "3.0.0-alpha.1", Uptime: 50 * 3600, OS: "linux/arm64", Roles: []string{"client", "storage"}, Manifest: 7,
		Storage: &wire.CensusStorage{FreeBytes: 40 << 30, TotalBytes: 100 << 30, State: "ok"},
		Peers:   []wire.CensusPeer{{Device: bobPC, Direct: true, Relay: true}, {Device: spare, Relay: true}}})
	w.c.Record(identity.Peer{Device: bobPC, Member: bob}, "tcp", wire.Census{Version: "3.0.0-alpha.1", OS: "windows/amd64", Roles: []string{"client"}, Manifest: 7})

	st := w.c.Status()
	if st.Network != "demo" || st.ManifestRevision != 7 || len(st.Devices) != 3 {
		t.Fatalf("status: %+v", st)
	}
	if got := []string{st.Devices[0].Device, st.Devices[1].Device, st.Devices[2].Device}; got[0] != falcon.String() || got[1] != bobPC.String() || got[2] != spare.String() {
		t.Fatalf("order: online, recent, never; got %v", got)
	}
	f, b, s := st.Devices[0], st.Devices[1], st.Devices[2]
	if f.Name != "falcon" || f.Member != "alice" || f.State != "online" || f.Carrier != "udp" || f.Version != "3.0.0-alpha.1" ||
		f.FreeBytes == nil || *f.FreeBytes != 40<<30 || f.Direct != 1 || f.Relayed != 1 || f.RelayIn != 2048 || f.RelayOut != 4096 || f.Manifest != 7 {
		t.Fatalf("falcon: %+v", f)
	}
	if b.Name != "" || b.Member != "bob" || b.State != "recent" || len(b.Roles) != 1 || b.Roles[0] != "client" || b.FreeBytes != nil {
		t.Fatalf("bob's device: %+v", b)
	}
	if s.Name != "spare" || s.State != "never" || s.Member != "alice" || len(s.Roles) != 1 || s.Roles[0] != "storage" {
		t.Fatalf("spare: %+v", s)
	}
	if len(st.Members) != 2 || st.Members[0].Name != "alice" || st.Members[0].Devices != 2 || st.Members[0].Online != 1 || st.Members[0].ContributionGB != 100 ||
		st.Members[1].Name != "bob" || st.Members[1].Devices != 1 || st.Members[1].Online != 0 {
		t.Fatalf("members: %+v", st.Members)
	}

	srv := httptest.NewServer(w.c.Handler())
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	code, page := get("/")
	for _, want := range []string{"falcon", "3.0.0-alpha.1", "40 GB of 100 GB", "reconnecting", "never seen", "status.json"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if code != 200 || strings.Contains(page, "203.0.113.5") || strings.Contains(page, "%!") || strings.Contains(page, "RedundaNet 2") {
		t.Fatalf("page: %d, leaks the observed address: %v, formatting error: %v", code, strings.Contains(page, "203.0.113.5"), strings.Contains(page, "%!"))
	}
	code, body := get("/status.json")
	var back Status
	if code != 200 || json.Unmarshal([]byte(body), &back) != nil || len(back.Devices) != 3 || strings.Contains(body, "203.0.113.5") || strings.Contains(body, "redundanet2") {
		t.Fatalf("status.json: %d %s", code, body[:120])
	}
	if code, _ := get("/nope"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}

	// Time passes without a channel: recent, then down, then dead.
	w.setLive(hub.Status{})
	w.advance(5 * time.Minute)
	if got := w.c.Status().Devices[0]; got.Device != falcon.String() || got.State != "recent" {
		t.Fatalf("after 5 min: %+v", got)
	}
	w.advance(6 * time.Minute)
	if got := deviceState(w.c.Status(), falcon); got != "down" {
		t.Fatalf("after 11 min: %s", got)
	}
	w.advance(73 * time.Hour)
	if got := deviceState(w.c.Status(), falcon); got != "dead" {
		t.Fatalf("after 73 h: %s", got)
	}
}

func deviceState(st Status, k identity.Key) string {
	for _, d := range st.Devices {
		if d.Device == k.String() {
			return d.State
		}
	}
	return "missing"
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "census.json")
	w := newWorld(t, path)
	w.c.Record(identity.Peer{Device: falcon, Member: alice}, "udp", wire.Census{Version: "3.0.0-alpha.1", Roles: []string{"storage"}})
	if err := w.c.Save(); err != nil {
		t.Fatal(err)
	}
	again := newWorld(t, path)
	again.advance(time.Minute)
	st := again.c.Status()
	f := st.Devices[0]
	if f.Device != falcon.String() || f.Version != "3.0.0-alpha.1" || f.LastSeen == nil || !f.LastSeen.Equal(w.now) || f.State != "recent" {
		t.Fatalf("loaded: %+v", f)
	}
	if err := again.c.Save(); err != nil {
		t.Fatal(err) // nothing dirty: a no-op
	}
}

func TestShareCensusMakesMemberUsage(t *testing.T) {
	w := newWorld(t, "")
	entry := func(si byte, n uint8, owner identity.Key, size int64) wire.ShareEntry {
		var e wire.ShareEntry
		e.StorageIndex[0], e.Number, e.Owner, e.Size = si, n, owner, size
		return e
	}
	// falcon reports in two chunks, then a delta that removes one share.
	w.c.RecordShares(identity.Peer{Device: falcon, Member: alice}, &wire.ShareCensus{Since: 0, Generation: 3, Added: []wire.ShareEntry{entry(1, 0, alice, 4096), entry(1, 1, alice, 4096)}, More: true})
	w.c.RecordShares(identity.Peer{Device: falcon, Member: alice}, &wire.ShareCensus{Since: 0, Generation: 3, Added: []wire.ShareEntry{entry(2, 0, bob, 8192)}})
	w.c.RecordShares(identity.Peer{Device: spare, Member: alice}, &wire.ShareCensus{Since: 0, Generation: 1, Added: []wire.ShareEntry{entry(2, 1, bob, 8192)}})
	st := w.c.Status()
	byName := map[string]Member{}
	for _, m := range st.Members {
		byName[m.Name] = m
	}
	if byName["alice"].StoredBytes != 8192 || byName["alice"].Objects != 1 || byName["bob"].StoredBytes != 16384 || byName["bob"].Objects != 1 {
		t.Fatalf("usage: %+v", st.Members)
	}
	w.c.RecordShares(identity.Peer{Device: falcon, Member: alice}, &wire.ShareCensus{Since: 3, Generation: 4, Removed: []wire.ShareKey{{StorageIndex: [16]byte{1}, Number: 1}}})
	st = w.c.Status()
	for _, m := range st.Members {
		if m.Name == "alice" && m.StoredBytes != 4096 {
			t.Fatalf("after removal: %+v", m)
		}
	}
	// A census from zero replaces the table: the node was rebuilt.
	w.c.RecordShares(identity.Peer{Device: falcon, Member: alice}, &wire.ShareCensus{Since: 0, Generation: 1, Added: []wire.ShareEntry{entry(9, 0, alice, 4096)}})
	st = w.c.Status()
	for _, m := range st.Members {
		if m.Name == "bob" && m.StoredBytes != 8192 {
			t.Fatalf("after a reset: %+v", m)
		}
	}
}

// The restore test streak travels in the heartbeat and the member row
// shows it beside the last verified restore.
func TestVerifyStreakOnTheMemberRow(t *testing.T) {
	w := newWorld(t, "")
	when := w.now.Add(-2 * time.Hour)
	w.c.Record(identity.Peer{Device: falcon, Member: alice}, "udp", wire.Census{Roles: []string{"client"},
		Backup: &wire.CensusBackup{LastBackup: when, LastVerify: when, VerifyOK: true, VerifyStreak: 3}})
	w.c.Record(identity.Peer{Device: spare, Member: alice}, "udp", wire.Census{Roles: []string{"client"},
		Backup: &wire.CensusBackup{LastVerify: when.Add(-time.Hour), VerifyOK: true, VerifyStreak: 1}})
	st := w.c.Status()
	for _, m := range st.Members {
		if m.Name == "alice" && (m.LastVerified != "2026-11-02" || m.VerifyStreak != 3) {
			t.Fatalf("alice: %+v", m)
		}
	}
	rec := httptest.NewRecorder()
	w.c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if page := rec.Body.String(); !strings.Contains(page, "2026-11-02, 3 weeks in a row") {
		t.Fatalf("page lacks the streak:\n%s", page)
	}
}

// The page is public: however many ask, the status is computed once in
// pageTTL, and what changed shows after it.
func TestTheListenerAnswersFromOneComputedStatus(t *testing.T) {
	w := newWorld(t, "")
	h := w.c.Handler()
	get := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	before := get("/status.json")
	if strings.Contains(before, `"version":"3.9.9"`) {
		t.Fatalf("the version is there before the census:\n%s", before)
	}
	w.c.Record(identity.Peer{Device: falcon, Member: alice}, "udp", wire.Census{Version: "3.9.9", Roles: []string{"storage"}, Manifest: 7})
	if got := get("/status.json"); got != before {
		t.Fatalf("a second request inside %s computed the status again", pageTTL)
	}
	if page := get("/"); strings.Contains(page, "3.9.9") {
		t.Fatalf("the page inside %s is not from the same status", pageTTL)
	}
	w.advance(pageTTL)
	if got := get("/status.json"); !strings.Contains(got, `"version":"3.9.9"`) {
		t.Fatalf("the status after %s is still the old one:\n%s", pageTTL, got)
	}
	if page := get("/"); !strings.Contains(page, "3.9.9") {
		t.Fatalf("the page after %s is still the old one", pageTTL)
	}
}
