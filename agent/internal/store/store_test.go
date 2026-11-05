package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
)

type owner struct {
	member identity.Key
	conv   [32]byte
	dev    *identity.DeviceKey
	cert   *identity.SignedCertificate
}

func newOwner(t *testing.T, b byte) *owner {
	t.Helper()
	var s identity.Seed
	s[0] = b
	ds := make([]byte, 32)
	ds[0] = b + 100
	dev, _ := identity.DeviceKeyFromSeed(ds)
	cert, err := identity.Issue(s, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: s.MemberKey(), Roles: []string{"client"}, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &owner{member: s.MemberKey(), conv: object.ConvergenceSecret(s.Secret()), dev: dev, cert: cert}
}

type obj struct {
	sd     *object.SignedDescriptor
	blocks [][][]byte
}

var small = object.Params{K: 2, N: 4, SegmentSize: 4096, Cipher: object.CipherAESGCM}

func encode(t *testing.T, o *owner, data []byte) *obj {
	t.Helper()
	r := bytes.NewReader(data)
	h, _ := object.HashSpool(r, int64(len(data)))
	e, err := object.Encode(r, int64(len(data)), h, small, o.conv, o.member)
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([][][]byte, small.N)
	for i := range blocks {
		blocks[i] = make([][]byte, e.Geometry().Segments())
	}
	_ = e.Shares(context.Background(), func(share, seg int, b []byte) error { blocks[share][seg] = append([]byte(nil), b...); return nil })
	sd, err := object.Sign(e.Descriptor, o.dev, o.cert)
	if err != nil {
		t.Fatal(err)
	}
	return &obj{sd: sd, blocks: blocks}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "disk")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	s, err := Open(Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "node", "storage-id"), Floor: 1, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

func putAll(t *testing.T, s *Store, o *obj, n uint8) {
	t.Helper()
	p, err := s.BeginPut(o.sd, n)
	if err != nil {
		t.Fatal(err)
	}
	for seg := p.Resume(); seg < p.Segments(); seg++ {
		if err := p.WriteBlock(seg, o.blocks[n][seg]); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
}

func data(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13+1) ^ salt
	}
	return b
}

