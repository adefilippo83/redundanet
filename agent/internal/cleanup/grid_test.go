package cleanup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

// The exit test of M5's third slice, in process: a real grid of two
// storage servers, a backup from the owner's laptop, an object nothing
// references, the cleanup that tombstones it only past the immunity, a
// pruned snapshot whose objects become garbage, and the servers' purge
// after the window, all on a clock the test moves.

type member struct {
	seed identity.Seed
	key  identity.Key
	dev  *identity.DeviceKey
	cert *identity.SignedCertificate
	tls  *identity.TLSIdentity
}

func deviceOf(t *testing.T, s identity.Seed, roles ...string) *member {
	t.Helper()
	dev, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := identity.Issue(s, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: s.MemberKey(), Roles: roles, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	tlsID, err := identity.NewTLSIdentity(dev, cert)
	if err != nil {
		t.Fatal(err)
	}
	return &member{seed: s, key: s.MemberKey(), dev: dev, cert: cert, tls: tlsID}
}

type policy struct {
	m    *manifest.Manifest
	self identity.Key
}

func (p *policy) Network() string { return "testnet" }
func (p *policy) MemberStatus(k identity.Key) (bool, bool) {
	mem := p.m.MemberByKey(k)
	return mem != nil && mem.Status == "active", false
}
func (p *policy) OwnerCap(identity.Key) int64 { return 1 << 40 }
func (p *policy) IsHub(identity.Key) bool     { return false }
func (p *policy) Device() identity.Key        { return p.self }

type grid struct {
	t       *testing.T
	ctx     context.Context
	log     *slog.Logger
	m       *manifest.Manifest
	hub     *hub.Server
	hubKey  identity.Key
	mu      sync.Mutex
	allowed map[identity.Key]bool
	now     time.Time
	stores  []*store.Store
}

func (g *grid) allow(p identity.Peer) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.allowed[p.Device] {
		return errors.New("not admitted")
	}
	return nil
}

func (g *grid) permit(k identity.Key) {
	g.mu.Lock()
	g.allowed[k] = true
	g.mu.Unlock()
}

func (g *grid) clock() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now
}

func (g *grid) advance(d time.Duration) {
	g.mu.Lock()
	g.now = g.now.Add(d)
	g.mu.Unlock()
}

