// Package object is the native engine's format: an immutable object is
// split into segments, encrypted under a convergent key derived from the
// owner's member secret, erasure coded into n shares of which any k rebuild
// it, and described by a small signed record that every share carries. The
// package is pure: no network, no disk, no clock. What it fixes are bytes,
// which is why the format is versioned and pinned by known-answer tests.
//
// This is the "Native engine" section of RFC 0001 as code.
package object

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Format constants.
const (
	FormatVersion      = 1
	DefaultSegmentSize = 1 << 20 // 1 MiB of plaintext per segment
	CipherAESGCM       = 1       // AES-256-GCM, chosen at G0
	CipherChaCha20     = 2       // ChaCha20-Poly1305, reserved for a network of nodes without AES instructions
	tagSize            = 16
	nonceSize          = 12
	MaxShares          = 255 // share numbers fit a byte; Reed-Solomon allows 256 shards
	labelConvergence   = "redundanet/3/object/convergence"
	domainDescriptor   = "object-descriptor"
)

// Key is an object's symmetric key: 32 bytes, derived from the plaintext.
type Key [32]byte

// StorageIndex is how servers address an object's shares: the first 16
// bytes of BLAKE3("si" || key), from which the key cannot be recovered.
type StorageIndex [16]byte

// Hash is a BLAKE3 digest.
type Hash [32]byte

// Params are the encoding an object is written with. They are part of the
// key, so a re-encoded object is a different object.
type Params struct {
	K, N        uint8
	SegmentSize uint32
	Cipher      uint8
}

// DefaultParams is k-of-n with the default segment size and cipher.
func DefaultParams(k, n int) Params {
	return Params{K: uint8(k), N: uint8(n), SegmentSize: DefaultSegmentSize, Cipher: CipherAESGCM}
}

func (p Params) check() error {
	switch {
	case p.K < 1:
		return errors.New("object: k must be at least 1")
	case p.N <= p.K:
		return errors.New("object: n must be greater than k")
	case int(p.N) > MaxShares:
		return fmt.Errorf("object: n must be at most %d", MaxShares)
	case p.SegmentSize < 1:
		return errors.New("object: the segment size must be positive")
	case p.Cipher != CipherAESGCM:
		return fmt.Errorf("object: cipher %d is not available in this version", p.Cipher)
	}
	return nil
}

// ConvergenceSecret derives the per-member secret that makes two uploads of
// one file by the same member the same object, while two members never learn
// that they hold the same file.
func ConvergenceSecret(ms identity.MemberSecret) [32]byte {
	var out [32]byte
	copy(out[:], ms.Subkey(labelConvergence, 32))
	return out
}

// DeriveKey is BLAKE3 keyed with the convergence secret over the plaintext
// hash and the encoding, so the encoding is part of the key.
func DeriveKey(conv [32]byte, plaintextHash Hash, p Params) Key {
	var enc [7]byte
	enc[0], enc[1] = p.K, p.N
	binary.BigEndian.PutUint32(enc[2:6], p.SegmentSize)
	enc[6] = p.Cipher
	kh, err := blake3.NewKeyed(conv[:])
	if err != nil {
		panic(err) // the key is always 32 bytes
	}
	_, _ = kh.Write(plaintextHash[:])
	_, _ = kh.Write(enc[:])
	var k Key
	copy(k[:], kh.Sum(nil))
	return k
}

// StorageIndex is the object's address on servers.
func (k Key) StorageIndex() StorageIndex {
	h := blake3.New()
	_, _ = h.Write([]byte("si"))
	_, _ = h.Write(k[:])
	var si StorageIndex
	copy(si[:], h.Sum(nil))
	return si
}

// Sum is BLAKE3 of b.
func Sum(b []byte) Hash { return Hash(blake3.Sum256(b)) }

// ShareRoot is the root over a share's block hashes: the hash of the list
// in segment order, a tree of one level, since the list for the largest
// object is 64 blocks and travels whole.
func ShareRoot(blockHashes []Hash) Hash {
	h := blake3.New()
	for _, bh := range blockHashes {
		_, _ = h.Write(bh[:])
	}
	var root Hash
	copy(root[:], h.Sum(nil))
	return root
}

