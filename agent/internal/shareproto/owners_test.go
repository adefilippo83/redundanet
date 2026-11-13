package shareproto

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// squat is what a member that saw a storage index announced can make: a
// descriptor of its own, for bytes of its own, under that index.
func squat(t *testing.T, m *member, si object.StorageIndex, n int, salt byte) *obj {
	t.Helper()
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*7+3) ^ salt
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
	d := e.Descriptor
	d.StorageIndex = si
	sd, err := object.Sign(d, m.dev, m.cert)
	if err != nil {
		t.Fatal(err)
	}
	return &obj{sd: sd, blocks: blocks, data: data}
}

func put(t *testing.T, ctx context.Context, c *Client, peer identity.Key, o *obj, n uint8) error {
	t.Helper()
	return c.PutShare(ctx, peer, o.sd, n, func(s int) ([]byte, error) { return o.blocks[n][s], nil })
}

// The state of today, kept until every server has the wider key: a member
// that registers a descriptor under an index first holds it, and the
// owner's upload is refused with the status it always had.
func TestASecondOwnerIsRefusedOverTheProtocol(t *testing.T) {
	w := newWorld(t)
	peer := w.server.dev.ID()
	alice := w.server
	mallory := newMember(t, 2, identity.RoleClient, identity.RoleStorage)
	w.pol.mu.Lock()
	w.pol.active[mallory.key] = true
	w.pol.mu.Unlock()
	ac, actx, _ := client(t, w, alice)
	mc, mctx, _ := client(t, w, mallory)
	o := encode(t, alice, 2*8192+10, 1)
	si := o.sd.Descriptor.StorageIndex
	sq := squat(t, mallory, si, 300, 9)
	if err := put(t, mctx, mc, peer, sq, 0); err != nil {
		t.Fatal(err)
	}
	if err := put(t, actx, ac, peer, o, 0); !isStatus(err, StatusDescriptor) {
		t.Fatalf("the owner's put under a taken index: %v", err)
	}
	// What the server holds under the index is not the owner's object, and
	// the owner is told nothing is held of it.
	if p, err := ac.Has(actx, peer, alice.key, si); err != nil || p != nil {
		t.Fatalf("the owner's has: %+v %v", p, err)
	}
	if p, err := ac.Has(actx, peer, mallory.key, si); err != nil || p == nil || p.Owner != mallory.key {
		t.Fatalf("a has that names the other member: %+v %v", p, err)
	}
}

