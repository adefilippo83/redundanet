package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// owner is a member with one certified device, from fixed seeds.
type owner struct {
	seed   identity.Seed
	member identity.Key
	conv   [32]byte
	dev    *identity.DeviceKey
	cert   *identity.SignedCertificate
}

func newOwner(t *testing.T, seedByte, deviceByte byte) *owner {
	t.Helper()
	var s identity.Seed
	s[0] = seedByte
	devSeed := make([]byte, 32)
	devSeed[0] = deviceByte
	dev, err := identity.DeviceKeyFromSeed(devSeed)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := identity.Issue(s, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: s.MemberKey(), Roles: []string{identity.RoleClient},
		IssuedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &owner{seed: s, member: s.MemberKey(), conv: ConvergenceSecret(s.Secret()), dev: dev, cert: cert}
}

func pattern(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+3) ^ salt
	}
	return b
}

// encoded is an object with every block in memory, as tests need it.
type encoded struct {
	*Encoded
	cap    ReadCap
	blocks [][][]byte // [share][segment]
	signed *SignedDescriptor
}

func encodeAll(t *testing.T, o *owner, data []byte, p Params) *encoded {
	t.Helper()
	spool := bytes.NewReader(data)
	h, err := HashSpool(spool, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := Encode(spool, int64(len(data)), h, p, o.conv, o.member)
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([][][]byte, p.N)
	for i := range blocks {
		blocks[i] = make([][]byte, e.Geometry().Segments())
	}
	if err := e.Shares(context.Background(), func(share, segment int, b []byte) error {
		blocks[share][segment] = append([]byte(nil), b...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rc, err := e.ReadCap()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := Sign(e.Descriptor, o.dev, o.cert)
	if err != nil {
		t.Fatal(err)
	}
	return &encoded{Encoded: e, cap: rc, blocks: blocks, signed: sd}
}

func (e *encoded) sources(shares ...int) []Source {
	var out []Source
	for _, i := range shares {
		i := i
		out = append(out, Source{Share: i, Hashes: e.BlockHashes[i], Read: func(_ context.Context, s int) ([]byte, error) { return e.blocks[i][s], nil }})
	}
	return out
}

func decode(t *testing.T, e *encoded, conv *[32]byte, shares ...int) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := Decode(context.Background(), &out, e.cap, &e.Descriptor, conv, e.sources(shares...))
	return out.Bytes(), err
}

func TestRoundTrip(t *testing.T) {
	o := newOwner(t, 1, 11)
	sizes := []int{0, 1, 15, 16, 255, 256, 4095, 65536, DefaultSegmentSize - 1, DefaultSegmentSize, DefaultSegmentSize + 1, 3*DefaultSegmentSize + 12345}
	params := []Params{DefaultParams(1, 2), DefaultParams(2, 4), DefaultParams(3, 6), DefaultParams(2, 3), {K: 2, N: 4, SegmentSize: 4096, Cipher: CipherAESGCM}}
	for _, p := range params {
		for _, n := range sizes {
			data := pattern(n, 0)
			e := encodeAll(t, o, data, p)
			// Every block has the segment's block size; every share the same count.
			g := e.Geometry()
			for i := range e.blocks {
				for s, b := range e.blocks[i] {
					if len(b) != g.BlockSize(s) {
						t.Fatalf("k=%d n=%d size=%d: block %d/%d has %d bytes, geometry says %d", p.K, p.N, n, i, s, len(b), g.BlockSize(s))
					}
				}
			}
			// The first k shares decode without reconstruction; the last k need it.
			first := make([]int, p.K)
			last := make([]int, p.K)
			for i := range first {
				first[i], last[i] = i, int(p.N)-1-i
			}
			for _, set := range [][]int{first, last} {
				got, err := decode(t, e, &o.conv, set...)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("k=%d n=%d size=%d shares %v: %v (got %d bytes)", p.K, p.N, n, set, err, len(got))
				}
			}
			if e.cap.Size != uint64(n) || e.Descriptor.Size != uint64(n) || e.Descriptor.StorageIndex != e.Key.StorageIndex() {
				t.Fatalf("descriptor for size %d: %+v", n, e.Descriptor)
			}
		}
	}
}

func TestAnyKOfNShares(t *testing.T) {
	o := newOwner(t, 1, 11)
	data := pattern(3*DefaultSegmentSize+777, 1)
	e := encodeAll(t, o, data, DefaultParams(2, 4))
	for a := 0; a < 4; a++ {
		for b := a + 1; b < 4; b++ {
			got, err := decode(t, e, &o.conv, a, b)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("shares %d,%d: %v", a, b, err)
			}
		}
	}
	if _, err := decode(t, e, &o.conv, 3); !errors.Is(err, ErrNotEnoughShares) {
		t.Fatalf("one share: %v", err)
	}
	if _, err := decode(t, e, &o.conv, 1, 1); !errors.Is(err, ErrNotEnoughShares) {
		t.Fatalf("the same share twice: %v", err)
	}
}

func TestCorruptionIsCaught(t *testing.T) {
	o := newOwner(t, 1, 11)
	data := pattern(2*DefaultSegmentSize+10, 2)
	e := encodeAll(t, o, data, DefaultParams(2, 4))
	for share := 0; share < 4; share++ {
		for seg := 0; seg < 3; seg++ {
			bad := *e
			bad.blocks = make([][][]byte, 4)
			for i := range bad.blocks {
				bad.blocks[i] = append([][]byte(nil), e.blocks[i]...)
			}
			b := append([]byte(nil), e.blocks[share][seg]...)
			b[len(b)/2] ^= 0x01
			bad.blocks[share][seg] = b
			// With only k shares the corruption fails the read.
			other := (share + 1) % 4
			if _, err := decode(t, &bad, &o.conv, share, other); !errors.Is(err, ErrNotEnoughShares) && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("share %d segment %d corrupt, k shares: %v", share, seg, err)
			}
			// With a spare the read succeeds from the good ones.
			got, err := decode(t, &bad, &o.conv, share, other, (share+2)%4)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("share %d segment %d corrupt, with a spare: %v", share, seg, err)
			}
		}
	}
	// A wrong hash list is refused before any block is read.
	bad := e.sources(0, 1)
	bad[0].Hashes = append([]Hash(nil), bad[0].Hashes...)
	bad[0].Hashes[0][0] ^= 1
	var out bytes.Buffer
	if err := Decode(context.Background(), &out, e.cap, &e.Descriptor, &o.conv, bad); !errors.Is(err, ErrNotEnoughShares) {
		t.Fatalf("tampered hash list: %v", err)
	}
}

