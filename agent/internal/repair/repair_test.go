package repair

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

	"github.com/adefilippo83/redundanet/agent/internal/collector"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The exit test of M5's second slice, in process: a hub with the
// scheduler, three storage nodes with workers, an owner's laptop that
// stores an object and then does nothing. One node dies; the hub issues a
// task; a surviving node regenerates the lost share and places it; the
// object is fully placed again with the owner idle. The dead node returns;
// the hub hints at the surplus; the holder confirms with the others and
// gives its copy up.

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

func newMember(t *testing.T, b byte, roles ...string) *member {
	t.Helper()
	var s identity.Seed
	s[0] = b
	return deviceOf(t, s, roles...)
}

// policy is one server's view of the manifest.
type policy struct {
	convK, convN int // the encoding accounted at zero, when set
	m            *manifest.Manifest
	self         identity.Key
	hubs         map[identity.Key]bool
}

func (p *policy) Network() string { return "testnet" }
func (p *policy) MemberStatus(k identity.Key) (bool, bool) {
	mem := p.m.MemberByKey(k)
	return mem != nil && mem.Status == "active", false
}
func (p *policy) OwnerCap(identity.Key) int64  { return 1 << 40 }
func (p *policy) IsHub(k identity.Key) bool    { return p.hubs[k] }
func (p *policy) Device() identity.Key         { return p.self }
func (p *policy) Converting() (int, int, bool) { return p.convK, p.convN, p.convK > 0 }

type node struct {
	member *member
	store  *store.Store
	worker *Worker
	ep     *transport.Endpoint
	cancel context.CancelFunc
}