// The attack once second owners are accepted: a second member registers a
// descriptor under an index first, and the owner's upload, read, delete
// and repair all go through; the second member's shares count against
// its own budget and its tombstone touches its own rows alone.
func TestTheOwnerIsNotHeldUpByASecondOwner(t *testing.T) {
	w := newWorld(t)
	if err := w.st.AcceptSecondOwners(); err != nil {
		t.Fatal(err)
	}
	peer := w.server.dev.ID()
	alice := w.server
	mallory := newMember(t, 2, identity.RoleClient, identity.RoleStorage)
	hubSigner, _ := identity.NewDeviceKey()
	w.pol.mu.Lock()
	w.pol.active[mallory.key] = true
	w.pol.hubs[hubSigner.ID()] = true
	w.pol.mu.Unlock()
	ac, actx, laptop := client(t, w, alice)
	mc, mctx, mdev := client(t, w, mallory)
	o := encode(t, alice, 2*8192+10, 1)
	si := o.sd.Descriptor.StorageIndex
	odh, _ := o.sd.Descriptor.Hash()
	sq := squat(t, mallory, si, 300, 9)
	sdh, _ := sq.sd.Descriptor.Hash()

	// Mallory is first, on every share number.
	for n := uint8(0); n < 4; n++ {
		if err := put(t, mctx, mc, peer, sq, n); err != nil {
			t.Fatal(err)
		}
	}
	before, err := mc.Usage(mctx, peer)
	if err != nil || before == 0 {
		t.Fatalf("mallory's usage: %d %v", before, err)
	}
	if u, _ := ac.Usage(actx, peer); u != 0 {
		t.Fatalf("alice is charged %d bytes for mallory's shares", u)
	}
	// Upload.
	for _, n := range []uint8{0, 1} {
		if err := put(t, actx, ac, peer, o, n); err != nil {
			t.Fatalf("the owner's put of share %d: %v", n, err)
		}
	}
	if u, _ := mc.Usage(mctx, peer); u != before {
		t.Fatalf("mallory's usage moved from %d to %d with alice's upload", before, u)
	}
	// Presence and read, each for its own object.
	p, err := ac.Has(actx, peer, alice.key, si)
	if err != nil || p == nil || p.Owner != alice.key || p.DescriptorHash != odh || len(p.Complete) != 2 {
		t.Fatalf("the owner's has: %+v %v", p, err)
	}
	if p, _ := mc.Has(mctx, peer, mallory.key, si); p == nil || p.DescriptorHash != sdh || len(p.Complete) != 4 {
		t.Fatalf("mallory's has: %+v", p)
	}
	sd, _, err := ac.GetInfo(actx, peer, alice.key, si, 0)
	if err != nil || sd.Descriptor.Owner != alice.key {
		t.Fatalf("the owner's descriptor: %v", err)
	}
	var got [][]byte
	if _, err := ac.GetShare(actx, peer, alice.key, si, 1, 0, 0, o.sd.Descriptor.Geometry(), func(_ int, b []byte) error { got = append(got, b); return nil }); err != nil {
		t.Fatalf("the owner's read: %v", err)
	}
	for s := range got {
		if !bytes.Equal(got[s], o.blocks[1][s]) {
			t.Fatalf("segment %d of the owner's share is other bytes", s)
		}
	}
	// A share number the owner has not stored is not found, though
	// mallory holds that number.
	if _, _, err := ac.GetInfo(actx, peer, alice.key, si, 3); !isStatus(err, StatusNotFound) {
		t.Fatalf("a share the owner never stored: %v", err)
	}
	// Repair, by a worker with the hub's task for the owner's object.
	task := &wire.RepairTask{Network: "testnet", ID: 1, StorageIndex: si, Owner: alice.key, DescriptorHash: odh, Worker: mdev.dev.ID(),
		Shares: []wire.RepairShare{{Number: 2, Server: peer}}, Issued: time.Now(), Expires: time.Now().Add(time.Hour)}
	if err := task.Sign(hubSigner); err != nil {
		t.Fatal(err)
	}
	ps, err := mc.BeginRepairPut(mctx, peer, o.sd, 2, task)
	if err != nil {
		t.Fatalf("the repair put: %v", err)
	}
	for s := range o.blocks[2] {
		if err := ps.Write(s, o.blocks[2][s]); err != nil {
			t.Fatal(err)
		}
	}
	if err := ps.Finish(); err != nil {
		t.Fatalf("the repair put: %v", err)
	}
	if p, _ := ac.Has(actx, peer, alice.key, si); len(p.Complete) != 3 {
		t.Fatalf("after the repair put: %+v", p)
	}
	// Listing: each member sees its own object, once.
	if l, err := ac.List(actx, peer); err != nil || len(l) != 1 || l[0].DescriptorHash != odh {
		t.Fatalf("the owner's list: %+v %v", l, err)
	}
	if l, err := mc.List(mctx, peer); err != nil || len(l) != 1 || l[0].DescriptorHash != sdh {
		t.Fatalf("mallory's list: %+v %v", l, err)
	}
	// Mallory's tombstone touches mallory's rows alone.
	w.setNow(time.Now().Add(store.Immunity + time.Hour))
	mt, err := SignDelete("testnet", mdev.dev, mdev.cert, si, sdh, time.Now().Add(store.Immunity+2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := mc.Delete(mctx, peer, mt); err != nil || p == nil || !p.Tombstoned {
		t.Fatalf("mallory's delete: %+v %v", p, err)
	}
	if p, _ := ac.Has(actx, peer, alice.key, si); p == nil || p.Tombstoned || len(p.Complete) != 3 {
		t.Fatalf("the owner's object after mallory's delete: %+v", p)
	}
	if _, err := ac.GetShare(actx, peer, alice.key, si, 0, 0, 0, o.sd.Descriptor.Geometry(), func(int, []byte) error { return nil }); err != nil {
		t.Fatalf("the owner's read after mallory's delete: %v", err)
	}
	// A token mallory signs for the owner's descriptor is about mallory's
	// object, which is under another descriptor.
	forged, _ := SignDelete("testnet", mdev.dev, mdev.cert, si, odh, time.Now().Add(store.Immunity+2*time.Hour))
	if _, err := mc.Delete(mctx, peer, forged); !isStatus(err, StatusDescriptor) {
		t.Fatalf("mallory's token for the owner's descriptor: %v", err)
	}
	// The owner's delete, and the owner's alone, tombstones the object.
	// The owner's own questions above touched it, so the immunity runs
	// again first.
	w.setNow(time.Now().Add(2*store.Immunity + 2*time.Hour))
	at, err := SignDelete("testnet", laptop.dev, laptop.cert, si, odh, time.Now().Add(2*store.Immunity+3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := ac.Delete(actx, peer, at); err != nil || p == nil || !p.Tombstoned || p.Owner != alice.key {
		t.Fatalf("the owner's delete: %+v %v", p, err)
	}
	if err := mc.Undelete(mctx, peer, si); err != nil {
		t.Fatalf("mallory's undelete of its own: %v", err)
	}
	if p, _ := ac.Has(actx, peer, alice.key, si); p == nil || !p.Tombstoned {
		t.Fatalf("mallory's undelete cleared the owner's tombstone: %+v", p)
	}
}

// A device from before the owner was part of a request sends none. It is
// answered for its own member's object when there is one, otherwise for
// the only object under the index, and with not-found when two members
// hold one and it is neither.
func TestARequestWithoutAnOwner(t *testing.T) {
	w := newWorld(t)
	if err := w.st.AcceptSecondOwners(); err != nil {
		t.Fatal(err)
	}
	peer := w.server.dev.ID()
	alice := w.server
	mallory := newMember(t, 2, identity.RoleClient, identity.RoleStorage)
	carol := newMember(t, 3, identity.RoleClient, identity.RoleStorage)
	w.pol.mu.Lock()
	w.pol.active[mallory.key], w.pol.active[carol.key] = true, true
	w.pol.mu.Unlock()
	ac, actx, _ := client(t, w, alice)
	mc, mctx, _ := client(t, w, mallory)
	cc, cctx, _ := client(t, w, carol)
	o := encode(t, alice, 2*8192+10, 1)
	si := o.sd.Descriptor.StorageIndex
	odh, _ := o.sd.Descriptor.Hash()
	sq := squat(t, mallory, si, 300, 9)
	sdh, _ := sq.sd.Descriptor.Hash()

	has := func(ctx context.Context, c *Client) (*Presence, error) {
		st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeHas, StorageIndex: si})
		if err != nil {
			return nil, err
		}
		done(st)
		return resp.Presence, nil
	}
	info := func(ctx context.Context, c *Client) (*object.SignedDescriptor, error) {
		st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGet, StorageIndex: si, Number: 0, HashesOnly: true})
		if err != nil {
			return nil, err
		}
		done(st)
		return resp.Descriptor, nil
	}
	// Nothing under the index.
	if p, err := has(cctx, cc); err != nil || p != nil {
		t.Fatalf("nothing held: %+v %v", p, err)
	}
	if _, err := info(cctx, cc); !isStatus(err, StatusNotFound) {
		t.Fatalf("a get of nothing: %v", err)
	}
	// One owner: its own device, and anyone else, a repair worker of the
	// version before for one, are answered for that object.
	if err := put(t, actx, ac, peer, o, 0); err != nil {
		t.Fatal(err)
	}
	for name, who := range map[string]struct {
		ctx context.Context
		c   *Client
	}{"the owner": {actx, ac}, "another member": {cctx, cc}} {
		if p, err := has(who.ctx, who.c); err != nil || p == nil || p.Owner != alice.key || p.DescriptorHash != odh {
			t.Fatalf("%s, one owner under the index: %+v %v", name, p, err)
		}
		if sd, err := info(who.ctx, who.c); err != nil || sd.Descriptor.Owner != alice.key {
			t.Fatalf("%s, a get with one owner under the index: %v", name, err)
		}
	}
	// Two owners: each is answered for its own, a third for neither.
	if err := put(t, mctx, mc, peer, sq, 0); err != nil {
		t.Fatal(err)
	}
	if p, err := has(actx, ac); err != nil || p == nil || p.DescriptorHash != odh {
		t.Fatalf("the owner, two owners under the index: %+v %v", p, err)
	}
	if p, err := has(mctx, mc); err != nil || p == nil || p.DescriptorHash != sdh {
		t.Fatalf("the second owner, two owners under the index: %+v %v", p, err)
	}
	if sd, err := info(actx, ac); err != nil || sd.Descriptor.Owner != alice.key {
		t.Fatalf("the owner's get, two owners under the index: %v", err)
	}
	if sd, err := info(mctx, mc); err != nil || sd.Descriptor.Owner != mallory.key {
		t.Fatalf("the second owner's get, two owners under the index: %v", err)
	}
	if _, err := has(cctx, cc); !isStatus(err, StatusNotFound) {
		t.Fatalf("a third member's has, two owners under the index: %v", err)
	}
	if _, err := info(cctx, cc); !isStatus(err, StatusNotFound) {
		t.Fatalf("a third member's get, two owners under the index: %v", err)
	}
	// With the owner named, the third member is answered.
	if p, err := cc.Has(cctx, peer, alice.key, si); err != nil || p == nil || p.DescriptorHash != odh {
		t.Fatalf("a third member naming the owner: %+v %v", p, err)
	}
}

// A server from before the owner was part of a request does not read it
// and answers for whoever holds the storage index. What it holds for
// another member is nothing of the object asked about.
func TestAServerThatDoesNotReadTheOwner(t *testing.T) {
	var mine, other identity.Key
	mine[0], other[0] = 1, 2
	var si object.StorageIndex
	si[0] = 5
	held := other
	oldServer := func(_ identity.Peer, _ string, st wire.Stream) {
		defer st.Close()
		defer st.CancelRead(0)
		var req Request
		if err := readMsg(st, &req); err != nil {
			return
		}
		_ = writeMsg(st, Response{Status: StatusOK, Presence: &Presence{Owner: held, Complete: []uint8{0, 1, 2, 3}}})
	}
	c := NewClient(LocalOpener(identity.Peer{Member: mine}, oldServer))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p, err := c.Has(ctx, other, mine, si); err != nil || p != nil {
		t.Fatalf("another member's shares counted as this object's: %+v %v", p, err)
	}
	held = mine
	if p, err := c.Has(ctx, other, mine, si); err != nil || p == nil || len(p.Complete) != 4 {
		t.Fatalf("the member's own shares on such a server: %+v %v", p, err)
	}
}
