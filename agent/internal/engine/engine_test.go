package engine

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

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

type member struct {
	seed identity.Seed
	key  identity.Key
	dev  *identity.DeviceKey
	cert *identity.SignedCertificate
	tls  *identity.TLSIdentity
}

func newMember(t *testing.T, b byte, roles ...string) *member {
	t.Helper()
	var s identity.Seed
	s[0] = b
	return deviceOf(t, s, roles...)
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

// policy is one server's view of the manifest, with a cap per owner.
type policy struct {
	m    *manifest.Manifest
	mu   sync.Mutex
	caps map[identity.Key]int64
}

func (p *policy) Network() string { return "testnet" }
func (p *policy) MemberStatus(k identity.Key) (bool, bool) {
	mem := p.m.MemberByKey(k)
	return mem != nil && mem.Status == "active", false
}
func (p *policy) OwnerCap(o identity.Key) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.caps[o]; ok {
		return c
	}
	return 1 << 40
}

type grid struct {
	ctx     context.Context
	log     *slog.Logger
	m       *manifest.Manifest
	hub     *hub.Server
	hubKey  identity.Key
	allowed map[identity.Key]bool
	mu      sync.Mutex
	servers map[identity.Key]*serverNode
}

type serverNode struct {
	member *member
	store  *store.Store
	pol    *policy
	ep     *transport.Endpoint
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

func (g *grid) endpoint(t *testing.T, m *member, onStream func(identity.Peer, string, *quic.Stream)) *transport.Endpoint {
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

// newGrid is a hub and one storage server for each of alice and bob, with
// a 2-of-4 manifest listing them.
func newGrid(t *testing.T) (*grid, *member, *member) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := &grid{ctx: ctx, allowed: map[identity.Key]bool{}, servers: map[identity.Key]*serverNode{}}
	g.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := newMember(t, 100, identity.RoleHub)
	g.permit(h.dev.ID())
	hs, err := hub.Start(ctx, hub.Config{Identity: h.tls, Device: h.dev.ID(), Allow: g.allow, UDP: "127.0.0.1:0", Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	g.hub, g.hubKey = hs, h.dev.ID()
	alice := newMember(t, 1, identity.RoleClient, identity.RoleStorage)
	bob := newMember(t, 2, identity.RoleClient, identity.RoleStorage)
	g.m = &manifest.Manifest{Schema: 3, Revision: 1, Network: manifest.Network{Name: "testnet", Encoding: manifest.Encoding{K: 2, N: 4}, QuotaHeadroom: 2},
		Members: []manifest.Member{{Name: "alice", Key: alice.key, ContributionGB: 100, Status: "active"}, {Name: "bob", Key: bob.key, ContributionGB: 100, Status: "active"}},
		Storage: []manifest.StorageDevice{{Member: "alice", Device: alice.dev.ID(), Name: "a"}, {Member: "bob", Device: bob.dev.ID(), Name: "b"}}}
	for _, m := range []*member{alice, bob} {
		root := filepath.Join(t.TempDir(), "disk")
		_ = os.Mkdir(root, 0o755)
		st, err := store.Open(store.Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "storage-id"), Floor: 1, Log: g.log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		pol := &policy{m: g.m, caps: map[identity.Key]int64{}}
		srv := shareproto.NewServer(st, pol, g.log)
		node := &serverNode{member: m, store: st, pol: pol}
		node.ep = g.endpoint(t, m, srv.Handle)
		g.servers[m.dev.ID()] = node
	}
	return g, alice, bob
}

// engineFor is a native engine on a fresh device of a member.
func (g *grid) engineFor(t *testing.T, m *member) (*Native, *member) {
	t.Helper()
	laptop := deviceOf(t, m.seed, identity.RoleClient)
	ep := g.endpoint(t, laptop, nil)
	for dev := range g.servers {
		ep.Connect(dev)
	}
	db, err := localdb.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e, err := New(Config{Network: "testnet", Member: m.key, Secret: m.seed.Secret(), Device: laptop.dev, Cert: laptop.cert,
		Manifest: func() *manifest.Manifest { return g.m }, Client: shareproto.NewClient(ep.OpenStream), DB: db, SpoolDir: t.TempDir(), Timeout: 5 * time.Second, Log: g.log})
	if err != nil {
		t.Fatal(err)
	}
	return e, laptop
}

func pattern(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+3) ^ salt
	}
	return b
}

// contract is what every engine must do.
func contract(t *testing.T, e Engine, member identity.Key, other identity.Key) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	data := pattern(2*object.DefaultSegmentSize+777, 1)
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Size != uint64(len(data)) {
		t.Fatalf("cap size %d", rc.Size)
	}
	var out bytes.Buffer
	if err := e.Get(ctx, rc, &out, nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("get: %v (%d bytes)", err, out.Len())
	}
	out.Reset()
	rng := &Range{Offset: object.DefaultSegmentSize + object.DefaultSegmentSize/2, Length: 4096}
	if err := e.Get(ctx, rc, &out, rng); err != nil || !bytes.Equal(out.Bytes(), data[rng.Offset:rng.Offset+rng.Length]) {
		t.Fatalf("get range: %v (%d bytes)", err, out.Len())
	}
	if got, ok, err := e.Has(ctx, object.Sum(data), int64(len(data))); err != nil || !ok || got != rc {
		t.Fatalf("has: %v %v", ok, err)
	}
	if _, ok, _ := e.Has(ctx, object.Sum([]byte("other")), 5); ok {
		t.Fatal("has found what was never stored")
	}
	h, err := e.Check(ctx, rc)
	if err != nil || !h.Readable || !h.Placed || len(h.Shares) != 4 {
		t.Fatalf("check: %+v %v", h, err)
	}
	again, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil || again != rc {
		t.Fatalf("second put: %v", err)
	}
	if _, err := e.Put(ctx, bytes.NewReader(data[:10]), int64(len(data))); !errors.Is(err, ErrSize) {
		t.Fatalf("short reader: %v", err)
	}
	v1, err := e.PutRoot(ctx, Root{Note: "v1"})
	if err != nil || v1 != 1 {
		t.Fatalf("root v1: %d %v", v1, err)
	}
	roots, err := e.GetRoots(ctx, member)
	if err != nil || len(roots.Views) != 1 || roots.Views[0].Root == nil || roots.Views[0].Root.Note != "v1" || roots.Views[0].Version != 1 {
		t.Fatalf("roots: %+v %v", roots, err)
	}
	if v2, _ := e.PutRoot(ctx, Root{Note: "v2"}); v2 != 2 {
		t.Fatalf("root v2: %d", v2)
	}
	roots, _ = e.GetRoots(ctx, member)
	if len(roots.Views) != 1 || roots.Views[0].Version != 2 || roots.Views[0].Root.Note != "v2" || roots.Answered < 1 {
		t.Fatalf("roots after v2: %+v", roots)
	}
	if roots, _ := e.GetRoots(ctx, other); len(roots.Views) != 0 {
		t.Fatal("another member's roots listed")
	}
}

