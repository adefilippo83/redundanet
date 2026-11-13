package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
)

// squatOn stores, on every server, every share of an object of the
// squatter's own under the storage index that the victim's bytes will
// get: what a storage member that saw the index announced can do before
// the victim's first share arrives.
func squatOn(t *testing.T, g *grid, squatter, victim *member, victimData []byte) object.StorageIndex {
	t.Helper()
	p := object.DefaultParams(g.m.Network.Encoding.K, g.m.Network.Encoding.N)
	si := object.DeriveKey(object.ConvergenceSecret(victim.seed.Secret()), object.Sum(victimData), p).StorageIndex()
	junk := pattern(700, 99)
	enc, err := object.Encode(bytes.NewReader(junk), int64(len(junk)), object.Sum(junk), p, object.ConvergenceSecret(squatter.seed.Secret()), squatter.key)
	if err != nil {
		t.Fatal(err)
	}
	d := enc.Descriptor
	d.StorageIndex = si
	sd, err := object.Sign(d, squatter.dev, squatter.cert)
	if err != nil {
		t.Fatal(err)
	}
	blocks := map[int]map[int][]byte{}
	_ = enc.Shares(context.Background(), func(share, seg int, b []byte) error {
		if blocks[share] == nil {
			blocks[share] = map[int][]byte{}
		}
		blocks[share][seg] = append([]byte(nil), b...)
		return nil
	})
	for _, node := range g.servers {
		for n := 0; n < int(p.N); n++ {
			put, err := node.store.BeginPut(sd, uint8(n))
			if err != nil {
				t.Fatal(err)
			}
			for seg := 0; seg < put.Segments(); seg++ {
				if err := put.WriteBlock(seg, blocks[n][seg]); err != nil {
					t.Fatal(err)
				}
			}
			if err := put.Complete(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return si
}

// The weakness as it stands until every server accepts a second owner: a
// member that registers a descriptor under an index first holds up the
// owner's upload, loudly. The backup reports the file as not stored; it
// never takes the other member's shares for its own.
func TestASquattedIndexHoldsUpTheUploadUntilServersAcceptSecondOwners(t *testing.T) {
	g, alice, bob := newGrid(t)
	e, _ := g.engineFor(t, alice)
	ctx := context.Background()
	data := pattern(object.DefaultSegmentSize+33, 4)
	squatOn(t, g, bob, alice, data)
	if _, ok, err := e.Has(ctx, object.Sum(data), int64(len(data))); err != nil || ok {
		t.Fatalf("another member's shares counted as the upload: %v %v", ok, err)
	}
	if _, err := e.Put(ctx, bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrNotEnoughServers) {
		t.Fatalf("put under a squatted index: %v", err)
	}
}

// The attack against servers that accept a second owner: the owner's
// upload, restore, check, listing and delete all go through, and the
// squatter's object is untouched by them and charged to the squatter.
func TestASquattedIndexDoesNotHoldUpTheOwner(t *testing.T) {
	g, alice, bob := newGrid(t)
	for _, node := range g.servers {
		if err := node.store.AcceptSecondOwners(); err != nil {
			t.Fatal(err)
		}
	}
	e, _ := g.engineFor(t, alice)
	ctx := context.Background()
	data := pattern(object.DefaultSegmentSize+33, 4)
	si := squatOn(t, g, bob, alice, data)
	usage := map[*serverNode]int64{}
	for _, node := range g.servers {
		usage[node], _ = node.store.Usage(bob.key)
		if u, _ := node.store.Usage(alice.key); u != 0 {
			t.Fatalf("alice is charged %d bytes for the squatter's shares", u)
		}
	}
	if _, ok, _ := e.Has(ctx, object.Sum(data), int64(len(data))); ok {
		t.Fatal("another member's shares counted as the upload")
	}
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("put under a squatted index: %v", err)
	}
	if rc.Key.StorageIndex() != si {
		t.Fatal("the test squatted on another index")
	}
	h, err := e.Check(ctx, rc)
	if err != nil || !h.Placed || h.Servers != 2 || len(h.Shares) != 4 {
		t.Fatalf("check: %+v %v", h, err)
	}
	var out bytes.Buffer
	if err := e.Get(ctx, rc, &out, nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("restore: %v", err)
	}
	// A second device of the owner finds the object stored.
	other, _ := g.engineFor(t, alice)
	if got, ok, err := other.Has(ctx, object.Sum(data), int64(len(data))); err != nil || !ok || got != rc {
		t.Fatalf("has on a second device: %v %v", ok, err)
	}
	l, err := e.ListObjects(ctx)
	if err != nil || len(l.Objects) != 1 || l.Objects[si] == nil || l.Objects[si].DescriptorHash != rc.DescriptorHash || len(l.Objects[si].Holders) != 2 {
		t.Fatalf("list: %+v %v", l, err)
	}
	// The owner's repair after a loss, with the squatter's shares of the
	// same numbers still there.
	for _, node := range g.servers {
		p, _ := node.store.Has(alice.key, si)
		if err := node.store.Remove(alice.key, si, p.Complete[0], "test"); err != nil {
			t.Fatal(err)
		}
		break
	}
	if h, err = e.Repair(ctx, rc); err != nil || !h.Placed {
		t.Fatalf("repair: %+v %v", h, err)
	}
	// Delete: the owner's object is tombstoned on both servers, the
	// squatter's is as it was.
	g.advance(store.Immunity + 24*time.Hour)
	res, err := e.DeleteByIndex(ctx, si, rc.DescriptorHash)
	if err != nil || res.Tombstoned != 2 {
		t.Fatalf("delete: %+v %v", res, err)
	}
	for _, node := range g.servers {
		p, _ := node.store.Has(bob.key, si)
		if p == nil || p.Tombstoned || len(p.Complete) != 4 {
			t.Fatalf("the squatter's object after the owner's delete: %+v", p)
		}
		if u, _ := node.store.Usage(bob.key); u != usage[node] {
			t.Fatalf("the squatter's usage moved from %d to %d", usage[node], u)
		}
		if p, _ := node.store.Has(alice.key, si); p == nil || !p.Tombstoned {
			t.Fatalf("the owner's object after its delete: %+v", p)
		}
	}
}