func (g *grid) endpoint(m *member, onStream func(identity.Peer, string, *quic.Stream)) *transport.Endpoint {
	t := g.t
	t.Helper()
	g.permit(m.dev.ID())
	cfg := transport.Config{Identity: m.tls, Device: m.dev.ID(), Allow: g.allow, Hubs: []transport.HubAddr{{Key: g.hubKey, UDP: g.hub.UDPAddr().String()}},
		Listen: "127.0.0.1:0", LoopbackCandidates: true, Log: g.log}
	if onStream != nil {
		cfg.OnStream = onStream
	}
	ep, err := transport.Start(g.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

// newGrid is a hub, two storage servers of two members, and the first
// member's laptop with a native engine, under a 2-of-4 manifest.
func newGrid(t *testing.T) (*grid, *engine.Native, *member, *localdb.DB) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := &grid{t: t, ctx: ctx, allowed: map[identity.Key]bool{}, now: time.Now()}
	g.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := deviceOf(t, identity.Seed{100}, identity.RoleHub)
	g.permit(h.dev.ID())
	hs, err := hub.Start(ctx, hub.Config{Identity: h.tls, Device: h.dev.ID(), Allow: g.allow, UDP: "127.0.0.1:0", Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	g.hub, g.hubKey = hs, h.dev.ID()
	alice := deviceOf(t, identity.Seed{1}, identity.RoleClient, identity.RoleStorage)
	bob := deviceOf(t, identity.Seed{2}, identity.RoleClient, identity.RoleStorage)
	g.m = &manifest.Manifest{Schema: 3, Revision: 1, Network: manifest.Network{Name: "testnet", Encoding: manifest.Encoding{K: 2, N: 4}, QuotaHeadroom: 2},
		Members: []manifest.Member{{Name: "alice", Key: alice.key, ContributionGB: 100, Status: "active"}, {Name: "bob", Key: bob.key, ContributionGB: 100, Status: "active"}},
		Storage: []manifest.StorageDevice{{Member: "alice", Device: alice.dev.ID(), Name: "a"}, {Member: "bob", Device: bob.dev.ID(), Name: "b"}}}
	for _, m := range []*member{alice, bob} {
		root := filepath.Join(t.TempDir(), "disk")
		_ = os.Mkdir(root, 0o755)
		st, err := store.Open(store.Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "storage-id"), Floor: 1, Now: g.clock, Log: g.log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		g.stores = append(g.stores, st)
		srv := shareproto.NewServer(st, &policy{m: g.m, self: m.dev.ID()}, g.log)
		g.endpoint(m, srv.Handle)
	}
	laptop := deviceOf(t, alice.seed, identity.RoleClient)
	ep := g.endpoint(laptop, nil)
	for _, s := range g.m.Storage {
		ep.Connect(s.Device)
	}
	db, err := localdb.Open(t.TempDir(), g.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e, err := engine.New(engine.Config{Network: "testnet", Member: alice.key, Secret: alice.seed.Secret(), Device: laptop.dev, Cert: laptop.cert,
		Manifest: func() *manifest.Manifest { return g.m }, Client: shareproto.NewClient(ep.OpenStream), DB: db, SpoolDir: t.TempDir(), Timeout: 5 * time.Second, Now: g.clock, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	return g, e, laptop, db
}

func TestCleanupOnTheGrid(t *testing.T) {
	g, e, laptop, db := newGrid(t)
	ctx, cancel := context.WithTimeout(g.ctx, 2*time.Minute)
	defer cancel()
	src := t.TempDir()
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.bin", bytes.Repeat([]byte("k"), 5000))
	write("gone.bin", bytes.Repeat([]byte("g"), 6000))
	retention := snapshot.Retention{Days: 1, Daily: 1, Weekly: 1, Monthly: 1}
	run := func() *backup.Result {
		t.Helper()
		res, err := backup.Run(ctx, backup.Config{Engine: e, DB: db, Member: laptop.key, Device: laptop.dev.ID(), Set: backup.Set{Name: "home", Folders: []string{src}}, Retention: retention, Now: g.clock, Log: g.log})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	run()
	// An object nothing references.
	orphan, err := e.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("o"), 7000)), 7000)
	if err != nil {
		t.Fatal(err)
	}
	gone, ok, _ := e.Has(ctx, object.Sum(bytes.Repeat([]byte("g"), 6000)), 6000)
	if !ok {
		t.Fatal("gone.bin's object is not stored")
	}
	cfg := Config{Grid: e, Member: laptop.key, Now: g.clock, Log: g.log}

	// Within the immunity the orphan is garbage but every server refuses
	// the token; nothing is tombstoned.
	rep, err := Run(ctx, cfg)
	if err != nil || rep.Garbage != 1 || rep.Recent != 1 || rep.Tombstoned != 0 || rep.RootsAnswered != 2 || rep.Roots != 1 {
		t.Fatalf("within the immunity: %+v %v", rep, err)
	}
	// Past it, the orphan goes; the backup's objects stay.
	g.advance(8 * 24 * time.Hour)
	rep, err = Run(ctx, cfg)
	if err != nil || rep.Garbage != 1 || rep.Tombstoned != 1 {
		t.Fatalf("past the immunity: %+v %v", rep, err)
	}
	if h, _ := e.Check(ctx, orphan); !h.Tombstoned || h.Readable {
		t.Fatalf("the orphan after cleanup: %+v", h)
	}
	if h, _ := e.Check(ctx, gone); !h.Placed {
		t.Fatalf("a referenced object after cleanup: %+v", h)
	}
	// The owner still reads it within the window.
	var out bytes.Buffer
	if err := e.Get(ctx, orphan, &out, nil); err != nil || out.Len() != 7000 {
		t.Fatalf("the owner's read of a tombstoned object: %v", err)
	}

	// gone.bin is deleted and, days later, its snapshot is pruned: its
	// object, the old directory and the old index become garbage.
	if err := os.Remove(filepath.Join(src, "gone.bin")); err != nil {
		t.Fatal(err)
	}
	run()
	g.advance(8 * 24 * time.Hour)
	run()
	rep, err = Run(ctx, cfg)
	if err != nil {
		t.Fatalf("after the prune: %+v %v", rep, err)
	}
	if h, _ := e.Check(ctx, gone); !h.Tombstoned {
		t.Fatalf("gone.bin's object after the prune and cleanup: %+v (report %+v)", h, rep)
	}
	if rep.Reachable < 3 || rep.Tombstoned < 1 {
		t.Fatalf("report after the prune: %+v", rep)
	}

	// The servers purge after the window: the shares go, the record
	// stays, the owner's read fails, and the census says purged.
	g.advance(31 * 24 * time.Hour)
	for _, st := range g.stores {
		if n, err := st.PurgeTombstones(); err != nil || n < 2 {
			t.Fatalf("purge: %d %v", n, err)
		}
	}
	if err := e.Get(ctx, orphan, &out, nil); !errors.Is(err, engine.ErrNotReadable) {
		t.Fatalf("read after the purge: %v", err)
	}
	d, _ := g.stores[0].Census(0)
	purged := 0
	for _, tb := range d.Tombstones {
		if tb.Purged {
			purged++
		}
	}
	if purged < 2 {
		t.Fatalf("census after the purge: %+v", d.Tombstones)
	}
	// What the roots reference still reads.
	if h, _ := e.Check(ctx, orphanOrKeep(t, e, ctx)); !h.Placed {
		t.Fatalf("keep.bin after everything: %+v", h)
	}
}

func orphanOrKeep(t *testing.T, e *engine.Native, ctx context.Context) object.ReadCap {
	t.Helper()
	rc, ok, err := e.Has(ctx, object.Sum(bytes.Repeat([]byte("k"), 5000)), 5000)
	if err != nil || !ok {
		t.Fatalf("keep.bin's object: %v %v", ok, err)
	}
	return rc
}
