package rebalance

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
	"github.com/adefilippo83/redundanet/agent/internal/cleanup"
	"github.com/adefilippo83/redundanet/agent/internal/collector"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The exit test of M5's last slice, in process: a backup under 2-of-4,
// the manifest moves to 2-of-3 with a deadline, the owner's device
// converts its snapshots, the hub's conversion bar reaches the end, a
// restore reads the converted snapshot, the next backup reuses the new
// objects, and the old ones are garbage for the cleanup.

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
	g    *grid
	self identity.Key
}

func (p *policy) Network() string { return "testnet" }
func (p *policy) MemberStatus(k identity.Key) (bool, bool) {
	mem := p.g.manifest().MemberByKey(k)
	return mem != nil && mem.Status == "active", false
}
func (p *policy) OwnerCap(identity.Key) int64 { return 1 << 40 }
func (p *policy) IsHub(identity.Key) bool     { return false }
func (p *policy) Device() identity.Key        { return p.self }
func (p *policy) Converting() (int, int, bool) {
	e, ok := p.g.manifest().Converting(p.g.clock())
	return e.K, e.N, ok
}

type grid struct {
	t       *testing.T
	ctx     context.Context
	log     *slog.Logger
	hub     *hub.Server
	hubKey  identity.Key
	col     *collector.Collector
	mu      sync.Mutex
	m       *manifest.Manifest
	allowed map[identity.Key]bool
	now     time.Time
	stores  []*store.Store
}

func (g *grid) manifest() *manifest.Manifest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.m
}

