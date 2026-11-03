package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
)

// locate finds an object's holders and its descriptor, verified against
// the capability.
func (e *Native) locate(ctx context.Context, rc object.ReadCap) (*object.Descriptor, *Health, []shareproto.Source, error) {
	servers, p, err := e.servers()
	if err != nil {
		return nil, nil, nil, err
	}
	si := rc.Key.StorageIndex()
	pres := e.presence(ctx, si, servers)
	hl := health(rc.Verify(), int(p.K), int(p.N), pres, servers)
	// One holder per share number, the fastest answering first. The
	// owner may still read an object under a tombstone within the window,
	// so those holders serve when the live ones are not enough.
	type holder struct {
		dev identity.Key
		rtt int64
	}
	byShare := map[uint8][]holder{}
	numbers := map[uint8]bool{}
	for dev, shares := range hl.Holders {
		for _, s := range shares {
			byShare[s] = append(byShare[s], holder{dev, int64(pres.RTT[dev])})
			numbers[s] = true
		}
	}
	if len(numbers) < int(p.K) {
		for dev, shares := range hl.Ghosts {
			for _, s := range shares {
				byShare[s] = append(byShare[s], holder{dev, int64(pres.RTT[dev])})
				numbers[s] = true
			}
		}
	}
	if len(numbers) < int(p.K) {
		return nil, hl, nil, fmt.Errorf("%w: %d of %d shares on %d of %d servers", ErrNotReadable, len(numbers), p.K, hl.Answered, hl.Asked)
	}
	var all []uint8
	for s := range numbers {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	var desc *object.Descriptor
	var sources []shareproto.Source
	for _, s := range all {
		hs := byShare[s]
		sort.Slice(hs, func(i, j int) bool { return hs[i].rtt < hs[j].rtt })
		dev := hs[0].dev
		if desc == nil {
			for _, h := range hs {
				cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
				sd, _, err := e.cfg.Client.GetInfo(cctx, h.dev, si, s)
				cancel()
				if err != nil || sd == nil {
					continue
				}
				if err := sd.Verify(e.cfg.Network); err != nil {
					continue
				}
				if dh, _ := sd.Descriptor.Hash(); dh != rc.DescriptorHash {
					continue
				}
				d := sd.Descriptor
				desc = &d
				break
			}
		}
		if desc == nil {
			continue
		}
		sources = append(sources, shareproto.Source{Share: int(s), Client: e.cfg.Client, Peer: dev, StorageIndex: si, Geometry: desc.Geometry()})
	}
	if desc == nil {
		return nil, hl, nil, errNoDescriptor
	}
	return desc, hl, sources, nil
}

// Describe fetches an object's descriptor from a holder, verified against
// the capability: what rebalance asks when the local database does not
// know an object's encoding.
func (e *Native) Describe(ctx context.Context, rc object.ReadCap) (*object.Descriptor, error) {
	desc, _, _, err := e.locate(ctx, rc)
	return desc, err
}

// Get reads an object, or a byte range of it, with every block verified.
func (e *Native) Get(ctx context.Context, rc object.ReadCap, w io.Writer, rng *Range) error {
	desc, _, sources, err := e.locate(ctx, rc)
	if err != nil {
		return err
	}
	srcs, err := shareproto.Sources(ctx, sources)
	if err != nil {
		return err
	}
	if rng == nil {
		return object.Decode(ctx, w, rc, desc, &e.conv, srcs)
	}
	if rng.Offset < 0 || rng.Length < 0 || uint64(rng.Offset+rng.Length) > rc.Size {
		return errors.New("engine: range outside the object")
	}
	if rng.Length == 0 {
		return nil
	}
	seg := int64(desc.SegmentSize)
	from, to := int(rng.Offset/seg), int((rng.Offset+rng.Length-1)/seg)+1
	var buf bytes.Buffer
	if err := object.DecodeRange(ctx, &buf, rc, desc, srcs, from, to); err != nil {
		return err
	}
	start := rng.Offset - int64(from)*seg
	_, err = w.Write(buf.Bytes()[start : start+rng.Length])
	return err
}

// Check reports where an object's shares are.
func (e *Native) Check(ctx context.Context, rc object.ReadCap) (*Health, error) {
	servers, p, err := e.servers()
	if err != nil {
		return nil, err
	}
	pres := e.presence(ctx, rc.Key.StorageIndex(), servers)
	return health(rc.Verify(), int(p.K), int(p.N), pres, servers), nil
}

// Repair is the owner's repair: read the object back, regenerate the
// missing share numbers and place them. Repair without the owner, by hub
// tasks and workers, comes in M5.
func (e *Native) Repair(ctx context.Context, rc object.ReadCap) (*Health, error) {
	desc, hl, sources, err := e.locate(ctx, rc)
	if err != nil {
		return hl, err
	}
	if hl.Placed {
		return hl, nil
	}
	srcs, err := shareproto.Sources(ctx, sources)
	if err != nil {
		return hl, err
	}
	spool, err := os.CreateTemp(e.cfg.SpoolDir, "rn3-repair-*")
	if err != nil {
		return hl, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	if err := object.Decode(ctx, spool, rc, desc, &e.conv, srcs); err != nil {
		return hl, err
	}
	ph, err := object.HashSpool(spool, int64(rc.Size))
	if err != nil {
		return hl, err
	}
	enc, err := object.Encode(spool, int64(rc.Size), ph, desc.Params(), e.conv, e.cfg.Member)
	if err != nil {
		return hl, err
	}
	if again, _ := enc.ReadCap(); again != rc {
		return hl, errors.New("engine: the re-encoded object differs from the capability")
	}
	sd, err := object.Sign(enc.Descriptor, e.cfg.Device, e.cfg.Cert)
	if err != nil {
		return hl, err
	}
	servers, _, _ := e.servers()
	e.send(ctx, enc, sd, hl, servers)
	return e.Check(ctx, rc)
}