func TestDescriptorMismatch(t *testing.T) {
	o := newOwner(t, 1, 11)
	e := encodeAll(t, o, pattern(1000, 3), DefaultParams(2, 4))
	d := e.Descriptor
	d.Size++
	var out bytes.Buffer
	if err := Decode(context.Background(), &out, e.cap, &d, &o.conv, e.sources(0, 1)); !errors.Is(err, ErrMismatch) {
		t.Fatalf("tampered descriptor: %v", err)
	}
	rc := e.cap
	rc.Key[0] ^= 1
	if err := Decode(context.Background(), &out, rc, &e.Descriptor, &o.conv, e.sources(0, 1)); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong key: %v", err)
	}
	// Another member's convergence secret cannot have produced this key.
	other := newOwner(t, 2, 22)
	if err := Decode(context.Background(), &out, e.cap, &e.Descriptor, &other.conv, e.sources(0, 1)); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("foreign convergence secret: %v", err)
	}
}

func TestConvergence(t *testing.T) {
	o := newOwner(t, 1, 11)
	data := pattern(DefaultSegmentSize+5, 4)
	a := encodeAll(t, o, data, DefaultParams(2, 4))
	b := encodeAll(t, o, data, DefaultParams(2, 4))
	if a.cap != b.cap {
		t.Fatal("the same file twice by the same member must be the same object")
	}
	// A second device of the member: the same descriptor hash, a different signature, both verify.
	dev2 := newOwner(t, 1, 12)
	sd2, err := Sign(a.Descriptor, dev2.dev, dev2.cert)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sd2.Signature, a.signed.Signature) {
		t.Fatal("two devices gave one signature")
	}
	for _, sd := range []*SignedDescriptor{a.signed, sd2} {
		if err := sd.Verify("testnet"); err != nil {
			t.Fatal(err)
		}
	}
	// Another member, another encoding: different objects.
	other := newOwner(t, 2, 22)
	c := encodeAll(t, other, data, DefaultParams(2, 4))
	d := encodeAll(t, o, data, DefaultParams(3, 6))
	if c.Key == a.Key || c.Key.StorageIndex() == a.Key.StorageIndex() || d.Key == a.Key {
		t.Fatal("another member or encoding must give another object")
	}
}

