package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

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
	if o, _ := e.cfg.DB.LookupObject(ph, p); o != nil && o.Placed >= int(p.N) && e.fresh(o) {
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
	// Nothing is sent only when the servers vouch for every share number,
	// each for no more than its part: see vouched.
	if v := vouched(hl, len(servers)); len(v) >= int(p.N) {
		_ = e.cfg.DB.RememberObject(localdb.Object{PlaintextHash: ph, Params: p, Cap: rc, Placed: len(v)})
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
// What the grid has is what the servers vouch for (see vouched), so a
// server that claims every share does not keep the others from getting
// theirs. It returns the distinct share numbers vouched for or stored by
// this call.
func (e *Native) send(ctx context.Context, enc *object.Encoded, sd *object.SignedDescriptor, hl *Health, servers []server) []uint8 {
	n := int(enc.Params.N)
	have := map[uint8]bool{}
	for _, s := range vouched(hl, len(servers)) {
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
// database first, then the servers. A server's answer alone is not taken:
// the object counts as stored only under a descriptor that a holder
// produces and that this member signed for exactly this object, which no
// server can make up for bytes that were never uploaded, and only when
// the servers vouch for k share numbers, each for no more than its part.
func (e *Native) Has(ctx context.Context, ph object.Hash, size int64) (object.ReadCap, bool, error) {
	servers, p, err := e.servers()
	if err != nil {
		return object.ReadCap{}, false, err
	}
	if o, _ := e.cfg.DB.LookupObject(ph, p); o != nil && o.Placed >= int(p.K) && e.fresh(o) {
		return o.Cap, true, nil
	}
	key := object.DeriveKey(e.conv, ph, p)
	pres := e.presence(ctx, key.StorageIndex(), servers)
	// Every descriptor hash a server names is a candidate, in a fixed
	// order; the servers' word decides nothing about which one is right.
	seen := map[object.Hash]bool{}
	var offered []object.Hash
	for _, pr := range pres.Pres {
		if pr != nil && len(pr.Complete) > 0 && !pr.Tombstoned && !seen[pr.DescriptorHash] {
			seen[pr.DescriptorHash] = true
			offered = append(offered, pr.DescriptorHash)
		}
	}
	sort.Slice(offered, func(i, j int) bool { return bytes.Compare(offered[i][:], offered[j][:]) < 0 })
	for _, dh := range offered {
		rc := object.ReadCap{Key: key, DescriptorHash: dh, Size: uint64(size)}
		hl := health(rc.Verify(), int(p.K), int(p.N), pres, servers)
		v := vouched(hl, len(servers))
		if len(v) < int(p.K) || !e.described(ctx, rc, p, hl) {
			continue
		}
		_ = e.cfg.DB.RememberObject(localdb.Object{PlaintextHash: ph, Params: p, Cap: rc, Placed: len(v)})
		return rc, true, nil
	}
	return object.ReadCap{}, false, nil
}

// described reports whether a holder produces the descriptor the
// capability names, signed by a device of this member for this object:
// the proof that the object was uploaded. Each holder is asked once.
func (e *Native) described(ctx context.Context, rc object.ReadCap, p object.Params, hl *Health) bool {
	si := rc.Key.StorageIndex()
	devs := make([]identity.Key, 0, len(hl.Holders))
	for dev := range hl.Holders {
		devs = append(devs, dev)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Less(devs[j]) })
	for _, dev := range devs {
		shares := hl.Holders[dev]
		if len(shares) == 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
		sd, _, err := e.cfg.Client.GetInfo(cctx, dev, e.cfg.Member, si, shares[0])
		cancel()
		if err != nil || sd == nil || sd.Verify(e.cfg.Network) != nil {
			continue
		}
		d := &sd.Descriptor
		if dh, err := d.Hash(); err != nil || dh != rc.DescriptorHash {
			continue
		}
		if d.Owner == e.cfg.Member && d.StorageIndex == si && d.Size == rc.Size && d.Params() == p {
			return true
		}
	}
	return false
}

var errNoDescriptor = errors.New("engine: no server could give the descriptor")

// reuseProbe is how long the local database's word on an object is taken
// without asking the servers. A reuse through the cache alone never
// touches the object on the servers, and the owner's cleanup deletes what
// no root references once it has gone untouched for the immunity; so
// an object the cache has not confirmed within the immunity is probed,
// which touches it on every server and finds a tombstone in time to
// upload again.
const reuseProbe = 6 * 24 * time.Hour

func (e *Native) fresh(o *localdb.Object) bool {
	return e.cfg.Now().Sub(o.LastUsed) < reuseProbe
}
