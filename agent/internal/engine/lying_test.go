package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// replayed is a stream whose first message was read already and is handed
// to the real server again.
type replayed struct {
	wire.Stream
	head io.Reader
}

func (r *replayed) Read(p []byte) (int, error) { return r.head.Read(p) }

// handle is the server's stream handler in the test grid. While lie is
// set, a has request is answered with what lie returns and never reaches
// the store; everything else is served honestly.
func (n *serverNode) handle(peer identity.Peer, proto string, st wire.Stream) {
	n.lieMu.Lock()
	lie := n.lie
	n.lieMu.Unlock()
	if lie == nil {
		n.srv.Handle(peer, proto, st)
		return
	}
	var hdr [4]byte
	if _, err := io.ReadFull(st, hdr[:]); err != nil {
		_ = st.Close()
		return
	}
	body := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(st, body); err != nil {
		_ = st.Close()
		return
	}
	var req shareproto.Request
	if err := identity.UnmarshalLenient(body, &req); err != nil || req.Type != shareproto.TypeHas {
		n.srv.Handle(peer, proto, &replayed{Stream: st, head: io.MultiReader(bytes.NewReader(hdr[:]), bytes.NewReader(body), st)})
		return
	}
	defer st.Close()
	defer st.CancelRead(0)
	out, _ := identity.Marshal(shareproto.Response{Status: shareproto.StatusOK, Presence: lie(req.StorageIndex)})
	buf := make([]byte, 4+len(out))
	binary.BigEndian.PutUint32(buf, uint32(len(out)))
	copy(buf[4:], out)
	_, _ = st.Write(buf)
}

func (n *serverNode) lying(f func(object.StorageIndex) *shareproto.Presence) {
	n.lieMu.Lock()
	n.lie = f
	n.lieMu.Unlock()
}

// A storage server that says it holds every share of whatever it is asked
// about must not make the owner skip an upload: nothing was ever stored,
// and the capability built from its answer could not be read by anyone.
func TestHasDoesNotTakeAServersWord(t *testing.T) {
	g, alice, bob := newGrid(t)
	e, _ := g.engineFor(t, alice)
	ctx := context.Background()
	var invented object.Hash
	invented[0], invented[31] = 0xbe, 0xef
	g.servers[bob.dev.ID()].lying(func(object.StorageIndex) *shareproto.Presence {
		return &shareproto.Presence{DescriptorHash: invented, Owner: alice.key, Complete: []uint8{0, 1, 2, 3}}
	})
	data := pattern(object.DefaultSegmentSize+11, 7)
	if _, ok, err := e.Has(ctx, object.Sum(data), int64(len(data))); err != nil || ok {
		t.Fatalf("has believed a server about an object that was never stored: %v %v", ok, err)
	}
	// The upload then goes ahead and the object reads back.
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.Get(ctx, rc, &out, nil); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("get: %v", err)
	}
	// Another device of the member, with an empty local database, finds
	// the object under its real descriptor however the liar answers.
	for i := 0; i < 4; i++ {
		other, _ := g.engineFor(t, alice)
		got, ok, err := other.Has(ctx, object.Sum(data), int64(len(data)))
		if err != nil || !ok || got != rc {
			t.Fatalf("has on a second device: %v %v, capability %v", ok, err, got == rc)
		}
	}
}

// A server that once held a share knows the object's real descriptor. Its
// word still counts for no more shares than placement would have given
// it, so the shares the other server lost are sent again.
func TestAServerVouchesForItsPlacementOnly(t *testing.T) {
	g, alice, bob := newGrid(t)
	e, _ := g.engineFor(t, alice)
	ctx := context.Background()
	data := pattern(object.DefaultSegmentSize+13, 8)
	rc, err := e.Put(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	si := rc.Key.StorageIndex()
	honest := g.servers[alice.dev.ID()].store
	before, err := honest.Has(si)
	if err != nil || before == nil || len(before.Complete) != 2 {
		t.Fatalf("placement before: %+v %v", before, err)
	}
	for _, n := range before.Complete {
		if err := honest.Remove(si, n, "test"); err != nil {
			t.Fatal(err)
		}
	}
	g.servers[bob.dev.ID()].lying(func(object.StorageIndex) *shareproto.Presence {
		return &shareproto.Presence{DescriptorHash: rc.DescriptorHash, Owner: alice.key, Complete: []uint8{0, 1, 2, 3}}
	})
	other, _ := g.engineFor(t, alice)
	if again, err := other.Put(ctx, bytes.NewReader(data), int64(len(data))); err != nil || again != rc {
		t.Fatalf("put on a second device: %v", err)
	}
	after, err := honest.Has(si)
	if err != nil || after == nil || len(after.Complete) != 2 {
		t.Fatalf("the shares the honest server lost were not sent again: %+v %v", after, err)
	}
}

func TestVouched(t *testing.T) {
	var a, b, c identity.Key
	a[0], b[0], c[0] = 1, 2, 3
	for _, tc := range []struct {
		name    string
		n       int
		servers int
		holders map[identity.Key][]uint8
		want    int
	}{
		{"one server claims all of 2-of-3 on three servers", 3, 3, map[identity.Key][]uint8{a: {0, 1, 2}}, 1},
		{"one share on each of three servers", 3, 3, map[identity.Key][]uint8{a: {0}, b: {1}, c: {2}}, 3},
		{"two each of four on two servers", 4, 2, map[identity.Key][]uint8{a: {0, 1}, b: {2, 3}}, 4},
		{"a liar beside an honest holder", 4, 2, map[identity.Key][]uint8{a: {0, 1, 2, 3}, b: {2, 3}}, 4},
		{"the same numbers twice", 3, 3, map[identity.Key][]uint8{a: {0}, b: {0}}, 1},
		{"a single server network", 3, 1, map[identity.Key][]uint8{a: {0, 1, 2}}, 3},
		{"nothing held", 3, 3, map[identity.Key][]uint8{}, 0},
	} {
		if got := vouched(&Health{N: tc.n, Holders: tc.holders}, tc.servers); len(got) != tc.want {
			t.Errorf("%s: %d share numbers vouched for, want %d (%v)", tc.name, len(got), tc.want, got)
		}
	}
}
