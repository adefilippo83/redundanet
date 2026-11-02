package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/zeebo/blake3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
)

// Put stores an object: spool and hash, look it up locally and on the
// grid, encode once, send every share in one pass, top up what is
// missing, and remember the result. A second upload of the same bytes by
// the same member is a no-op.
func (e *Native) Put(ctx context.Context, r io.Reader, size int64) (object.ReadCap, error) {
	servers, p, err := e.servers()
	if err != nil {
		return object.ReadCap{}, err
	}
	spool, err := os.CreateTemp(e.cfg.SpoolDir, "rn3-spool-*")
	if err != nil {
		return object.ReadCap{}, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	h := blake3.New()
	n, err := io.Copy(io.MultiWriter(spool, h), io.LimitReader(r, size))
	if err != nil {
		return object.ReadCap{}, err
	}
	if n != size {
		return object.ReadCap{}, ErrSize
	}
	var ph object.Hash
	copy(ph[:], h.Sum(nil))
	if o, _ := e.cfg.DB.LookupObject(ph, p); o != nil && o.Placed >= int(p.N) {
		return o.Cap, nil
	}
	key := object.DeriveKey(e.conv, ph, p)
	si := key.StorageIndex()
	pres := e.presence(ctx, si, servers)
	enc, err := object.Encode(spool, size, ph, p, e.conv, e.cfg.Member)
	if err != nil {
		return object.ReadCap{}, err
	}
	rc, err := enc.ReadCap()
	if err != nil {
		return object.ReadCap{}, err
	}
	hl := health(rc.Verify(), int(p.K), int(p.N), pres, servers)
	if hl.Placed {
		_ = e.cfg.DB.RememberObject(localdb.Object{PlaintextHash: ph, Params: p, Cap: rc, Placed: len(hl.Shares)})
		return rc, nil
	}
	sd, err := object.Sign(enc.Descriptor, e.cfg.Device, e.cfg.Cert)
	if err != nil {
		return object.ReadCap{}, err
	}
	placed := e.send(ctx, enc, sd, hl, servers)
	if len(placed) < int(p.K) {
		return object.ReadCap{}, fmt.Errorf("%w: %d of %d", ErrNotEnoughServers, len(placed), p.K)
	}
	if err := e.cfg.DB.RememberObject(localdb.Object{PlaintextHash: ph, Params: p, Cap: rc, Placed: len(placed)}); err != nil {
		e.log.Warn("object not remembered", "err", err)
	}
	if len(placed) < int(p.N) {
		e.log.Warn("object under-placed; repair completes it", "si", si.String()[:8], "placed", len(placed), "n", p.N)
	}
	return rc, nil
}

// send places the share numbers the grid lacks: one encoding pass feeds a
// stream per share; a share a server refused is retried on another, once.
// It returns the distinct complete share numbers afterwards.
func (e *Native) send(ctx context.Context, enc *object.Encoded, sd *object.SignedDescriptor, hl *Health, servers []server) []uint8 {
	n := int(enc.Params.N)
	have := map[uint8]bool{}
	for _, s := range hl.Shares {
		have[s] = true
	}
	held := map[identity.Key]int{}
	for dev, shares := range hl.Holders {
		held[dev] = len(shares)
	}
	var missing []int
	for i := 0; i < n; i++ {
		if !have[uint8(i)] {
			missing = append(missing, i)
		}
	}
	bad := map[identity.Key]bool{}
	for pass := 0; pass < 2 && len(missing) > 0; pass++ {
		assignment := plan(sd.Descriptor.StorageIndex, n, missing, servers, held, bad)
		if len(assignment) == 0 {
			break
		}
		streams := map[int]*shareproto.PutStream{}
		for share, dev := range assignment {
			bctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
			ps, err := e.cfg.Client.BeginPut(bctx, dev, sd, uint8(share))
			cancel()
			if err != nil {
				e.log.Info("server refused a share", "server", dev.Short(), "share", share, "err", err)
				bad[dev] = true
				continue
			}
			streams[share] = ps
		}
		if len(streams) > 0 {
			failed := map[int]bool{}
			_ = enc.Shares(ctx, func(share, segment int, b []byte) error {
				ps, ok := streams[share]
				if !ok || failed[share] {
					return nil
				}
				if err := ps.Write(segment, b); err != nil {
					failed[share] = true
				}
				return ctx.Err()
			})
			for share, ps := range streams {
				dev := assignment[share]
				if err := ps.Finish(); err != nil {
					e.log.Info("share not stored", "server", dev.Short(), "share", share, "err", err)
					bad[dev] = true
					continue
				}
				have[uint8(share)] = true
				held[dev]++
			}
		}
		missing = missing[:0]
		for i := 0; i < n; i++ {
			if !have[uint8(i)] {
				missing = append(missing, i)
			}
		}
	}
	var out []uint8
	for s := range have {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Has reports whether the member already stores these bytes: the local
// database first, then the servers.
func (e *Native) Has(ctx context.Context, ph object.Hash, size int64) (object.ReadCap, bool, error) {
	servers, p, err := e.servers()
	if err != nil {
		return object.ReadCap{}, false, err
	}
	if o, _ := e.cfg.DB.LookupObject(ph, p); o != nil && o.Placed >= int(p.K) {
		return o.Cap, true, nil
	}
	key := object.DeriveKey(e.conv, ph, p)
	pres := e.presence(ctx, key.StorageIndex(), servers)
	// The descriptor hash is whatever the holders agree on.
	var dh object.Hash
	for _, pr := range pres.Pres {
		if pr != nil && len(pr.Complete) > 0 && !pr.Tombstoned {
			dh = pr.DescriptorHash
			break
		}
	}
	if dh == (object.Hash{}) {
		return object.ReadCap{}, false, nil
	}
	rc := object.ReadCap{Key: key, DescriptorHash: dh, Size: uint64(size)}
	hl := health(rc.Verify(), int(p.K), int(p.N), pres, servers)
	if !hl.Readable {
		return object.ReadCap{}, false, nil
	}
	_ = e.cfg.DB.RememberObject(localdb.Object{PlaintextHash: ph, Params: p, Cap: rc, Placed: len(hl.Shares)})
	return rc, true, nil
}

var errNoDescriptor = errors.New("engine: no server could give the descriptor")
