package shareproto

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
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// A member with one device on the mesh.
type member struct {
	seed identity.Seed
	key  identity.Key
	conv [32]byte
	dev  *identity.DeviceKey
	cert *identity.SignedCertificate
	tls  *identity.TLSIdentity
}

func newMember(t *testing.T, b byte, roles ...string) *member {
	t.Helper()
	var s identity.Seed
	s[0] = b
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
	return &member{seed: s, key: s.MemberKey(), conv: object.ConvergenceSecret(s.Secret()), dev: dev, cert: cert, tls: tlsID}
}

// policy is the manifest as the server sees it.
type policy struct {
	self    identity.Key
	hubs    map[identity.Key]bool
	mu      sync.Mutex
	active  map[identity.Key]bool
	leaving map[identity.Key]bool
	capFor  map[identity.Key]int64
	allowed map[identity.Key]bool
}

func (p *policy) Network() string           { return "testnet" }
func (p *policy) Device() identity.Key      { return p.self }
func (p *policy) IsHub(k identity.Key) bool { return p.hubs[k] }
func (p *policy) MemberStatus(m identity.Key) (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active[m], p.leaving[m]
}
func (p *policy) OwnerCap(o identity.Key) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.capFor[o]; ok {
		return c
	}
	return 1 << 40
}
func (p *policy) allow(pr identity.Peer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowed[pr.Device] {
		return errors.New("not admitted")
	}
	return nil
}