func (g *grid) setManifest(m *manifest.Manifest) {
	g.mu.Lock()
	g.m = m
	g.mu.Unlock()
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

func shareCensus(st *store.Store) func(since uint64) (*wire.ShareCensus, error) {
	return func(since uint64) (*wire.ShareCensus, error) {
		if since > 0 {
			if gen, err := st.Generation(); err != nil || gen == since {
				return nil, err
			}
		}
		d, err := st.Census(since)
		if err != nil {
			return nil, err
		}
		out := &wire.ShareCensus{Since: d.Since, Generation: d.Generation, Added: []wire.ShareEntry{}, Removed: []wire.ShareKey{}, Tombstones: []wire.TombstoneEntry{}}
		for _, e := range d.Added {
			out.Added = append(out.Added, wire.ShareEntry{StorageIndex: e.StorageIndex, Number: e.Number, Owner: e.Owner, Size: e.Size, K: e.K, N: e.N, DescriptorHash: e.DescriptorHash})
		}
		for _, k := range d.Removed {
			out.Removed = append(out.Removed, wire.ShareKey{StorageIndex: k.StorageIndex, Number: k.Number})
		}
		return out, nil
	}
}

func (g *grid) endpoint(m *member, onStream func(identity.Peer, string, *quic.Stream), st *store.Store) *transport.Endpoint {
	t := g.t
	t.Helper()
	g.permit(m.dev.ID())
	cfg := transport.Config{Identity: m.tls, Device: m.dev.ID(), Allow: g.allow, Hubs: []transport.HubAddr{{Key: g.hubKey, UDP: g.hub.UDPAddr().String()}},
		Listen: "127.0.0.1:0", LoopbackCandidates: true, Log: g.log}
	if onStream != nil {
		cfg.OnStream = onStream
		cfg.Census = func() wire.Census {
			return wire.Census{Roles: []string{identity.RoleStorage}, Storage: &wire.CensusStorage{FreeBytes: 100 << 30, TotalBytes: 200 << 30, State: "ok", Floor: 1}}
		}
		cfg.ShareCensus = shareCensus(st)
	}
	ep, err := transport.Start(g.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

// newGrid is a hub with a collector, two storage servers of two members,
// and the first member's laptop with a native engine, under 2-of-4.
func newGrid(t *testing.T) (*grid, *engine.Native, *member, *localdb.DB) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := &grid{t: t, ctx: ctx, allowed: map[identity.Key]bool{}, now: time.Now()}
	g.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := deviceOf(t, identity.Seed{100}, identity.RoleHub)
	g.permit(h.dev.ID())
	g.hubKey = h.dev.ID()
	alice := deviceOf(t, identity.Seed{1}, identity.RoleClient, identity.RoleStorage)
	bob := deviceOf(t, identity.Seed{2}, identity.RoleClient, identity.RoleStorage)
	g.m = &manifest.Manifest{Schema: 3, Revision: 1, Network: manifest.Network{Name: "testnet", Encoding: manifest.Encoding{K: 2, N: 4}, QuotaHeadroom: 2, RepairAfterHours: 72},
		Hubs:    []manifest.Hub{{Name: "hub", Key: h.dev.ID()}},
		Members: []manifest.Member{{Name: "alice", Key: alice.key, ContributionGB: 100, Status: "active"}, {Name: "bob", Key: bob.key, ContributionGB: 100, Status: "active"}},
		Storage: []manifest.StorageDevice{{Member: "alice", Device: alice.dev.ID(), Name: "a"}, {Member: "bob", Device: bob.dev.ID(), Name: "b"}}}
	col, err := collector.New(collector.Config{Device: h.dev.ID(), Manifest: g.manifest, Now: g.clock, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = col.Close() })
	hs, err := hub.Start(ctx, hub.Config{Identity: h.tls, Device: h.dev.ID(), Allow: g.allow, UDP: "127.0.0.1:0", OnCensus: col.Record, OnShares: col.RecordShares, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	col.SetLive(hs.Status)
	g.hub, g.col = hs, col
	for _, m := range []*member{alice, bob} {
		root := filepath.Join(t.TempDir(), "disk")
		_ = os.Mkdir(root, 0o755)
		st, err := store.Open(store.Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "storage-id"), Floor: 1, Now: g.clock, Log: g.log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		g.stores = append(g.stores, st)
		srv := shareproto.NewServer(st, &policy{g: g, self: m.dev.ID()}, g.log)
		g.endpoint(m, srv.Handle, st)
	}
	laptop := deviceOf(t, alice.seed, identity.RoleClient)
	ep := g.endpoint(laptop, nil, nil)
	for _, s := range g.m.Storage {
		ep.Connect(s.Device)
	}
	db, err := localdb.Open(t.TempDir(), g.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e, err := engine.New(engine.Config{Network: "testnet", Member: alice.key, Secret: alice.seed.Secret(), Device: laptop.dev, Cert: laptop.cert,
		Manifest: g.manifest, Client: shareproto.NewClient(ep.OpenStream), DB: db, SpoolDir: t.TempDir(), Timeout: 5 * time.Second, Now: g.clock, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	return g, e, laptop, db
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConversionOnTheGrid(t *testing.T) {
	restoreTiming := transport.SetCensusTiming(500*time.Millisecond, 200*time.Millisecond)
	defer restoreTiming()
	g, e, laptop, db := newGrid(t)
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Minute)
	defer cancel()
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	files := map[string][]byte{"a.bin": bytes.Repeat([]byte("a"), 5000), "sub/b.bin": bytes.Repeat([]byte("b"), 70000), "sub/c.txt": []byte("tiny")}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func() *backup.Result {
		t.Helper()
		res, err := backup.Run(ctx, backup.Config{Engine: e, DB: db, Member: laptop.key, Device: laptop.dev.ID(), Set: backup.Set{Name: "home", Folders: []string{src}}, Now: g.clock, Log: g.log})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	run()
	// A second, identical snapshot: it shares every object with the first,
	// so the conversion must touch each once.
	g.mu.Lock()
	g.now = g.now.Add(time.Hour)
	g.mu.Unlock()
	run()
	conv := func() *collector.Conversion {
		_, grid, err := g.col.Objects()
		if err != nil || grid == nil {
			return nil
		}
		return grid.Conversion
	}
	// Nothing to do without a transition.
	cfg := Config{Grid: e, DB: db, Member: laptop.key, Device: laptop.dev.ID(), Manifest: g.manifest, SpoolDir: t.TempDir(), Now: g.clock, Log: g.log}
	st, err := Run(ctx, cfg)
	if err != nil || !st.Nothing {
		t.Fatalf("without a transition: %+v %v", st, err)
	}
	// The manifest moves to 2-of-3 with a deadline; the hub sees every
	// object under the old encoding.
	m2 := *g.manifest()
	m2.Network.Encoding = manifest.Encoding{K: 2, N: 3}
	m2.Network.EncodingTransition = &manifest.Transition{From: manifest.Encoding{K: 2, N: 4}, To: manifest.Encoding{K: 2, N: 3}, Deadline: g.clock().Add(30 * 24 * time.Hour).UTC().Format("2006-01-02")}
	g.setManifest(&m2)
	// The table holds seven objects once both servers have reported:
	// a.bin, sub/b.bin, two directories, the top, and the two indexes.
	eventually(t, 30*time.Second, "the hub to see the old encoding", func() bool {
		c := conv()
		return c != nil && c.Objects == 7 && c.Converted == 0
	})
	before := conv()
	// The owner converts: every object the snapshot references, the
	// directories and the index.
	st, err = Run(ctx, cfg)
	if err != nil || st.Nothing || st.Done != 2 || st.Snapshots != 2 || st.Objects == 0 || st.From != "2-of-4" || st.To != "2-of-3" {
		t.Fatalf("conversion: %+v %v", st, err)
	}
	// a.bin, sub/b.bin, the sub and folder directories, the top directory:
	// five objects in the walk, each once although two snapshots share
	// them; the index is published beside them.
	if st.Objects != 5 {
		t.Fatalf("%d objects re-encoded, want 5", st.Objects)
	}
	// The hub's bar reaches the end: as many converted as the old count,
	// and the old ones still there until the cleanup.
	// Seven new objects: the five of the walk and an index published after
	// each snapshot, so progress is durable. The old seven stay until the
	// cleanup.
	deadline := time.Now().Add(30 * time.Second)
	for {
		c := conv()
		if c != nil && c.Converted == 7 && c.Objects == before.Objects+7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the conversion bar did not reach the end: before %+v, now %+v", before, c)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A restore reads the converted snapshot.
	l, err := restore.List(ctx, e, laptop.key)
	if err != nil || len(l.Devices) != 1 || l.Devices[0].Index == nil || len(l.Devices[0].Index.Snapshots) != 2 {
		t.Fatalf("listing after conversion: %+v %v", l, err)
	}
	snap := l.Devices[0].Index.Snapshots[1]
	if d, err := e.Describe(ctx, mustCap(t, snap.Top)); err != nil || d.N != 3 {
		t.Fatalf("the top directory after conversion: %+v %v", d, err)
	}
	target := t.TempDir()
	rep, err := restore.Restore(ctx, e, &snap, nil, target, restore.Options{})
	if err != nil || rep.Files != 3 {
		t.Fatalf("restore after conversion: %+v %v", rep, err)
	}
	restored := map[string][]byte{}
	_ = filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			restored[filepath.Base(p)] = b
		}
		return nil
	})
	for name, b := range files {
		if got, ok := restored[filepath.Base(name)]; !ok || !bytes.Equal(got, b) {
			t.Fatalf("restored %s: present %v, same %v", name, ok, bytes.Equal(got, b))
		}
	}
	// Running again converts nothing: the scan rows carry the new
	// capabilities, so the next backup reuses them and references nothing old.
	if st, err := Run(ctx, cfg); err != nil || st.Objects != 0 {
		t.Fatalf("second run: %+v %v", st, err)
	}
	res := run()
	if res.Snapshot.Uploaded != 0 {
		t.Fatalf("the backup after conversion uploaded %d bytes", res.Snapshot.Uploaded)
	}
	if st, err := Run(ctx, cfg); err != nil || st.Objects != 0 {
		t.Fatalf("after the next backup: %+v %v", st, err)
	}
	// The old objects are garbage for the cleanup once past the immunity,
	// and so are the two indexes the conversion published, replaced by the
	// next backup's.
	g.mu.Lock()
	g.now = g.now.Add(8 * 24 * time.Hour)
	g.mu.Unlock()
	crep, err := cleanup.Run(ctx, cleanup.Config{Grid: e, Member: laptop.key, Now: g.clock, Log: g.log, Force: true})
	if err != nil || crep.Tombstoned != before.Objects+2 || crep.Reachable != 6 {
		t.Fatalf("cleanup after conversion: %+v %v (old objects %d)", crep, err, before.Objects)
	}
}

func mustCap(t *testing.T, s string) object.ReadCap {
	t.Helper()
	rc, err := object.ParseReadCap(s)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}