func TestChangedSpool(t *testing.T) {
	o := newOwner(t, 1, 11)
	data := pattern(5000, 5)
	spool := bytes.NewReader(data)
	wrong, _ := HashSpool(bytes.NewReader(pattern(5000, 6)), 5000)
	if _, err := Encode(spool, 5000, wrong, DefaultParams(2, 4), o.conv, o.member); !errors.Is(err, ErrChanged) {
		t.Fatalf("a stale plaintext hash: %v", err)
	}
	h, _ := HashSpool(spool, 5000)
	e, err := Encode(spool, 5000, h, DefaultParams(2, 4), o.conv, o.member)
	if err != nil {
		t.Fatal(err)
	}
	// The spool changes between the passes.
	e.spool = bytes.NewReader(pattern(5000, 7))
	if err := e.Shares(context.Background(), func(int, int, []byte) error { return nil }); !errors.Is(err, ErrChanged) {
		t.Fatalf("a changed spool: %v", err)
	}
}

func TestSignedDescriptor(t *testing.T) {
	o := newOwner(t, 1, 11)
	e := encodeAll(t, o, pattern(100, 8), DefaultParams(2, 4))
	if err := e.signed.Verify("othernet"); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("wrong network: %v", err)
	}
	bad := *e.signed
	bad.Signature = append([]byte(nil), bad.Signature...)
	bad.Signature[0] ^= 1
	if err := bad.Verify("testnet"); err == nil {
		t.Fatal("a tampered signature verified")
	}
	tampered := *e.signed
	tampered.Descriptor.Size++
	if err := tampered.Verify("testnet"); err == nil {
		t.Fatal("a tampered descriptor verified")
	}
	other := newOwner(t, 2, 22)
	if _, err := Sign(e.Descriptor, other.dev, other.cert); err == nil {
		t.Fatal("another member's device signed the descriptor")
	}
	// The record round-trips through its encoding and the hash ignores the signature.
	enc, _ := e.signed.Encode()
	back, err := DecodeSignedDescriptor(enc)
	if err != nil || back.Verify("testnet") != nil {
		t.Fatalf("encode/decode: %v", err)
	}
	h1, _ := back.Descriptor.Hash()
	if h1 != e.cap.DescriptorHash {
		t.Fatal("descriptor hash changed through encoding")
	}
}

