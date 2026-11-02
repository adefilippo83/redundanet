package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/adefilippo83/redundanet/agent/internal/object"
)

// Put is a share being received: the header is on disk, blocks follow in
// segment order, and Complete verifies them against the signed root.
type Put struct {
	s        *Store
	sd       *object.SignedDescriptor
	si       object.StorageIndex
	n        uint8
	geom     object.Geometry
	hdrLen   int
	f        *os.File
	received int // segments on disk
}

// Resume is the segment the sender should continue from.
func (p *Put) Resume() int { return p.received }

// Segments is the total.
func (p *Put) Segments() int { return p.geom.Segments() }

// BeginPut starts or resumes receiving share n of the object the signed
// descriptor describes. The descriptor must already be verified by the
// caller. It returns ErrAlreadyComplete when nothing needs moving.
func (s *Store) BeginPut(sd *object.SignedDescriptor, n uint8) (*Put, error) {
	if err := s.Available(); err != nil {
		return nil, err
	}
	d := &sd.Descriptor
	if int(n) >= int(d.N) {
		return nil, fmt.Errorf("store: share number %d of n=%d", n, d.N)
	}
	dhash, err := d.Hash()
	if err != nil {
		return nil, err
	}
	si := d.StorageIndex
	pres, err := s.Has(si)
	if err != nil {
		return nil, err
	}
	if pres != nil {
		if pres.DescriptorHash != dhash {
			return nil, ErrDescriptor
		}
		if pres.Tombstoned {
			if pres.TombstoneOwner != d.Owner {
				return nil, ErrTombstoned
			}
			// An upload from a device of the owner clears the tombstone.
			if err := s.ClearTombstone(si); err != nil {
				return nil, err
			}
		}
		for _, c := range pres.Complete {
			if c == n {
				_ = s.Touch(si)
				return nil, ErrAlreadyComplete
			}
		}
	}
	free, _, err := s.Free()
	if err == nil && int64(free) < s.cfg.Floor {
		return nil, ErrNoSpace
	}
	geom := d.Geometry()
	path := s.sharePath(si, n)
	p := &Put{s: s, sd: sd, si: si, n: n, geom: geom}
	if received, ok := pres.partial(n); ok {
		// Resume: the header is on disk; trust the index's count.
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		hdr, err := object.ReadShareHeader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		p.f, p.hdrLen, p.received = f, hdr.Len, int(received)
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	hdrLen, err := object.WriteShareHeader(f, sd, n)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	p.f, p.hdrLen = f, hdrLen
	size := roundUsage(object.ShareSize(geom, hdrLen))
	now := s.now().Unix()
	if _, err := s.db.Exec(`INSERT INTO shares (si, n, owner, dhash, k, nn, size, state, segments, received, created, last_touched, verified, gen)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'partial', ?, 0, ?, ?, NULL, 0)`,
		si[:], n, d.Owner[:], dhash[:], d.K, d.N, size, geom.Segments(), now, now); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return p, nil
}

func (p *Presence) partial(n uint8) (uint32, bool) {
	if p == nil {
		return 0, false
	}
	r, ok := p.Partial[n]
	return r, ok
}

// WriteBlock stores the next block; blocks arrive in segment order.
func (p *Put) WriteBlock(segment int, b []byte) error {
	if segment != p.received {
		return ErrOutOfOrder
	}
	if len(b) != p.geom.BlockSize(segment) {
		return fmt.Errorf("store: block %d has %d bytes, the geometry says %d", segment, len(b), p.geom.BlockSize(segment))
	}
	if _, err := p.f.WriteAt(b, object.BlockOffset(p.geom, p.hdrLen, segment)); err != nil {
		return err
	}
	p.received++
	_, err := p.s.db.Exec(`UPDATE shares SET received = ?, last_touched = ? WHERE si = ? AND n = ?`, p.received, p.s.now().Unix(), p.si[:], p.n)
	return err
}

// Complete verifies every block against the signed share root, writes the
// hash trailer and marks the share complete. A share that does not match
// is removed.
func (p *Put) Complete() error {
	defer p.f.Close()
	segments := p.geom.Segments()
	if p.received != segments {
		return fmt.Errorf("%w: %d of %d segments", ErrPartial, p.received, segments)
	}
	hashes := make([]object.Hash, segments)
	buf := make([]byte, p.geom.BlockSize(0))
	for s := 0; s < segments; s++ {
		b := buf[:p.geom.BlockSize(s)]
		if _, err := p.f.ReadAt(b, object.BlockOffset(p.geom, p.hdrLen, s)); err != nil {
			return err
		}
		hashes[s] = object.Sum(b)
	}
	if object.ShareRoot(hashes) != p.sd.Descriptor.ShareRoots[p.n] {
		_ = p.f.Close()
		_ = p.s.Remove(p.si, p.n, "blocks do not match the signed root")
		return ErrRoot
	}
	if _, err := p.f.Seek(object.BlockOffset(p.geom, p.hdrLen, segments), io.SeekStart); err != nil {
		return err
	}
	if err := object.WriteShareTrailer(p.f, hashes); err != nil {
		return err
	}
	if err := p.f.Sync(); err != nil {
		return err
	}
	tx, err := p.s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	g, err := bump(tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE shares SET state = 'complete', received = ?, last_touched = ?, gen = ? WHERE si = ? AND n = ?`,
		segments, p.s.now().Unix(), g, p.si[:], p.n); err != nil {
		return err
	}
	return tx.Commit()
}

// Abort leaves the partial share for a later resume.
func (p *Put) Abort() { _ = p.f.Close() }

// Share is a complete share open for reading.
type Share struct {
	Header *object.ShareHeader
	Hashes []object.Hash
	geom   object.Geometry
	f      *os.File
}

// OpenShare opens a complete share.
func (s *Store) OpenShare(si object.StorageIndex, n uint8) (*Share, error) {
	var state string
	err := s.db.QueryRow(`SELECT state FROM shares WHERE si = ? AND n = ?`, si[:], n).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != "complete" {
		return nil, ErrPartial
	}
	f, err := os.Open(s.sharePath(si, n))
	if err != nil {
		return nil, err
	}
	hdr, err := object.ReadShareHeader(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	geom := hdr.Descriptor.Descriptor.Geometry()
	if _, err := f.Seek(object.BlockOffset(geom, hdr.Len, geom.Segments()), io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	hashes, err := object.ReadShareTrailer(f, geom.Segments())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Share{Header: hdr, Hashes: hashes, geom: geom, f: f}, nil
}

// Block reads block s.
func (sh *Share) Block(s int) ([]byte, error) {
	if s < 0 || s >= sh.geom.Segments() {
		return nil, fmt.Errorf("store: no segment %d", s)
	}
	b := make([]byte, sh.geom.BlockSize(s))
	_, err := sh.f.ReadAt(b, object.BlockOffset(sh.geom, sh.Header.Len, s))
	return b, err
}

// Verify checks every block against the trailer and the trailer against
// the signed root.
func (sh *Share) Verify() error {
	if object.ShareRoot(sh.Hashes) != sh.Header.Descriptor.Descriptor.ShareRoots[sh.Header.Number] {
		return object.ErrCorrupt
	}
	for s := 0; s < sh.geom.Segments(); s++ {
		b, err := sh.Block(s)
		if err != nil {
			return err
		}
		if object.Sum(b) != sh.Hashes[s] {
			return fmt.Errorf("%w: block %d", object.ErrCorrupt, s)
		}
	}
	return nil
}

// Close closes the file.
func (sh *Share) Close() { _ = sh.f.Close() }
