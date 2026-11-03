package object

import (
	"bytes"
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
	"github.com/zeebo/blake3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Errors a caller tells apart.
var (
	ErrChanged         = errors.New("object: the spool changed since it was hashed")
	ErrCorrupt         = errors.New("object: a block does not match its signed hash")
	ErrNotEnoughShares = errors.New("object: fewer than k shares are available")
	ErrMismatch        = errors.New("object: the descriptor does not match the capability")
	ErrKeyMismatch     = errors.New("object: the plaintext does not match the capability's key")
)

// HashSpool is the plaintext hash of a spool, what the scan records.
func HashSpool(spool io.ReaderAt, size int64) (Hash, error) {
	h := blake3.New()
	if _, err := io.Copy(h, io.NewSectionReader(spool, 0, size)); err != nil {
		return Hash{}, err
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// Encoded is an object computed from a spool: its key, its descriptor and
// every block hash, after one pass. Shares regenerates the blocks in a
// second pass, so memory holds one segment at a time.
type Encoded struct {
	Params        Params
	Key           Key
	PlaintextHash Hash
	Descriptor    Descriptor
	BlockHashes   [][]Hash // [share][segment]

	spool io.ReaderAt
	size  int64
	geom  Geometry
}

// Encode computes the object a spool holds. plaintextHash is what the scan
// recorded; the pass recomputes it and fails with ErrChanged on a
// difference, so an object can never sit under a key that does not match
// its bytes.
func Encode(spool io.ReaderAt, size int64, plaintextHash Hash, p Params, conv [32]byte, owner identity.Key) (*Encoded, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, errors.New("object: negative size")
	}
	if owner.IsZero() {
		return nil, errors.New("object: an owner is required")
	}
	key := DeriveKey(conv, plaintextHash, p)
	e := &Encoded{Params: p, Key: key, PlaintextHash: plaintextHash, spool: spool, size: size,
		geom: Geometry{Size: uint64(size), SegmentSize: p.SegmentSize, K: p.K}}
	gcm, err := aead(key, p.Cipher)
	if err != nil {
		return nil, err
	}
	rs, err := reedsolomon.New(int(p.K), int(p.N)-int(p.K))
	if err != nil {
		return nil, err
	}
	segments := e.geom.Segments()
	e.BlockHashes = make([][]Hash, p.N)
	for i := range e.BlockHashes {
		e.BlockHashes[i] = make([]Hash, segments)
	}
	ptHash, ctHash := blake3.New(), blake3.New()
	buf := make([]byte, int(p.SegmentSize)+tagSize)
	si := key.StorageIndex()
	for s := 0; s < segments; s++ {
		pt, ct, err := e.segment(s, buf, gcm, si)
		if err != nil {
			return nil, err
		}
		_, _ = ptHash.Write(pt)
		_, _ = ctHash.Write(ct)
		blocks, err := e.blocks(s, ct, rs)
		if err != nil {
			return nil, err
		}
		for i, b := range blocks {
			e.BlockHashes[i][s] = Sum(b)
		}
	}
	var got Hash
	copy(got[:], ptHash.Sum(nil))
	if got != plaintextHash {
		return nil, ErrChanged
	}
	e.Descriptor = Descriptor{Version: FormatVersion, StorageIndex: si, Size: uint64(size), SegmentSize: p.SegmentSize,
		K: p.K, N: p.N, Cipher: p.Cipher, Owner: owner, ShareRoots: make([]Hash, p.N)}
	copy(e.Descriptor.CiphertextHash[:], ctHash.Sum(nil))
	for i := range e.BlockHashes {
		e.Descriptor.ShareRoots[i] = ShareRoot(e.BlockHashes[i])
	}
	return e, nil
}

// segment reads and seals segment s; pt and ct alias buf.
func (e *Encoded) segment(s int, buf []byte, gcm cipher.AEAD, si StorageIndex) (pt, ct []byte, err error) {
	n := e.geom.PlaintextLen(s)
	pt = buf[:n]
	if n > 0 {
		if _, err := e.spool.ReadAt(pt, int64(s)*int64(e.geom.SegmentSize)); err != nil {
			return nil, nil, fmt.Errorf("object: spool: %w", err)
		}
	}
	// Seal in place: the ciphertext is the plaintext plus the tag, and the
	// plaintext bytes are hashed before they are overwritten.
	cp := make([]byte, n)
	copy(cp, pt)
	ct = gcm.Seal(buf[:0], nonce(uint64(s)), cp, additionalData(si, uint64(s), e.geom.Size))
	return cp, ct, nil
}

// blocks splits a segment's ciphertext into k data blocks, padded, and
// computes the n-k parity blocks.
func (e *Encoded) blocks(s int, ct []byte, rs reedsolomon.Encoder) ([][]byte, error) {
	bs := e.geom.BlockSize(s)
	shards := make([][]byte, e.Params.N)
	for i := range shards {
		shards[i] = make([]byte, bs)
	}
	for i := 0; i < int(e.Params.K); i++ {
		from := i * bs
		if from < len(ct) {
			copy(shards[i], ct[from:min(from+bs, len(ct))])
		}
	}
	if err := rs.Encode(shards); err != nil {
		return nil, err
	}
	return shards, nil
}

// Geometry is the object's size answers.
func (e *Encoded) Geometry() Geometry { return e.geom }

// ReadCap is the capability for the object, once the descriptor is known.
func (e *Encoded) ReadCap() (ReadCap, error) {
	dh, err := e.Descriptor.Hash()
	if err != nil {
		return ReadCap{}, err
	}
	return ReadCap{Key: e.Key, DescriptorHash: dh, Size: uint64(e.size)}, nil
}

// Shares regenerates every block in one pass over the spool and hands each
// to sink in segment order, share by share within a segment: what an upload
// streams to n servers at once. A block that no longer matches its hash
// from the first pass stops with ErrChanged.
func (e *Encoded) Shares(ctx context.Context, sink func(share, segment int, block []byte) error) error {
	gcm, err := aead(e.Key, e.Params.Cipher)
	if err != nil {
		return err
	}
	rs, err := reedsolomon.New(int(e.Params.K), int(e.Params.N)-int(e.Params.K))
	if err != nil {
		return err
	}
	buf := make([]byte, int(e.Params.SegmentSize)+tagSize)
	si := e.Key.StorageIndex()
	for s := 0; s < e.geom.Segments(); s++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, ct, err := e.segment(s, buf, gcm, si)
		if err != nil {
			return err
		}
		blocks, err := e.blocks(s, ct, rs)
		if err != nil {
			return err
		}
		for i, b := range blocks {
			if Sum(b) != e.BlockHashes[i][s] {
				return ErrChanged
			}
			if err := sink(i, s, b); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- decoding ----

// Source is one share a reader can draw blocks from: its number, the
// block hash list its holder serves, and a reader of one block.
type Source struct {
	Share  int
	Hashes []Hash
	Read   func(ctx context.Context, segment int) ([]byte, error)
}

// Decode rebuilds the object into w from any k of its shares, verifying
// every block against the signed share roots, the whole ciphertext against
// its hash, and, when the convergence secret is given, the plaintext
// against the key in the capability. Nothing unverified is written: a
// failure returns an error, and the caller discards what it wrote.
func Decode(ctx context.Context, w io.Writer, rc ReadCap, desc *Descriptor, conv *[32]byte, sources []Source) error {
	if err := desc.check(); err != nil {
		return err
	}
	dh, err := desc.Hash()
	if err != nil {
		return err
	}
	if dh != rc.DescriptorHash || desc.StorageIndex != rc.Key.StorageIndex() || desc.Size != rc.Size {
		return ErrMismatch
	}
	geom := desc.Geometry()
	segments := geom.Segments()
	// Sources with a verified hash list, one per share number.
	usable := map[int]Source{}
	for _, src := range sources {
		if src.Share < 0 || src.Share >= int(desc.N) || len(src.Hashes) != segments {
			continue
		}
		if ShareRoot(src.Hashes) != desc.ShareRoots[src.Share] {
			continue
		}
		if _, dup := usable[src.Share]; !dup {
			usable[src.Share] = src
		}
	}
	if len(usable) < int(desc.K) {
		return ErrNotEnoughShares
	}
	gcm, err := aead(rc.Key, desc.Cipher)
	if err != nil {
		return err
	}
	rs, err := reedsolomon.New(int(desc.K), int(desc.N)-int(desc.K))
	if err != nil {
		return err
	}
	// Order of preference: data shares first, so a healthy object needs no
	// reconstruction, then parity shares as spares.
	order := make([]int, 0, len(usable))
	for i := 0; i < int(desc.N); i++ {
		if _, ok := usable[i]; ok {
			order = append(order, i)
		}
	}
	ptHash, ctHash := blake3.New(), blake3.New()
	for s := 0; s < segments; s++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		shards := make([][]byte, desc.N)
		have := 0
		var lastErr error
		for _, i := range order {
			if have == int(desc.K) {
				break
			}
			src := usable[i]
			b, err := src.Read(ctx, s)
			if err != nil {
				lastErr = err
				continue
			}
			if len(b) != geom.BlockSize(s) || Sum(b) != src.Hashes[s] {
				lastErr = ErrCorrupt
				continue
			}
			shards[i] = b
			have++
		}
		if have < int(desc.K) {
			if lastErr != nil {
				return fmt.Errorf("%w (segment %d: %v)", ErrNotEnoughShares, s, lastErr)
			}
			return ErrNotEnoughShares
		}
		if err := rs.ReconstructData(shards); err != nil {
			return fmt.Errorf("object: reconstruct segment %d: %w", s, err)
		}
		ct := bytes.Join(shards[:desc.K], nil)[:geom.CiphertextLen(s)]
		_, _ = ctHash.Write(ct)
		pt, err := gcm.Open(nil, nonce(uint64(s)), ct, additionalData(desc.StorageIndex, uint64(s), desc.Size))
		if err != nil {
			return fmt.Errorf("%w: segment %d fails authentication", ErrCorrupt, s)
		}
		_, _ = ptHash.Write(pt)
		if _, err := w.Write(pt); err != nil {
			return err
		}
	}
	var got Hash
	copy(got[:], ctHash.Sum(nil))
	if got != desc.CiphertextHash {
		return fmt.Errorf("%w: ciphertext hash", ErrCorrupt)
	}
	if conv != nil {
		var pt Hash
		copy(pt[:], ptHash.Sum(nil))
		if DeriveKey(*conv, pt, desc.Params()) != rc.Key {
			return ErrKeyMismatch
		}
	}
	return nil
}

// DecodeRange rebuilds segments [from, to) into w, verifying every block
// against the signed share roots. A range cannot check the ciphertext
// hash or the key, which cover the whole object; Decode does both.
func DecodeRange(ctx context.Context, w io.Writer, rc ReadCap, desc *Descriptor, sources []Source, from, to int) error {
	if err := desc.check(); err != nil {
		return err
	}
	dh, err := desc.Hash()
	if err != nil {
		return err
	}
	if dh != rc.DescriptorHash || desc.StorageIndex != rc.Key.StorageIndex() || desc.Size != rc.Size {
		return ErrMismatch
	}
	geom := desc.Geometry()
	segments := geom.Segments()
	if from < 0 || to > segments || from > to {
		return fmt.Errorf("object: segments %d to %d of %d", from, to, segments)
	}
	usable := map[int]Source{}
	for _, src := range sources {
		if src.Share < 0 || src.Share >= int(desc.N) || len(src.Hashes) != segments || ShareRoot(src.Hashes) != desc.ShareRoots[src.Share] {
			continue
		}
		if _, dup := usable[src.Share]; !dup {
			usable[src.Share] = src
		}
	}
	if len(usable) < int(desc.K) {
		return ErrNotEnoughShares
	}
	gcm, err := aead(rc.Key, desc.Cipher)
	if err != nil {
		return err
	}
	rs, err := reedsolomon.New(int(desc.K), int(desc.N)-int(desc.K))
	if err != nil {
		return err
	}
	order := make([]int, 0, len(usable))
	for i := 0; i < int(desc.N); i++ {
		if _, ok := usable[i]; ok {
			order = append(order, i)
		}
	}
	for s := from; s < to; s++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		shards := make([][]byte, desc.N)
		have := 0
		for _, i := range order {
			if have == int(desc.K) {
				break
			}
			src := usable[i]
			b, err := src.Read(ctx, s)
			if err != nil || len(b) != geom.BlockSize(s) || Sum(b) != src.Hashes[s] {
				continue
			}
			shards[i] = b
			have++
		}
		if have < int(desc.K) {
			return ErrNotEnoughShares
		}
		if err := rs.ReconstructData(shards); err != nil {
			return fmt.Errorf("object: reconstruct segment %d: %w", s, err)
		}
		ct := bytes.Join(shards[:desc.K], nil)[:geom.CiphertextLen(s)]
		pt, err := gcm.Open(nil, nonce(uint64(s)), ct, additionalData(desc.StorageIndex, uint64(s), desc.Size))
		if err != nil {
			return fmt.Errorf("%w: segment %d fails authentication", ErrCorrupt, s)
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
	}
	return nil
}

// ---- regenerating shares without the key ----

// Reconstruct regenerates the wanted share numbers from any k shares. It
// needs no key: Reed-Solomon runs over the ciphertext blocks, which is
// what lets a repair worker that is not the owner do it. Every source
// block is checked against its hash list, whose root the signed descriptor
// fixes, and each regenerated share is checked against its own signed root
// once its last block is known. sink receives the blocks in segment order,
// share by share within a segment, before that final check, so a caller
// that streams them to a server must wait for Reconstruct to return
// before it lets the server mark the share complete; the server checks
// the root once more on its side. The hash lists of the wanted shares are
// returned, indexed like wanted.
func Reconstruct(ctx context.Context, desc *Descriptor, sources []Source, wanted []uint8, sink func(share uint8, segment int, block []byte) error) ([][]Hash, error) {
	if err := desc.check(); err != nil {
		return nil, err
	}
	for _, w := range wanted {
		if int(w) >= int(desc.N) {
			return nil, fmt.Errorf("object: share number %d of n=%d", w, desc.N)
		}
	}
	geom := desc.Geometry()
	segments := geom.Segments()
	usable := map[int]Source{}
	for _, src := range sources {
		if src.Share < 0 || src.Share >= int(desc.N) || len(src.Hashes) != segments || ShareRoot(src.Hashes) != desc.ShareRoots[src.Share] {
			continue
		}
		if _, dup := usable[src.Share]; !dup {
			usable[src.Share] = src
		}
	}
	if len(usable) < int(desc.K) {
		return nil, ErrNotEnoughShares
	}
	rs, err := reedsolomon.New(int(desc.K), int(desc.N)-int(desc.K))
	if err != nil {
		return nil, err
	}
	order := make([]int, 0, len(usable))
	for i := 0; i < int(desc.N); i++ {
		if _, ok := usable[i]; ok {
			order = append(order, i)
		}
	}
	hashes := make([][]Hash, len(wanted))
	for i := range hashes {
		hashes[i] = make([]Hash, segments)
	}
	for s := 0; s < segments; s++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		shards := make([][]byte, desc.N)
		have := 0
		var lastErr error
		for _, i := range order {
			if have == int(desc.K) {
				break
			}
			src := usable[i]
			b, err := src.Read(ctx, s)
			if err != nil {
				lastErr = err
				continue
			}
			if len(b) != geom.BlockSize(s) || Sum(b) != src.Hashes[s] {
				lastErr = ErrCorrupt
				continue
			}
			shards[i] = b
			have++
		}
		if have < int(desc.K) {
			if lastErr != nil {
				return nil, fmt.Errorf("%w (segment %d: %v)", ErrNotEnoughShares, s, lastErr)
			}
			return nil, ErrNotEnoughShares
		}
		if err := rs.Reconstruct(shards); err != nil {
			return nil, fmt.Errorf("object: reconstruct segment %d: %w", s, err)
		}
		for i, w := range wanted {
			b := shards[w]
			hashes[i][s] = Sum(b)
			if err := sink(w, s, b); err != nil {
				return nil, err
			}
		}
	}
	for i, w := range wanted {
		if ShareRoot(hashes[i]) != desc.ShareRoots[w] {
			return nil, fmt.Errorf("%w: regenerated share %d does not match its signed root", ErrCorrupt, w)
		}
	}
	return hashes, nil
}