func TestCapabilities(t *testing.T) {
	o := newOwner(t, 1, 11)
	e := encodeAll(t, o, pattern(12, 9), DefaultParams(2, 4))
	s := e.cap.String()
	if !strings.HasPrefix(s, "rn:r1:") || !strings.HasSuffix(s, ":12") {
		t.Fatalf("read cap: %s", s)
	}
	back, err := ParseReadCap(s)
	if err != nil || back != e.cap {
		t.Fatalf("parse read cap: %v", err)
	}
	v := e.cap.Verify()
	vs := v.String()
	vb, err := ParseVerifyCap(vs)
	if err != nil || vb != v || !strings.HasPrefix(vs, "rn:v1:") || v.StorageIndex != e.Descriptor.StorageIndex {
		t.Fatalf("verify cap: %v %s", err, vs)
	}
	for _, bad := range []string{"", "rn:r2:a:b:1", s[:len(s)-1] + "x", "rn:r1:abc:def:1", strings.Replace(s, ":12", ":x", 1)} {
		if _, err := ParseReadCap(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := ParseVerifyCap(s); err == nil {
		t.Fatal("a read cap parsed as a verify cap")
	}
}

func TestShareStream(t *testing.T) {
	o := newOwner(t, 1, 11)
	data := pattern(2*DefaultSegmentSize+99, 10)
	e := encodeAll(t, o, data, DefaultParams(2, 4))
	g := e.Geometry()
	var streams [4][]byte
	for i := 0; i < 4; i++ {
		var buf bytes.Buffer
		hdrLen, err := WriteShareHeader(&buf, e.signed, uint8(i))
		if err != nil {
			t.Fatal(err)
		}
		for s := range e.blocks[i] {
			if int64(buf.Len()) != BlockOffset(g, hdrLen, s) {
				t.Fatalf("block %d offset %d, stream at %d", s, BlockOffset(g, hdrLen, s), buf.Len())
			}
			buf.Write(e.blocks[i][s])
		}
		if err := WriteShareTrailer(&buf, e.BlockHashes[i]); err != nil {
			t.Fatal(err)
		}
		if int64(buf.Len()) != ShareSize(g, hdrLen) {
			t.Fatalf("share %d is %d bytes, ShareSize says %d", i, buf.Len(), ShareSize(g, hdrLen))
		}
		streams[i] = buf.Bytes()
	}
	// Read the streams back and decode from two of them.
	var sources []Source
	for _, i := range []int{1, 3} {
		r := bytes.NewReader(streams[i])
		hdr, err := ReadShareHeader(r)
		if err != nil || hdr.Number != uint8(i) || hdr.Descriptor.Verify("testnet") != nil {
			t.Fatalf("header of share %d: %v", i, err)
		}
		blocks := make([][]byte, g.Segments())
		for s := range blocks {
			blocks[s] = make([]byte, g.BlockSize(s))
			if _, err := r.Read(blocks[s]); err != nil {
				t.Fatal(err)
			}
		}
		hashes, err := ReadShareTrailer(r, g.Segments())
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, Source{Share: i, Hashes: hashes, Read: func(_ context.Context, s int) ([]byte, error) { return blocks[s], nil }})
	}
	var out bytes.Buffer
	if err := Decode(context.Background(), &out, e.cap, &e.signed.Descriptor, &o.conv, sources); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("decode from streams: %v", err)
	}
	if _, err := ReadShareHeader(bytes.NewReader([]byte("nope"))); err == nil {
		t.Fatal("garbage parsed as a share")
	}
}

// TestKnownAnswers pins the format: these values must never change for
// version 1. They were produced by this implementation on 2026-11-02 and
// cross-checked by the round-trip tests; a change here is a format change.
func TestKnownAnswers(t *testing.T) {
	o := newOwner(t, 1, 11)
	data := pattern(3*DefaultSegmentSize+123, 0)
	e := encodeAll(t, o, data, DefaultParams(2, 4))
	got := map[string]string{
		"member":          o.member.String(),
		"plaintext_hash":  e.PlaintextHash.String(),
		"key":             enc32(e.Key[:]),
		"storage_index":   e.Key.StorageIndex().String(),
		"ciphertext_hash": e.Descriptor.CiphertextHash.String(),
		"share_root_0":    e.Descriptor.ShareRoots[0].String(),
		"share_root_3":    e.Descriptor.ShareRoots[3].String(),
		"block_0_0_hash":  e.BlockHashes[0][0].String(),
		"descriptor_hash": e.cap.DescriptorHash.String(),
		"read_cap":        e.cap.String(),
	}
	for k, v := range knownAnswers {
		if got[k] != v {
			t.Errorf("%s: got %s, want %s", k, got[k], v)
		}
	}
	if len(e.blocks[0][0]) != (DefaultSegmentSize+16+1)/2 || e.Geometry().Segments() != 4 {
		t.Fatalf("geometry: %d segments, block 0 of %d bytes", e.Geometry().Segments(), len(e.blocks[0][0]))
	}
}