func TestPutResumeAndRead(t *testing.T) {
	s, _ := newStore(t)
	o := newOwner(t, 1)
	ob := encode(t, o, data(3*4096+10, 0))
	si := ob.sd.Descriptor.StorageIndex

	p, err := s.BeginPut(ob.sd, 1)
	if err != nil || p.Resume() != 0 || p.Segments() != 4 {
		t.Fatalf("begin: %v %+v", err, p)
	}
	if err := p.WriteBlock(1, ob.blocks[1][1]); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("out of order: %v", err)
	}
	if err := p.WriteBlock(0, ob.blocks[1][0]); err != nil {
		t.Fatal(err)
	}
	p.Abort()
	pres, _ := s.Has(si)
	if pres == nil || pres.Partial[1] != 1 || len(pres.Complete) != 0 {
		t.Fatalf("after abort: %+v", pres)
	}
	if u, _ := s.Usage(o.member); u == 0 {
		t.Fatal("a partial share must already count against the owner")
	}
	// Resume where it stopped.
	p, err = s.BeginPut(ob.sd, 1)
	if err != nil || p.Resume() != 1 {
		t.Fatalf("resume: %v %d", err, p.Resume())
	}
	for seg := 1; seg < 4; seg++ {
		if err := p.WriteBlock(seg, ob.blocks[1][seg]); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
	pres, _ = s.Has(si)
	if pres == nil || len(pres.Complete) != 1 || pres.Complete[0] != 1 || len(pres.Partial) != 0 || pres.Owner != o.member {
		t.Fatalf("after complete: %+v", pres)
	}
	if _, err := s.BeginPut(ob.sd, 1); !errors.Is(err, ErrAlreadyComplete) {
		t.Fatalf("second put: %v", err)
	}
	sh, err := s.OpenShare(si, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	if err := sh.Verify(); err != nil {
		t.Fatal(err)
	}
	b, _ := sh.Block(2)
	if !bytes.Equal(b, ob.blocks[1][2]) || len(sh.Hashes) != 4 || sh.Header.Number != 1 {
		t.Fatal("read back differs")
	}
	hdrLen := sh.Header.Len
	want := roundUsage(object.ShareSize(ob.sd.Descriptor.Geometry(), hdrLen))
	if u, _ := s.Usage(o.member); u != want {
		t.Fatalf("usage %d, want %d", u, want)
	}
	shares, bytesHeld, _ := s.Totals()
	if shares != 1 || bytesHeld != want {
		t.Fatalf("totals: %d %d", shares, bytesHeld)
	}
	if _, err := s.OpenShare(si, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing share: %v", err)
	}
	d, _ := s.Census(0)
	if d.Generation != 1 || len(d.Added) != 1 || d.Added[0].Number != 1 || d.Added[0].Owner != o.member || d.Added[0].Size != want {
		t.Fatalf("census: %+v", d)
	}
}

func TestRefusals(t *testing.T) {
	s, _ := newStore(t)
	o := newOwner(t, 1)
	ob := encode(t, o, data(5000, 1))
	if _, err := s.BeginPut(ob.sd, 4); err == nil {
		t.Fatal("share number 4 of n=4 accepted")
	}
	putAll(t, s, ob, 0)
	// Another descriptor for the same storage index is refused.
	other := *ob.sd
	other.Descriptor.CiphertextHash[0] ^= 1
	if _, err := s.BeginPut(&other, 1); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("different descriptor: %v", err)
	}
	// Blocks that do not match the signed root never become a share.
	p, err := s.BeginPut(ob.sd, 2)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), ob.blocks[2][0]...)
	bad[0] ^= 1
	_ = p.WriteBlock(0, bad)
	for seg := 1; seg < p.Segments(); seg++ {
		_ = p.WriteBlock(seg, ob.blocks[2][seg])
	}
	if err := p.Complete(); !errors.Is(err, ErrRoot) {
		t.Fatalf("bad block: %v", err)
	}
	pres, _ := s.Has(ob.sd.Descriptor.StorageIndex)
	if len(pres.Complete) != 1 || len(pres.Partial) != 0 {
		t.Fatalf("the bad share stayed: %+v", pres)
	}
	if _, err := os.Stat(s.sharePath(ob.sd.Descriptor.StorageIndex, 2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the bad share's file stayed")
	}
	// The floor.
	s.cfg.Floor = 1 << 60
	if _, err := s.BeginPut(ob.sd, 3); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("below the floor: %v", err)
	}
}

func TestTombstones(t *testing.T) {
	s, _ := newStore(t)
	o := newOwner(t, 1)
	ob := encode(t, o, data(100, 2))
	si := ob.sd.Descriptor.StorageIndex
	dh, _ := ob.sd.Descriptor.Hash()
	putAll(t, s, ob, 0)
	if err := s.Tombstone(si, o.member, dh, time.Now()); err != nil {
		t.Fatal(err)
	}
	pres, _ := s.Has(si)
	if !pres.Tombstoned {
		t.Fatal("not tombstoned")
	}
	d, _ := s.Census(0)
	if len(d.Tombstones) != 1 || d.Tombstones[0].StorageIndex != si || d.Tombstones[0].Purged {
		t.Fatalf("census tombstones: %+v", d.Tombstones)
	}
	// An upload from a device of the owner clears it.
	putAll(t, s, ob, 1)
	if pres, _ = s.Has(si); pres.Tombstoned {
		t.Fatal("the owner's upload did not clear the tombstone")
	}
	// A tombstone recorded for another owner refuses the put.
	stranger := newOwner(t, 2)
	if err := s.Tombstone(si, stranger.member, dh, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPut(ob.sd, 2); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("tombstoned by another owner: %v", err)
	}
}