type world struct {
	ctx    context.Context
	pol    *policy
	hub    *hub.Server
	hubKey identity.Key
	st     *store.Store
	server *member
	srvEp  *transport.Endpoint
	log    *slog.Logger
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &world{ctx: ctx, pol: &policy{hubs: map[identity.Key]bool{}, active: map[identity.Key]bool{}, leaving: map[identity.Key]bool{}, capFor: map[identity.Key]int64{}, allowed: map[identity.Key]bool{}}}
	w.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := newMember(t, 100, identity.RoleHub)
	w.pol.allowed[h.dev.ID()] = true
	hs, err := hub.Start(ctx, hub.Config{Identity: h.tls, Device: h.dev.ID(), Allow: w.pol.allow, UDP: "127.0.0.1:0", Log: w.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	w.hub, w.hubKey = hs, h.dev.ID()

	root := filepath.Join(t.TempDir(), "disk")
	_ = os.Mkdir(root, 0o755)
	st, err := store.Open(store.Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "storage-id"), Floor: 1, Log: w.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	w.st = st
	w.server = newMember(t, 1, identity.RoleClient, identity.RoleStorage)
	w.pol.active[w.server.key] = true
	w.pol.self = w.server.dev.ID()
	srv := NewServer(st, w.pol, w.log)
	w.srvEp = w.endpoint(t, w.server, srv.Handle)
	return w
}

func (w *world) endpoint(t *testing.T, m *member, onStream func(identity.Peer, string, *quic.Stream)) *transport.Endpoint {
	t.Helper()
	w.pol.mu.Lock()
	w.pol.allowed[m.dev.ID()] = true
	w.pol.mu.Unlock()
	cfg := transport.Config{Identity: m.tls, Device: m.dev.ID(), Allow: w.pol.allow, Hubs: []transport.HubAddr{{Key: w.hubKey, UDP: w.hub.UDPAddr().String()}},
		Listen: "127.0.0.1:0", LoopbackCandidates: true, Log: w.log}
	if onStream != nil {
		cfg.OnStream = onStream
	}
	ep, err := transport.Start(w.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

type obj struct {
	sd     *object.SignedDescriptor
	blocks [][][]byte
	data   []byte
	cap    object.ReadCap
}

var params = object.Params{K: 2, N: 4, SegmentSize: 8192, Cipher: object.CipherAESGCM}

func encode(t *testing.T, m *member, n int, salt byte) *obj {
	t.Helper()
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*11+5) ^ salt
	}
	r := bytes.NewReader(data)
	h, _ := object.HashSpool(r, int64(n))
	e, err := object.Encode(r, int64(n), h, params, m.conv, m.key)
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([][][]byte, params.N)
	for i := range blocks {
		blocks[i] = make([][]byte, e.Geometry().Segments())
	}
	_ = e.Shares(context.Background(), func(share, seg int, b []byte) error { blocks[share][seg] = append([]byte(nil), b...); return nil })
	sd, err := object.Sign(e.Descriptor, m.dev, m.cert)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := e.ReadCap()
	return &obj{sd: sd, blocks: blocks, data: data, cap: rc}
}

// device is another device of the same member: a laptop beside the node.
func device(t *testing.T, m *member) *member {
	t.Helper()
	dev, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := identity.Issue(m.seed, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: m.key, Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 2})
	if err != nil {
		t.Fatal(err)
	}
	tlsID, err := identity.NewTLSIdentity(dev, cert)
	if err != nil {
		t.Fatal(err)
	}
	return &member{seed: m.seed, key: m.key, conv: m.conv, dev: dev, cert: cert, tls: tlsID}
}

func client(t *testing.T, w *world, m *member) (*Client, context.Context, *member) {
	t.Helper()
	d := device(t, m)
	ep := w.endpoint(t, d, nil)
	ep.Connect(w.server.dev.ID())
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	t.Cleanup(cancel)
	return NewClient(ep.OpenStream), ctx, d
}

func TestPutHasGetAndResume(t *testing.T) {
	w := newWorld(t)
	c, ctx, _ := client(t, w, w.server)
	peer := w.server.dev.ID()
	o := encode(t, w.server, 3*8192+100, 1)
	si := o.sd.Descriptor.StorageIndex

	if p, err := c.Has(ctx, peer, si); err != nil || p != nil {
		t.Fatalf("has before: %+v %v", p, err)
	}
	if err := c.PutShare(ctx, peer, o.sd, 1, func(s int) ([]byte, error) { return o.blocks[1][s], nil }); err != nil {
		t.Fatal(err)
	}
	p, err := c.Has(ctx, peer, si)
	if err != nil || p == nil || len(p.Complete) != 1 || p.Complete[0] != 1 || p.Owner != w.server.key {
		t.Fatalf("has after: %+v %v", p, err)
	}
	// Again: nothing moves.
	calls := 0
	if err := c.PutShare(ctx, peer, o.sd, 1, func(s int) ([]byte, error) { calls++; return o.blocks[1][s], nil }); err != nil || calls != 0 {
		t.Fatalf("second put: %v, %d blocks sent", err, calls)
	}
	// A sender that fails after two blocks leaves a partial the next put resumes.
	fail := errors.New("laptop closed")
	err = c.PutShare(ctx, peer, o.sd, 2, func(s int) ([]byte, error) {
		if s == 2 {
			return nil, fail
		}
		return o.blocks[2][s], nil
	})
	if !errors.Is(err, fail) {
		t.Fatalf("interrupted put: %v", err)
	}
	var partial uint32
	for i := 0; i < 50; i++ {
		p, _ = c.Has(ctx, peer, si)
		if p != nil && p.Partial[2] == 2 {
			partial = 2
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if partial != 2 {
		t.Fatalf("partial after interruption: %+v", p)
	}
	sent := []int{}
	if err := c.PutShare(ctx, peer, o.sd, 2, func(s int) ([]byte, error) { sent = append(sent, s); return o.blocks[2][s], nil }); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0] != 2 {
		t.Fatalf("resume sent segments %v", sent)
	}
	// Get the whole share and a range, and decode from the two shares.
	var got [][]byte
	hashes, err := c.GetShare(ctx, peer, si, 1, 0, 0, o.sd.Descriptor.Geometry(), func(s int, b []byte) error { got = append(got, b); return nil })
	if err != nil || len(got) != 4 || !bytes.Equal(got[3], o.blocks[1][3]) || object.ShareRoot(hashes) != o.sd.Descriptor.ShareRoots[1] {
		t.Fatalf("get: %v", err)
	}
	var rng [][]byte
	if _, err := c.GetShare(ctx, peer, si, 2, 1, 3, o.sd.Descriptor.Geometry(), func(s int, b []byte) error { rng = append(rng, b); return nil }); err != nil || len(rng) != 2 || !bytes.Equal(rng[0], o.blocks[2][1]) {
		t.Fatalf("get range: %v", err)
	}
	var out bytes.Buffer
	src := func(n int) Source {
		return Source{Share: n, Client: c, Peer: peer, StorageIndex: si, Geometry: o.sd.Descriptor.Geometry()}
	}
	srcs, err := Sources(ctx, []Source{src(1), src(2)})
	if err != nil {
		t.Fatal(err)
	}
	if err := object.Decode(ctx, &out, o.cap, &o.sd.Descriptor, &w.server.conv, srcs); err != nil || !bytes.Equal(out.Bytes(), o.data) {
		t.Fatalf("decode over the protocol: %v", err)
	}
	if _, err := c.GetShare(ctx, peer, si, 3, 0, 0, o.sd.Descriptor.Geometry(), func(int, []byte) error { return nil }); !isStatus(err, StatusNotFound) {
		t.Fatalf("get of a missing share: %v", err)
	}
}

func isStatus(err error, status string) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

func TestRefusals(t *testing.T) {
	w := newWorld(t)
	peer := w.server.dev.ID()
	stranger := newMember(t, 2, identity.RoleClient)
	sc, ctx, _ := client(t, w, stranger)
	o := encode(t, stranger, 5000, 2)
	// Not an active member: refused even before the quota.
	if err := sc.PutShare(ctx, peer, o.sd, 0, func(s int) ([]byte, error) { return o.blocks[0][s], nil }); !isStatus(err, StatusRefused) {
		t.Fatalf("inactive member: %v", err)
	}
	w.pol.mu.Lock()
	w.pol.active[stranger.key] = true
	w.pol.capFor[stranger.key] = 100
	w.pol.mu.Unlock()
	if err := sc.PutShare(ctx, peer, o.sd, 0, func(s int) ([]byte, error) { return o.blocks[0][s], nil }); !isStatus(err, StatusQuota) {
		t.Fatalf("over the cap: %v", err)
	}
	w.pol.mu.Lock()
	delete(w.pol.capFor, stranger.key)
	w.pol.mu.Unlock()
	if err := sc.PutShare(ctx, peer, o.sd, 0, func(s int) ([]byte, error) { return o.blocks[0][s], nil }); err != nil {
		t.Fatal(err)
	}
	// Another member's device may not store the stranger's object.
	oc, octx, _ := client(t, w, w.server)
	if err := oc.PutShare(octx, peer, o.sd, 1, func(s int) ([]byte, error) { return o.blocks[1][s], nil }); !isStatus(err, StatusRefused) {
		t.Fatalf("another member storing the object: %v", err)
	}
	// A tombstoned object is served to the owner only.
	dh, _ := o.sd.Descriptor.Hash()
	_ = w.st.Tombstone(o.sd.Descriptor.StorageIndex, stranger.key, dh, time.Now())
	if _, err := oc.GetShare(octx, peer, o.sd.Descriptor.StorageIndex, 0, 0, 0, o.sd.Descriptor.Geometry(), func(int, []byte) error { return nil }); !isStatus(err, StatusTombstoned) {
		t.Fatalf("tombstoned to another member: %v", err)
	}
	if _, err := sc.GetShare(ctx, peer, o.sd.Descriptor.StorageIndex, 0, 0, 0, o.sd.Descriptor.Geometry(), func(int, []byte) error { return nil }); err != nil {
		t.Fatalf("tombstoned to the owner: %v", err)
	}
	// A descriptor whose signature does not verify is a bad request.
	bad := *o.sd
	bad.Signature = append([]byte(nil), bad.Signature...)
	bad.Signature[0] ^= 1
	if err := sc.PutShare(ctx, peer, &bad, 2, func(s int) ([]byte, error) { return o.blocks[2][s], nil }); !isStatus(err, StatusBad) {
		t.Fatalf("bad signature: %v", err)
	}
}

func TestRoots(t *testing.T) {
	w := newWorld(t)
	peer := w.server.dev.ID()
	c, ctx, laptop := client(t, w, w.server)
	e1, err := SignRoot("testnet", laptop.dev, laptop.cert, 1, []byte("ciphertext v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutRoot(ctx, peer, e1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutRoot(ctx, peer, e1); !isStatus(err, StatusLowerVersion) {
		t.Fatalf("same version again: %v", err)
	}
	e2, _ := SignRoot("testnet", laptop.dev, laptop.cert, 2, []byte("ciphertext v2"))
	if err := c.PutRoot(ctx, peer, e2); err != nil {
		t.Fatal(err)
	}
	roots, err := c.GetRoots(ctx, peer, w.server.key)
	if err != nil || len(roots) != 1 || roots[0].Version != 2 || string(roots[0].Ciphertext) != "ciphertext v2" || roots[0].Verify("testnet") != nil {
		t.Fatalf("roots: %v %+v", err, roots)
	}
	// Another member's device can neither publish this device's root nor list the member's roots.
	other := newMember(t, 3, identity.RoleClient)
	w.pol.mu.Lock()
	w.pol.active[other.key] = true
	w.pol.mu.Unlock()
	oc, octx, _ := client(t, w, other)
	if err := oc.PutRoot(octx, peer, e2); !isStatus(err, StatusRefused) {
		t.Fatalf("root from another device: %v", err)
	}
	if _, err := oc.GetRoots(octx, peer, w.server.key); !isStatus(err, StatusRefused) {
		t.Fatalf("roots of another member: %v", err)
	}
	// A tampered envelope is a bad request.
	bad := *e2
	bad.Version = 3
	if err := c.PutRoot(ctx, peer, &bad); !isStatus(err, StatusBad) {
		t.Fatalf("tampered root: %v", err)
	}
}

// TestStreamsAreReleased: a backup makes thousands of requests on one
// connection; each must release its stream, or the server's stream limit
// (1024 at the endpoint) stalls every request after it.
func TestStreamsAreReleased(t *testing.T) {
	w := newWorld(t)
	c, _, _ := client(t, w, w.server)
	peer := w.server.dev.ID()
	o := encode(t, w.server, 100, 7)
	for i := 0; i < 1500; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := c.Has(ctx, peer, o.sd.Descriptor.StorageIndex)
		cancel()
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
}

// A device of another member stores a share only as a repair worker with
// a task a hub signed, and then past the owner's cap, never past a
// tombstone.
func TestRepairPutNeedsAValidTask(t *testing.T) {
	w := newWorld(t)
	peer := w.server.dev.ID()
	alice := w.server
	bob := newMember(t, 2, identity.RoleClient, identity.RoleStorage)
	w.pol.mu.Lock()
	w.pol.active[bob.key] = true
	w.pol.mu.Unlock()
	hubSigner, _ := identity.NewDeviceKey()
	w.pol.mu.Lock()
	w.pol.hubs[hubSigner.ID()] = true
	w.pol.mu.Unlock()
	bc, ctx, worker := client(t, w, bob)
	o := encode(t, alice, 3*8192+100, 3)
	si := o.sd.Descriptor.StorageIndex
	dhash, _ := o.sd.Descriptor.Hash()
	task := func(worker identity.Key, number uint8, server identity.Key, expires time.Duration, signer *identity.DeviceKey) *wire.RepairTask {
		tk := &wire.RepairTask{Network: "testnet", ID: 1, StorageIndex: si, Owner: alice.key, DescriptorHash: dhash, Worker: worker,
			Shares: []wire.RepairShare{{Number: number, Server: server}}, Issued: time.Now(), Expires: time.Now().Add(expires)}
		if err := tk.Sign(signer); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	putWith := func(number uint8, tk *wire.RepairTask) error {
		ps, err := bc.BeginRepairPut(ctx, peer, o.sd, number, tk)
		if err != nil {
			return err
		}
		for s := range o.blocks[number] {
			if err := ps.Write(s, o.blocks[number][s]); err != nil {
				ps.Abandon()
				return err
			}
		}
		return ps.Finish()
	}
	me := worker.dev.ID()
	if err := bc.PutShare(ctx, peer, o.sd, 2, func(s int) ([]byte, error) { return o.blocks[2][s], nil }); !isStatus(err, StatusRefused) {
		t.Fatalf("a stranger's put without a task: %v", err)
	}
	stranger, _ := identity.NewDeviceKey()
	var other identity.Key
	other[0] = 7
	for name, tk := range map[string]*wire.RepairTask{
		"signed by a key that is not a hub": task(me, 2, peer, time.Hour, stranger),
		"naming another worker":             task(other, 2, peer, time.Hour, hubSigner),
		"naming another server":             task(me, 2, other, time.Hour, hubSigner),
		"naming another share number":       task(me, 3, peer, time.Hour, hubSigner),
		"expired":                           task(me, 2, peer, -time.Minute, hubSigner),
	} {
		if err := putWith(2, tk); !isStatus(err, StatusRefused) {
			t.Fatalf("task %s: %v", name, err)
		}
	}
	if err := putWith(2, task(me, 2, peer, time.Hour, hubSigner)); err != nil {
		t.Fatalf("a valid task: %v", err)
	}
	p, err := bc.Has(ctx, peer, si)
	if err != nil || p == nil || len(p.Complete) != 1 || p.Complete[0] != 2 || p.Owner != alice.key {
		t.Fatalf("after the repair put: %+v %v", p, err)
	}
	// The same put again moves nothing.
	if ps, err := bc.BeginRepairPut(ctx, peer, o.sd, 2, task(me, 2, peer, time.Hour, hubSigner)); err != nil || !ps.Complete() {
		t.Fatalf("repeat: %v", err)
	} else {
		ps.Abandon()
	}
	// Past the owner's cap: the owner is refused, repair goes through.
	w.pol.mu.Lock()
	w.pol.capFor[alice.key] = 0
	w.pol.mu.Unlock()
	ac, actx, _ := client(t, w, alice)
	if err := ac.PutShare(actx, peer, o.sd, 0, func(s int) ([]byte, error) { return o.blocks[0][s], nil }); !isStatus(err, StatusQuota) {
		t.Fatalf("owner past the cap: %v", err)
	}
	if err := putWith(0, task(me, 0, peer, time.Hour, hubSigner)); err != nil {
		t.Fatalf("repair past the cap: %v", err)
	}
	// A leaving member: writes refused, repair on their behalf continues.
	w.pol.mu.Lock()
	w.pol.active[alice.key], w.pol.leaving[alice.key] = false, true
	w.pol.mu.Unlock()
	if err := ac.PutShare(actx, peer, o.sd, 1, func(s int) ([]byte, error) { return o.blocks[1][s], nil }); !isStatus(err, StatusRefused) {
		t.Fatalf("leaving owner: %v", err)
	}
	if err := putWith(1, task(me, 1, peer, time.Hour, hubSigner)); err != nil {
		t.Fatalf("repair for a leaving member: %v", err)
	}
	// A tombstone is never crossed by repair.
	if err := w.st.Tombstone(si, alice.key, dhash, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := putWith(3, task(me, 3, peer, time.Hour, hubSigner)); !isStatus(err, StatusTombstoned) {
		t.Fatalf("repair of a tombstoned object: %v", err)
	}
}