type grid struct {
	t       *testing.T
	ctx     context.Context
	log     *slog.Logger
	m       *manifest.Manifest
	hub     *hub.Server
	hubKey  identity.Key
	signer  *identity.DeviceKey
	col     *collector.Collector
	mu      sync.Mutex
	allowed map[identity.Key]bool
	now     time.Time
	nodes   map[identity.Key]*node
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

// shareCensus adapts a store's census to the wire, as the agent does.
func shareCensus(st *store.Store) func(since uint64) (*wire.ShareCensus, error) {
	return func(since uint64) (*wire.ShareCensus, error) {
		if since > 0 {
			if g, err := st.Generation(); err != nil || g == since {
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
		for _, tb := range d.Tombstones {
			out.Tombstones = append(out.Tombstones, wire.TombstoneEntry{StorageIndex: tb.StorageIndex, Owner: tb.Owner, DescriptorHash: tb.DescriptorHash, Purged: tb.Purged})
		}
		return out, nil
	}
}

// startNode brings a storage node up: store, server, worker, endpoint.
func (g *grid) startNode(m *member, st *store.Store) *node {
	t := g.t
	t.Helper()
	g.permit(m.dev.ID())
	ctx, cancel := context.WithCancel(g.ctx)
	n := &node{member: m, store: st, cancel: cancel}
	pol := &policy{m: g.m, self: m.dev.ID(), hubs: map[identity.Key]bool{g.hubKey: true}}
	srv := shareproto.NewServer(st, pol, g.log)
	var epMu sync.Mutex
	var ep *transport.Endpoint
	client := shareproto.NewClient(func(ctx context.Context, peer identity.Key) (wire.Stream, string, error) {
		epMu.Lock()
		e := ep
		epMu.Unlock()
		return e.OpenStream(ctx, peer)
	})
	w, err := New(Config{Device: m.dev.ID(), Manifest: func() *manifest.Manifest { return g.m }, Client: client, Store: st, Timeout: 5 * time.Second, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	n.worker = w
	go w.Run(ctx)
	census := func() wire.Census {
		return wire.Census{Version: "test", Roles: []string{identity.RoleStorage}, Worker: true,
			Storage: &wire.CensusStorage{FreeBytes: 100 << 30, TotalBytes: 200 << 30, State: "ok", Floor: 1}}
	}
	e, err := transport.Start(ctx, transport.Config{Identity: m.tls, Device: m.dev.ID(), Allow: g.allow, Hubs: []transport.HubAddr{{Key: g.hubKey, UDP: g.hub.UDPAddr().String()}},
		Listen: "127.0.0.1:0", LoopbackCandidates: true, OnStream: srv.Handle, Census: census, ShareCensus: shareCensus(st),
		OnTask: w.Handle, OnRelease: w.Release, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	epMu.Lock()
	ep = e
	epMu.Unlock()
	n.ep = e
	t.Cleanup(func() { cancel(); _ = e.Close() })
	g.mu.Lock()
	g.nodes[m.dev.ID()] = n
	g.mu.Unlock()
	return n
}

func (g *grid) stopNode(n *node) {
	n.cancel()
	_ = n.ep.Close()
}

func newStore(t *testing.T, lg *slog.Logger) *store.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "disk")
	_ = os.Mkdir(root, 0o755)
	st, err := store.Open(store.Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "storage-id"), Floor: 1, Log: lg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newGrid is a hub with the scheduler and three storage nodes, one per
// member, under a 2-of-3 manifest.
func newGrid(t *testing.T) (*grid, []*member) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := &grid{t: t, ctx: ctx, allowed: map[identity.Key]bool{}, now: time.Now(), nodes: map[identity.Key]*node{}}
	g.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if os.Getenv("REPAIR_TEST_LOG") != "" {
		g.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	h := newMember(t, 100, identity.RoleHub)
	g.permit(h.dev.ID())
	g.hubKey, g.signer = h.dev.ID(), h.dev
	members := []*member{newMember(t, 1, identity.RoleClient, identity.RoleStorage), newMember(t, 2, identity.RoleClient, identity.RoleStorage), newMember(t, 3, identity.RoleClient, identity.RoleStorage)}
	g.m = &manifest.Manifest{Schema: 3, Revision: 1, Network: manifest.Network{Name: "testnet", Encoding: manifest.Encoding{K: 2, N: 3}, QuotaHeadroom: 2, RepairAfterHours: 72},
		Hubs: []manifest.Hub{{Name: "hub", Key: h.dev.ID()}}}
	for i, m := range members {
		name := string(rune('a' + i))
		g.m.Members = append(g.m.Members, manifest.Member{Name: name, Key: m.key, ContributionGB: 100, Status: "active"})
		g.m.Storage = append(g.m.Storage, manifest.StorageDevice{Member: name, Device: m.dev.ID(), Name: name + "-node"})
	}
	col, err := collector.New(collector.Config{Device: h.dev.ID(), Signer: h.dev, Manifest: func() *manifest.Manifest { return g.m }, Now: g.clock, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = col.Close() })
	hs, err := hub.Start(ctx, hub.Config{Identity: h.tls, Device: h.dev.ID(), Allow: g.allow, UDP: "127.0.0.1:0",
		OnCensus: col.Record, OnShares: col.RecordShares, OnTaskReport: col.RecordTaskReport, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	col.SetLive(hs.Status)
	col.SetSend(hs.Send)
	g.hub, g.col = hs, col
	for _, m := range members {
		g.startNode(m, newStore(t, g.log))
	}
	return g, members
}

// laptop is an owner's client device with a native engine.
func (g *grid) laptop(m *member) *engine.Native {
	t := g.t
	t.Helper()
	d := deviceOf(t, m.seed, identity.RoleClient)
	g.permit(d.dev.ID())
	ep, err := transport.Start(g.ctx, transport.Config{Identity: d.tls, Device: d.dev.ID(), Allow: g.allow, Hubs: []transport.HubAddr{{Key: g.hubKey, UDP: g.hub.UDPAddr().String()}},
		Listen: "127.0.0.1:0", LoopbackCandidates: true, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	for _, s := range g.m.Storage {
		ep.Connect(s.Device)
	}
	db, err := localdb.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e, err := engine.New(engine.Config{Network: "testnet", Member: m.key, Secret: m.seed.Secret(), Device: d.dev, Cert: d.cert,
		Manifest: func() *manifest.Manifest { return g.m }, Client: shareproto.NewClient(ep.OpenStream), DB: db, SpoolDir: t.TempDir(), Timeout: 5 * time.Second, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (g *grid) object(si object.StorageIndex) (collector.Object, bool) {
	objs, _, err := g.col.Objects()
	if err != nil {
		g.t.Fatal(err)
	}
	for _, o := range objs {
		if o.StorageIndex == si {
			return o, true
		}
	}
	return collector.Object{}, false
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

func pattern(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+3) ^ salt
	}
	return b
}

func TestRepairWithoutTheOwner(t *testing.T) {
	restore := transport.SetCensusTiming(500*time.Millisecond, 200*time.Millisecond)
	defer restore()
	g, members := newGrid(t)
	ctx, cancel := context.WithTimeout(g.ctx, 2*time.Minute)
	defer cancel()

	// The owner stores an object: one share per node.
	e := g.laptop(members[0])
	data := pattern(2*object.DefaultSegmentSize+777, 1)
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	si := rc.Key.StorageIndex()
	eventually(t, 30*time.Second, "the placement table to show the object placed", func() bool {
		o, ok := g.object(si)
		return ok && o.Placed && o.Spread == 3
	})

	// One node dies; its share is gone from the table after the dead
	// window, and the object is due for repair at once.
	var victim *node
	var lost uint8
	o, _ := g.object(si)
	for dev, shares := range o.Holders {
		if dev == members[2].dev.ID() {
			victim, lost = g.nodes[dev], shares[0]
		}
	}
	g.stopNode(victim)
	g.advance(73 * time.Hour)
	eventually(t, 20*time.Second, "the dead node's share to stop counting", func() bool {
		o, ok := g.object(si)
		return ok && o.Spread == 2 && o.Need == "repair" && len(o.Missing) == 1 && o.Missing[0] == lost
	})

	// The hub issues a task; a surviving node regenerates the share from
	// its own share and the other node's, and places it. The owner's
	// laptop does nothing.
	g.col.Schedule(ctx)
	if st := g.col.Status(); st.Grid.RepairRunning != 1 {
		t.Fatalf("no task issued: %+v", st.Grid)
	}
	eventually(t, 30*time.Second, "the object to be fully placed again", func() bool {
		o, ok := g.object(si)
		return ok && o.Placed && len(o.Shares) == 3
	})
	g.col.Schedule(ctx)
	st := g.col.Status()
	if st.Grid.RepairRunning != 0 || st.Grid.RepairDone != 1 || st.Grid.RepairFailed != 0 {
		t.Fatalf("after repair: %+v", st.Grid)
	}
	var worked *node
	for _, n := range g.nodes {
		if n != victim && n.worker.Status().Done == 1 {
			worked = n
		}
	}
	if worked == nil {
		t.Fatal("no worker reports a task done")
	}
	// The regenerated share reads back: the owner's data is intact
	// through the repaired share.
	var out bytes.Buffer
	if err := e.Get(ctx, rc, &out, nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("get after repair: %v", err)
	}
	h, err := e.Check(ctx, rc)
	if err != nil || !h.Placed || len(h.Shares) != 3 {
		t.Fatalf("check after repair: %+v %v", h, err)
	}

	// The dead node returns with its share: a surplus. The hub hints at the
	// holder with the most shares, which confirms with the others and
	// gives its copy up; the object is back to one share per node.
	g.startNode(members[2], victim.store)
	eventually(t, 30*time.Second, "the returned node's share to count again", func() bool {
		o, ok := g.object(si)
		return ok && o.Surplus == 1 && o.Spread == 3
	})
	g.col.Schedule(ctx)
	eventually(t, 30*time.Second, "the surplus to be released", func() bool {
		o, ok := g.object(si)
		return ok && o.Surplus == 0 && o.Placed && o.Spread == 3
	})
	released := 0
	for _, n := range g.nodes {
		released += n.worker.Status().Released
	}
	if released != 1 {
		t.Fatalf("%d shares released", released)
	}
	if err := e.Get(ctx, rc, &out, nil); err != nil {
		t.Fatalf("get after the release: %v", err)
	}
}

// A task that names this node as a target and a holder uses the local
// store for both, since a node cannot open a stream to itself.
func TestWorkerStoresOnItself(t *testing.T) {
	restore := transport.SetCensusTiming(500*time.Millisecond, 200*time.Millisecond)
	defer restore()
	g, members := newGrid(t)
	ctx, cancel := context.WithTimeout(g.ctx, time.Minute)
	defer cancel()
	e := g.laptop(members[0])
	data := pattern(object.DefaultSegmentSize+5, 2)
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	si := rc.Key.StorageIndex()
	eventually(t, 30*time.Second, "the object placed", func() bool { o, ok := g.object(si); return ok && o.Placed })
	o, _ := g.object(si)
	self := g.nodes[members[1].dev.ID()]
	var mine uint8
	for _, n := range o.Holders[self.member.dev.ID()] {
		mine = n
	}
	// A task for the number the third node holds, to be stored on this
	// node too, handed to this node directly.
	var want uint8
	for _, n := range o.Holders[members[2].dev.ID()] {
		want = n
	}
	task := &wire.RepairTask{Network: "testnet", ID: 42, StorageIndex: si, Owner: members[0].key, DescriptorHash: rc.DescriptorHash, Worker: self.member.dev.ID(),
		Holders: []identity.Key{self.member.dev.ID(), members[0].dev.ID()}, Shares: []wire.RepairShare{{Number: want, Server: self.member.dev.ID()}},
		Issued: time.Now(), Expires: time.Now().Add(time.Hour)}
	signer := g.signer
	if err := task.Sign(signer); err != nil {
		t.Fatal(err)
	}
	reports := make(chan *wire.TaskReport, 1)
	self.worker.Handle(task, func(r *wire.TaskReport) error { reports <- r; return nil })
	select {
	case r := <-reports:
		if r.Failed != "" || len(r.Done) != 1 || r.Done[0] != want {
			t.Fatalf("report: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no report")
	}
	pres, err := self.store.Has(members[0].key, si)
	if err != nil || pres == nil || len(pres.Complete) != 2 {
		t.Fatalf("the node's store after the task: %+v %v (own share %d)", pres, err, mine)
	}
	sh, err := self.store.OpenShare(members[0].key, si, want)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	if err := sh.Verify(); err != nil {
		t.Fatalf("the regenerated share does not verify: %v", err)
	}
	// A task for an object nobody holds fails with a report, not a hang.
	var none object.StorageIndex
	none[0] = 0xff
	bad := &wire.RepairTask{Network: "testnet", ID: 43, StorageIndex: none, Owner: members[0].key, DescriptorHash: rc.DescriptorHash, Worker: self.member.dev.ID(),
		Shares: []wire.RepairShare{{Number: 0, Server: self.member.dev.ID()}}, Issued: time.Now(), Expires: time.Now().Add(time.Hour)}
	_ = bad.Sign(signer)
	self.worker.Handle(bad, func(r *wire.TaskReport) error { reports <- r; return nil })
	select {
	case r := <-reports:
		if r.Failed == "" || len(r.Done) != 0 {
			t.Fatalf("report for a missing object: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no report for a missing object")
	}
	if st := self.worker.Status(); st.Done != 1 || st.Failed != 1 || st.Last == "" {
		t.Fatalf("worker status: %+v", st)
	}
}