func TestExpirePartialsAndRemoval(t *testing.T) {
	s, c := newStore(t)
	o := newOwner(t, 1)
	ob := encode(t, o, data(9000, 3))
	putAll(t, s, ob, 0)
	p, _ := s.BeginPut(ob.sd, 1)
	_ = p.WriteBlock(0, ob.blocks[1][0])
	p.Abort()
	c.t = c.t.Add(8 * 24 * time.Hour)
	n, err := s.ExpirePartials(7 * 24 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	pres, _ := s.Has(ob.sd.Descriptor.StorageIndex)
	if len(pres.Partial) != 0 || len(pres.Complete) != 1 {
		t.Fatalf("after expiry: %+v", pres)
	}
	g, _ := s.Generation()
	if err := s.Remove(ob.sd.Descriptor.StorageIndex, 0, "test"); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Census(g)
	if len(d.Removed) != 1 || d.Removed[0].Number != 0 || len(d.Added) != 0 || d.Generation != g+1 {
		t.Fatalf("census after removal: %+v", d)
	}
	if err := s.Remove(ob.sd.Descriptor.StorageIndex, 0, "again"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
}

func TestScrubFindsCorruption(t *testing.T) {
	s, c := newStore(t)
	o := newOwner(t, 1)
	good := encode(t, o, data(7000, 4))
	bad := encode(t, o, data(7000, 5))
	putAll(t, s, good, 0)
	putAll(t, s, bad, 0)
	f, err := os.OpenFile(s.sharePath(bad.sd.Descriptor.StorageIndex, 0), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	// Flip a byte in the middle of the blocks, after the header.
	off := st.Size() - 100
	var b [1]byte
	_, _ = f.ReadAt(b[:], off)
	b[0] ^= 1
	_, _ = f.WriteAt(b[:], off)
	f.Close()
	if since, err := s.VerifiedSince(); err != nil || !since.IsZero() {
		t.Fatalf("verified since before any check: %v %v", since, err)
	}
	c.t = c.t.Add(time.Hour)
	checked, failed, err := s.Scrub(context.Background(), 10)
	if err != nil || checked != 2 || failed != 1 {
		t.Fatalf("scrub: checked %d failed %d err %v", checked, failed, err)
	}
	if since, err := s.VerifiedSince(); err != nil || !since.Equal(c.t) {
		t.Fatalf("verified since after the pass: %v %v, want %v", since, err, c.t)
	}
	if pres, _ := s.Has(bad.sd.Descriptor.StorageIndex); pres != nil && len(pres.Complete) != 0 {
		t.Fatal("the corrupt share stayed")
	}
	var verified int64
	_ = s.db.QueryRow(`SELECT verified FROM shares WHERE si = ?`, good.sd.Descriptor.StorageIndex[:]).Scan(&verified)
	if verified != c.t.Unix() {
		t.Fatalf("verified_at not recorded: %d", verified)
	}
}

func TestMarker(t *testing.T) {
	s, _ := newStore(t)
	root, rec := s.cfg.Root, s.cfg.RecordPath
	if err := s.Available(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	again, err := Open(Config{Root: root, RecordPath: rec, Floor: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
	// Another node's record does not match the disk.
	if _, err := Open(Config{Root: root, RecordPath: filepath.Join(t.TempDir(), "other-node"), Floor: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("foreign record: %v", err)
	}
	// The marker gone from the disk: unavailable, not recreated.
	_ = os.Remove(filepath.Join(root, markerName))
	if _, err := Open(Config{Root: root, RecordPath: rec, Floor: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, markerName)); err == nil {
		t.Fatal("the marker was recreated")
	}
	// A root that is not a directory is never created.
	if _, err := Open(Config{Root: filepath.Join(t.TempDir(), "missing"), RecordPath: rec, Floor: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing root: %v", err)
	}
}

func TestRoots(t *testing.T) {
	s, _ := newStore(t)
	var member, devA, devB identity.Key
	member[0], devA[0], devB[0] = 1, 2, 3
	for v := uint64(1); v <= 15; v++ {
		if err := s.PutRoot(RootRecord{Member: member, Device: devA, Version: v, Envelope: []byte{byte(v)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutRoot(RootRecord{Member: member, Device: devA, Version: 15, Envelope: []byte{15}}); !errors.Is(err, ErrLowerVersion) {
		t.Fatalf("same version: %v", err)
	}
	if err := s.PutRoot(RootRecord{Member: member, Device: devA, Version: 9, Envelope: []byte{9}}); !errors.Is(err, ErrLowerVersion) {
		t.Fatalf("lower version: %v", err)
	}
	if err := s.PutRoot(RootRecord{Member: member, Device: devB, Version: 1, Envelope: []byte{100}}); err != nil {
		t.Fatal(err)
	}
	roots, err := s.Roots(member)
	if err != nil || len(roots) != 2 || roots[0].Version != 15 || roots[0].Envelope[0] != 15 || roots[1].Version != 1 {
		t.Fatalf("roots: %v %+v", err, roots)
	}
	var kept int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM roots WHERE device = ?`, devA[:]).Scan(&kept)
	if kept != keepRoots+1 {
		t.Fatalf("kept %d versions, want %d", kept, keepRoots+1)
	}
}

func TestDeleteTokensAndTheWindow(t *testing.T) {
	s, c := newStore(t)
	o := newOwner(t, 1)
	ob := encode(t, o, data(9000, 6))
	si := ob.sd.Descriptor.StorageIndex
	dh, _ := ob.sd.Descriptor.Hash()
	putAll(t, s, ob, 0)
	putAll(t, s, ob, 1)
	uploaded := c.t
	// Nothing held: nothing to do, no error.
	var other object.StorageIndex
	other[0] = 0xff
	if p, err := s.ApplyDelete(other, o.member, dh, c.t); err != nil || p != nil {
		t.Fatalf("delete of nothing: %+v %v", p, err)
	}
	// Within the immunity the token is refused; so is another member's,
	// another descriptor's, and one issued before the last touch.
	if _, err := s.ApplyDelete(si, o.member, dh, c.t); !errors.Is(err, ErrRecent) {
		t.Fatalf("within the immunity: %v", err)
	}
	c.t = c.t.Add(Immunity + time.Hour)
	stranger := newOwner(t, 2)
	if _, err := s.ApplyDelete(si, stranger.member, dh, c.t); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("another member's token: %v", err)
	}
	var otherHash object.Hash
	otherHash[0] = 1
	if _, err := s.ApplyDelete(si, o.member, otherHash, c.t); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("another descriptor: %v", err)
	}
	if _, err := s.ApplyDelete(si, o.member, dh, uploaded.Add(-time.Minute)); !errors.Is(err, ErrStale) {
		t.Fatalf("a token older than the last touch: %v", err)
	}
	// A touch by the owner restarts the immunity.
	if err := s.Touch(si); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDelete(si, o.member, dh, c.t); !errors.Is(err, ErrRecent) {
		t.Fatalf("after a touch: %v", err)
	}
	c.t = c.t.Add(Immunity + time.Hour)
	p, err := s.ApplyDelete(si, o.member, dh, c.t)
	if err != nil || !p.Tombstoned || p.Purged {
		t.Fatalf("delete: %+v %v", p, err)
	}
	if p, err := s.ApplyDelete(si, o.member, dh, c.t); err != nil || !p.Tombstoned {
		t.Fatalf("delete twice: %+v %v", p, err)
	}
	// Undelete within the window brings it back; after the purge it cannot.
	if err := s.Undelete(si, stranger.member); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a stranger's undelete: %v", err)
	}
	if err := s.Undelete(si, o.member); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Has(si); p.Tombstoned {
		t.Fatal("undelete left the tombstone")
	}
	if _, err := s.ApplyDelete(si, o.member, dh, c.t); err != nil {
		t.Fatal(err)
	}
	// The window: nothing purged before it, the shares gone after it, the
	// record kept and reported as purged by the census, repair refused,
	// the owner's upload accepted again.
	c.t = c.t.Add(TombstoneWindow - time.Hour)
	if n, err := s.PurgeTombstones(); err != nil || n != 0 {
		t.Fatalf("purge before the window: %d %v", n, err)
	}
	c.t = c.t.Add(2 * time.Hour)
	if n, err := s.PurgeTombstones(); err != nil || n != 1 {
		t.Fatalf("purge after the window: %d %v", n, err)
	}
	p, _ = s.Has(si)
	if p == nil || !p.Tombstoned || !p.Purged || len(p.Complete) != 0 {
		t.Fatalf("after the purge: %+v", p)
	}
	d, _ := s.Census(0)
	if len(d.Added) != 0 || len(d.Tombstones) != 1 || !d.Tombstones[0].Purged {
		t.Fatalf("census after the purge: %+v", d)
	}
	if err := s.Undelete(si, o.member); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undelete after the purge: %v", err)
	}
	if _, err := s.BeginRepair(ob.sd, 0); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("repair after the purge: %v", err)
	}
	putAll(t, s, ob, 0)
	if p, _ := s.Has(si); p.Tombstoned || len(p.Complete) != 1 {
		t.Fatalf("the owner's upload after the purge: %+v", p)
	}
	// A purged record is dropped long after.
	if _, err := s.ApplyDelete(si, o.member, dh, c.t.Add(Immunity+time.Hour)); err != nil {
		c.t = c.t.Add(Immunity + time.Hour)
		if _, err := s.ApplyDelete(si, o.member, dh, c.t); err != nil {
			t.Fatal(err)
		}
	}
	c.t = c.t.Add(TombstoneWindow + time.Hour)
	_, _ = s.PurgeTombstones()
	c.t = c.t.Add(tombstoneKeep + time.Hour)
	_, _ = s.PurgeTombstones()
	if p, _ := s.Has(si); p != nil {
		t.Fatalf("the record outlived its keep: %+v", p)
	}
}

func TestListOwnerPages(t *testing.T) {
	s, c := newStore(t)
	o := newOwner(t, 1)
	stranger := newOwner(t, 2)
	var sis []object.StorageIndex
	for i := 0; i < 5; i++ {
		ob := encode(t, o, data(100+i, byte(i)))
		putAll(t, s, ob, 0)
		sis = append(sis, ob.sd.Descriptor.StorageIndex)
	}
	putAll(t, s, encode(t, stranger, data(100, 9)), 0)
	dh, _ := encode(t, o, data(100, 0)).sd.Descriptor.Hash()
	c.t = c.t.Add(Immunity + time.Hour)
	if _, err := s.ApplyDelete(sis[0], o.member, dh, c.t); err != nil {
		t.Fatal(err)
	}
	var all []Listed
	var after object.StorageIndex
	pages := 0
	for {
		page, more, err := s.ListOwner(o.member, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		all = append(all, page...)
		if !more {
			break
		}
		after = page[len(page)-1].StorageIndex
	}
	if len(all) != 5 || pages != 3 {
		t.Fatalf("listed %d in %d pages", len(all), pages)
	}
	tomb := 0
	for i, l := range all {
		if i > 0 && bytes.Compare(all[i-1].StorageIndex[:], l.StorageIndex[:]) >= 0 {
			t.Fatal("not in storage index order")
		}
		if l.Tombstoned {
			tomb++
		}
		if l.LastTouched.IsZero() {
			t.Fatal("no last touch")
		}
	}
	if tomb != 1 {
		t.Fatalf("%d tombstoned", tomb)
	}
}

func TestMembershipGrace(t *testing.T) {
	s, c := newStore(t)
	alice, bob := newOwner(t, 1), newOwner(t, 2)
	a := encode(t, alice, data(500, 1))
	b := encode(t, bob, data(500, 2))
	putAll(t, s, a, 0)
	putAll(t, s, b, 0)
	if err := s.PutRoot(RootRecord{Member: bob.member, Device: bob.dev.ID(), Version: 1, Envelope: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	active := map[identity.Key]bool{alice.member: true, bob.member: true}
	status := func(m identity.Key) bool { return active[m] }
	grace := 30 * 24 * time.Hour
	if purged, err := s.ApplyMembership(status, grace); err != nil || len(purged) != 0 {
		t.Fatalf("all active: %v %v", purged, err)
	}
	// Bob leaves: noted, nothing purged until the grace has run.
	active[bob.member] = false
	if purged, _ := s.ApplyMembership(status, grace); len(purged) != 0 {
		t.Fatal("purged at once")
	}
	c.t = c.t.Add(grace - time.Hour)
	if purged, _ := s.ApplyMembership(status, grace); len(purged) != 0 {
		t.Fatal("purged before the grace")
	}
	// Bob comes back: the note goes, and leaving again starts afresh.
	active[bob.member] = true
	_, _ = s.ApplyMembership(status, grace)
	active[bob.member] = false
	c.t = c.t.Add(2 * time.Hour)
	if purged, _ := s.ApplyMembership(status, grace); len(purged) != 0 {
		t.Fatal("a reverted leave counted the old clock")
	}
	c.t = c.t.Add(grace + time.Hour)
	purged, err := s.ApplyMembership(status, grace)
	if err != nil || len(purged) != 1 || purged[0] != bob.member {
		t.Fatalf("after the grace: %v %v", purged, err)
	}
	if p, _ := s.Has(b.sd.Descriptor.StorageIndex); p != nil {
		t.Fatalf("bob's share stayed: %+v", p)
	}
	if roots, _ := s.Roots(bob.member); len(roots) != 0 {
		t.Fatal("bob's root stayed")
	}
	if p, _ := s.Has(a.sd.Descriptor.StorageIndex); p == nil || len(p.Complete) != 1 {
		t.Fatal("alice's share went")
	}
	d, _ := s.Census(0)
	if len(d.Added) != 1 {
		t.Fatalf("census after the purge: %d shares", len(d.Added))
	}
}

func TestUsageExcludesTheEncodingBeingRetired(t *testing.T) {
	s, _ := newStore(t)
	o := newOwner(t, 1)
	putAll(t, s, encode(t, o, data(9000, 7)), 0) // 2-of-4, as small says
	wide := object.Params{K: 2, N: 3, SegmentSize: 4096, Cipher: object.CipherAESGCM}
	r := bytes.NewReader(data(9000, 8))
	h, _ := object.HashSpool(r, 9000)
	e, err := object.Encode(r, 9000, h, wide, o.conv, o.member)
	if err != nil {
		t.Fatal(err)
	}
	sd, _ := object.Sign(e.Descriptor, o.dev, o.cert)
	blocks := map[int][]byte{}
	_ = e.Shares(context.Background(), func(share, seg int, b []byte) error {
		if share == 0 {
			blocks[seg] = append([]byte(nil), b...)
		}
		return nil
	})
	p, err := s.BeginPut(sd, 0)
	if err != nil {
		t.Fatal(err)
	}
	for seg := 0; seg < p.Segments(); seg++ {
		if err := p.WriteBlock(seg, blocks[seg]); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Usage(o.member)
	without, _ := s.UsageExcluding(o.member, 2, 4)
	other, _ := s.UsageExcluding(o.member, 3, 5)
	if without <= 0 || without >= all || other != all {
		t.Fatalf("usage %d, excluding 2-of-4 %d, excluding an absent encoding %d", all, without, other)
	}
}