var knownAnswers = map[string]string{
	"member":          "k7befm2kl4rlvlkuerza2pkeyxfaqkhif7k32s4zgqmi7y4zkt4q",
	"plaintext_hash":  "yfpjtkvmkg2yni2t52u22hsupmb4zaweb5uk767tlafoykjnyoea",
	"key":             "3cqut4guchfuwwoov2vc7djrxlw2ipwugnv7htmskltctnce6l3q",
	"storage_index":   "szrw6dq6epkoxbw5xzlayv7zt4",
	"ciphertext_hash": "vqolnauyd54plruhidjktpxytqir65kddpsmwx7pz3w3ienflwia",
	"share_root_0":    "mn5wcpps66yoa7jqtbwqaiesnwv2iqmljg2uhk2tm2b2jni4kbea",
	"share_root_3":    "6nohh3xjtek5upt2vvz5nkb654tqcf7rso62zpgchiba3qvxebka",
	"block_0_0_hash":  "67eyjam6qpgcx2mm6uvh4sxjsnbkx7hdxkns6sddvndtuvnh652a",
	"descriptor_hash": "mq2iogrf67g4vxpsyvwyc3zcy7kvxit6hkcetrk4ft6lbmeccznq",
	"read_cap":        "rn:r1:3cqut4guchfuwwoov2vc7djrxlw2ipwugnv7htmskltctnce6l3q:mq2iogrf67g4vxpsyvwyc3zcy7kvxit6hkcetrk4ft6lbmeccznq:3145851",
}

func BenchmarkEncode(b *testing.B) {
	o := benchOwner(b)
	data := pattern(64<<20, 0)
	h, _ := HashSpool(bytes.NewReader(data), int64(len(data)))
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := Encode(bytes.NewReader(data), int64(len(data)), h, DefaultParams(2, 4), o.conv, o.member)
		if err != nil {
			b.Fatal(err)
		}
		if err := e.Shares(context.Background(), func(int, int, []byte) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	o := benchOwner(b)
	data := pattern(64<<20, 0)
	h, _ := HashSpool(bytes.NewReader(data), int64(len(data)))
	e, err := Encode(bytes.NewReader(data), int64(len(data)), h, DefaultParams(2, 4), o.conv, o.member)
	if err != nil {
		b.Fatal(err)
	}
	blocks := make([][][]byte, 4)
	for i := range blocks {
		blocks[i] = make([][]byte, e.Geometry().Segments())
	}
	_ = e.Shares(context.Background(), func(share, seg int, blk []byte) error { blocks[share][seg] = append([]byte(nil), blk...); return nil })
	rc, _ := e.ReadCap()
	srcs := []Source{
		{Share: 1, Hashes: e.BlockHashes[1], Read: func(_ context.Context, s int) ([]byte, error) { return blocks[1][s], nil }},
		{Share: 3, Hashes: e.BlockHashes[3], Read: func(_ context.Context, s int) ([]byte, error) { return blocks[3][s], nil }},
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sink countWriter
		if err := Decode(context.Background(), &sink, rc, &e.Descriptor, &o.conv, srcs); err != nil {
			b.Fatal(err)
		}
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func benchOwner(b *testing.B) *owner {
	b.Helper()
	var s identity.Seed
	s[0] = 1
	devSeed := make([]byte, 32)
	devSeed[0] = 11
	dev, _ := identity.DeviceKeyFromSeed(devSeed)
	cert, err := identity.Issue(s, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: s.MemberKey(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		b.Fatal(err)
	}
	return &owner{seed: s, member: s.MemberKey(), conv: ConvergenceSecret(s.Secret()), dev: dev, cert: cert}
}

var _ = fmt.Sprintf