// aead opens the object's cipher.
func aead(key Key, cipherID uint8) (cipher.AEAD, error) {
	if cipherID != CipherAESGCM {
		return nil, fmt.Errorf("object: cipher %d is not available in this version", cipherID)
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// nonce is the segment index; the key is unique per object, so the counter
// never repeats under one key.
func nonce(segment uint64) []byte {
	n := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(n[4:], segment)
	return n
}

// additionalData binds a segment's ciphertext to its object and position.
func additionalData(si StorageIndex, segment, size uint64) []byte {
	ad := make([]byte, 16+8+8)
	copy(ad, si[:])
	binary.BigEndian.PutUint64(ad[16:], segment)
	binary.BigEndian.PutUint64(ad[24:], size)
	return ad
}

// Geometry answers the size questions a reader and a server share.
type Geometry struct {
	Size        uint64
	SegmentSize uint32
	K           uint8
}

// Segments is how many segments the object has; an empty object has one,
// holding only the cipher's tag, so every object has at least one block per
// share.
func (g Geometry) Segments() int {
	if g.Size == 0 {
		return 1
	}
	return int((g.Size + uint64(g.SegmentSize) - 1) / uint64(g.SegmentSize))
}

// PlaintextLen is the plaintext bytes of segment s.
func (g Geometry) PlaintextLen(s int) int {
	start := uint64(s) * uint64(g.SegmentSize)
	if start >= g.Size {
		return 0
	}
	return int(min(uint64(g.SegmentSize), g.Size-start))
}

// CiphertextLen is the ciphertext bytes of segment s: the plaintext and the tag.
func (g Geometry) CiphertextLen(s int) int { return g.PlaintextLen(s) + tagSize }

// BlockSize is the bytes every share holds for segment s: the ciphertext
// split into k equal blocks, the last padded with zeros.
func (g Geometry) BlockSize(s int) int {
	c := g.CiphertextLen(s)
	return (c + int(g.K) - 1) / int(g.K)
}

// ShareBlocksSize is the bytes of blocks one share holds for the object.
func (g Geometry) ShareBlocksSize() int64 {
	var total int64
	for s := 0; s < g.Segments(); s++ {
		total += int64(g.BlockSize(s))
	}
	return total
}

// ---- capabilities ----

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func enc32(b []byte) string { return strings.ToLower(b32.EncodeToString(b)) }

func dec32(s string, n int) ([]byte, error) {
	b, err := b32.DecodeString(strings.ToUpper(s))
	if err != nil || len(b) != n {
		return nil, fmt.Errorf("object: bad base32 field %q", s)
	}
	return b, nil
}

// ReadCap lets its holder fetch, verify and decrypt an object:
// rn:r1:<key>:<descriptor hash>:<size>.
type ReadCap struct {
	Key            Key
	DescriptorHash Hash
	Size           uint64
}

// String is the capability's text form.
func (c ReadCap) String() string {
	return "rn:r1:" + enc32(c.Key[:]) + ":" + enc32(c.DescriptorHash[:]) + ":" + strconv.FormatUint(c.Size, 10)
}

// ParseReadCap parses the text form.
func ParseReadCap(s string) (ReadCap, error) {
	var c ReadCap
	parts := strings.Split(s, ":")
	if len(parts) != 5 || parts[0] != "rn" || parts[1] != "r1" {
		return c, errors.New("object: not a read capability")
	}
	k, err := dec32(parts[2], len(c.Key))
	if err != nil {
		return c, err
	}
	d, err := dec32(parts[3], len(c.DescriptorHash))
	if err != nil {
		return c, err
	}
	size, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil {
		return c, fmt.Errorf("object: bad size in capability: %w", err)
	}
	copy(c.Key[:], k)
	copy(c.DescriptorHash[:], d)
	c.Size = size
	return c, nil
}

// Verify is the verify capability of a read capability: enough to check and
// repair, not to read.
func (c ReadCap) Verify() VerifyCap {
	return VerifyCap{StorageIndex: c.Key.StorageIndex(), DescriptorHash: c.DescriptorHash}
}

// VerifyCap names an object without the key: rn:v1:<storage index>:<descriptor hash>.
type VerifyCap struct {
	StorageIndex   StorageIndex
	DescriptorHash Hash
}

// String is the capability's text form.
func (c VerifyCap) String() string {
	return "rn:v1:" + enc32(c.StorageIndex[:]) + ":" + enc32(c.DescriptorHash[:])
}

// ParseVerifyCap parses the text form.
func ParseVerifyCap(s string) (VerifyCap, error) {
	var c VerifyCap
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != "rn" || parts[1] != "v1" {
		return c, errors.New("object: not a verify capability")
	}
	si, err := dec32(parts[2], len(c.StorageIndex))
	if err != nil {
		return c, err
	}
	d, err := dec32(parts[3], len(c.DescriptorHash))
	if err != nil {
		return c, err
	}
	copy(c.StorageIndex[:], si)
	copy(c.DescriptorHash[:], d)
	return c, nil
}

// String is the storage index in the capability alphabet.
func (si StorageIndex) String() string { return enc32(si[:]) }

// ParseStorageIndex reads a storage index as String prints it.
func ParseStorageIndex(s string) (StorageIndex, error) {
	b, err := dec32(s, len(StorageIndex{}))
	if err != nil {
		return StorageIndex{}, fmt.Errorf("object: storage index: %w", err)
	}
	return StorageIndex(b), nil
}

// String is the hash in the capability alphabet.
func (h Hash) String() string { return enc32(h[:]) }