func TestFakeContract(t *testing.T) {
	var member, device, other identity.Key
	member[0], device[0], other[0] = 1, 2, 3
	f := NewFake(member, device, 2, 4)
	contract(t, f, member, other)
	if f.Puts != 1 {
		t.Fatalf("the fake uploaded %d times", f.Puts)
	}
}

func TestNativeContract(t *testing.T) {
	g, alice, bob := newGrid(t)
	e, _ := g.engineFor(t, alice)
	contract(t, e, alice.key, bob.key)
	// Placement: two shares on each member's server.
	ctx := context.Background()
	data := pattern(2*object.DefaultSegmentSize+777, 1)
	rc, _ := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	h, _ := e.Check(ctx, rc)
	if h.Servers != 2 || h.Members != 2 || len(h.Holders[alice.dev.ID()]) != 2 || len(h.Holders[bob.dev.ID()]) != 2 {
		t.Fatalf("placement: %+v", h.Holders)
	}
}

func TestRefusedServerIsReplaced(t *testing.T) {
	g, alice, bob := newGrid(t)
	g.servers[bob.dev.ID()].pol.mu.Lock()
	g.servers[bob.dev.ID()].pol.caps[alice.key] = 0 // bob's server has no room for alice
	g.servers[bob.dev.ID()].pol.mu.Unlock()
	e, _ := g.engineFor(t, alice)
	ctx := context.Background()
	data := pattern(object.DefaultSegmentSize+5, 2)
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := e.Check(ctx, rc)
	if !h.Placed || h.Servers != 1 || len(h.Holders[alice.dev.ID()]) != 4 {
		t.Fatalf("after a refusal every share must land on the other server: %+v", h)
	}
	var out bytes.Buffer
	if err := e.Get(ctx, rc, &out, nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("get: %v", err)
	}
}

func TestOneServerDownThenRepair(t *testing.T) {
	g, alice, bob := newGrid(t)
	e, _ := g.engineFor(t, alice)
	ctx := context.Background()
	data := pattern(object.DefaultSegmentSize+9, 3)
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	_ = g.servers[bob.dev.ID()].ep.Close()
	var out bytes.Buffer
	if err := e.Get(ctx, rc, &out, nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("get with bob down: %v", err)
	}
	h, _ := e.Check(ctx, rc)
	if !h.Readable || h.Placed || len(h.Shares) != 2 || h.Answered != 1 {
		t.Fatalf("check with bob down: %+v", h)
	}
	h, err = e.Repair(ctx, rc)
	if err != nil || !h.Placed || len(h.Holders[alice.dev.ID()]) != 4 {
		t.Fatalf("repair onto the survivor: %+v %v", h, err)
	}
	// Roots need a majority: one of two is not enough.
	if _, err := e.PutRoot(ctx, Root{Note: "alone"}); !errors.Is(err, ErrQuorum) {
		t.Fatalf("root without quorum: %v", err)
	}
	roots, _ := e.GetRoots(ctx, alice.key)
	if roots.Answered != 1 || roots.Servers != 2 {
		t.Fatalf("roots answered %d of %d", roots.Answered, roots.Servers)
	}
}

func TestPlan(t *testing.T) {
	var a, b, c identity.Key
	a[0], b[0], c[0] = 1, 2, 3
	servers := []server{{a, "alice"}, {b, "bob"}, {c, "alice"}}
	var si object.StorageIndex
	got := plan(si, 4, []int{0, 1, 2, 3}, servers, nil, nil)
	counts := map[identity.Key]int{}
	for _, dev := range got {
		counts[dev]++
	}
	if len(got) != 4 || counts[a] > 2 || counts[b] > 2 || counts[c] > 2 || len(counts) != 3 {
		t.Fatalf("plan over three servers: %v", counts)
	}
	// Consecutive shares alternate members.
	if got[0] == got[1] {
		t.Fatalf("shares 0 and 1 on one server: %v", got)
	}
	got = plan(si, 4, []int{2, 3}, servers, map[identity.Key]int{a: 2}, map[identity.Key]bool{b: true})
	if len(got) != 2 || got[2] != c || got[3] != c {
		t.Fatalf("plan around a full and a bad server: %v", got)
	}
}
