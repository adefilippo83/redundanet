package shareproto

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
)

// Policy is what the server asks the manifest.
type Policy interface {
	Network() string
	// MemberStatus reports whether a member key is listed and active or
	// leaving; an unlisted key is neither.
	MemberStatus(member identity.Key) (active, leaving bool)
	// OwnerCap is the bytes this server allows an owner: budget times this
	// server's share of the contribution times the headroom. Zero or less
	// refuses every write.
	OwnerCap(owner identity.Key) int64
	// IsHub reports whether a device key is one of the manifest's hubs,
	// whose signed repair tasks this server honours.
	IsHub(device identity.Key) bool
	// Device is this server's own device key, which a repair task names.
	Device() identity.Key
}

// Server serves the storage protocol over streams of the mesh.
type Server struct {
	st  *store.Store
	pol Policy
	log *slog.Logger
}

// NewServer wires a store and a policy.
func NewServer(st *store.Store, pol Policy, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{st: st, pol: pol, log: log.With("component", "shareproto")}
}

// Handle is the endpoint's OnStream: one request per stream.
//
// A stream is released only when both of its halves are done: the write
// half by Close, the read half by reading to EOF or cancelling it. Every
// request's bytes have been read when a handler returns, so the read half
// is cancelled with code 0 (nothing is lost). A stream left half-open
// counts against the peer's stream limit until the connection ends; the
// first long backup hit that limit after about a thousand requests and
// every further request waited for a stream that never came.
func (s *Server) Handle(peer identity.Peer, _ string, st *quic.Stream) {
	defer st.Close()
	defer st.CancelRead(0)
	var req Request
	if err := readMsg(st, &req); err != nil {
		st.CancelRead(1)
		return
	}
	var err error
	switch req.Type {
	case TypePut:
		err = s.put(peer, st, &req)
	case TypeGet:
		err = s.get(peer, st, &req)
	case TypeHas:
		err = s.has(peer, st, &req)
	case TypePutRoot:
		err = s.putRoot(peer, st, &req)
	case TypeGetRoots:
		err = s.getRoots(peer, st, &req)
	case TypeDelete:
		err = s.delete(peer, st, &req)
	case TypeUndelete:
		err = s.undelete(peer, st, &req)
	case TypeList:
		err = s.list(peer, st, &req)
	default:
		err = writeMsg(st, Response{Status: StatusBad, Message: "unknown request " + req.Type})
	}
	if err != nil {
		s.log.Debug("request ended", "type", req.Type, "peer", peer.Device.Short(), "err", err)
		st.CancelRead(1)
	}
}

func refuse(st io.Writer, status, msg string) error {
	return writeMsg(st, Response{Status: status, Message: msg})
}

func statusOf(err error) (string, string) {
	switch {
	case errors.Is(err, store.ErrUnavailable):
		return StatusUnavailable, err.Error()
	case errors.Is(err, store.ErrNoSpace):
		return StatusNoSpace, err.Error()
	case errors.Is(err, store.ErrTombstoned):
		return StatusTombstoned, err.Error()
	case errors.Is(err, store.ErrDescriptor):
		return StatusDescriptor, err.Error()
	case errors.Is(err, store.ErrNotFound):
		return StatusNotFound, err.Error()
	case errors.Is(err, store.ErrPartial):
		return StatusPartial, err.Error()
	case errors.Is(err, store.ErrLowerVersion):
		return StatusLowerVersion, err.Error()
	case errors.Is(err, store.ErrRecent):
		return StatusRecent, err.Error()
	case errors.Is(err, store.ErrStale):
		return StatusStale, err.Error()
	case errors.Is(err, store.ErrNotOwner):
		return StatusRefused, err.Error()
	case errors.Is(err, store.ErrRoot), errors.Is(err, object.ErrCorrupt):
		return StatusBad, err.Error()
	}
	return StatusFailed, err.Error()
}

func presenceOf(p *store.Presence) *Presence {
	if p == nil {
		return nil
	}
	return &Presence{DescriptorHash: p.DescriptorHash, Owner: p.Owner, Complete: p.Complete, Partial: p.Partial, Tombstoned: p.Tombstoned, Purged: p.Purged}
}

// delete applies a delete token: the first phase of deletion, from any
// device of the member whose token it is, so a late token queued on one
// device may be delivered by another.
func (s *Server) delete(peer identity.Peer, st *quic.Stream, req *Request) error {
	t := req.Token
	if t == nil {
		return refuse(st, StatusBad, "delete without a token")
	}
	if err := t.Verify(s.pol.Network()); err != nil {
		return refuse(st, StatusBad, err.Error())
	}
	if t.Member != peer.Member {
		return refuse(st, StatusRefused, "a delete token is accepted from the member's own devices only")
	}
	if t.StorageIndex != req.StorageIndex {
		return refuse(st, StatusBad, "the storage index does not match the token")
	}
	if err := s.st.Available(); err != nil {
		return refuse(st, StatusUnavailable, err.Error())
	}
	p, err := s.st.ApplyDelete(t.StorageIndex, t.Member, t.DescriptorHash, t.IssuedAt)
	if err != nil {
		code, msg := statusOf(err)
		return refuse(st, code, msg)
	}
	return writeMsg(st, Response{Status: StatusOK, Presence: presenceOf(p)})
}

// undelete clears a tombstone the member's own token placed, within the
// window.
func (s *Server) undelete(peer identity.Peer, st *quic.Stream, req *Request) error {
	if err := s.st.Available(); err != nil {
		return refuse(st, StatusUnavailable, err.Error())
	}
	if err := s.st.Undelete(req.StorageIndex, peer.Member); err != nil {
		code, msg := statusOf(err)
		return refuse(st, code, msg)
	}
	return writeMsg(st, Response{Status: StatusOK})
}

// list pages through the objects held for the asking member: the grid's
// side of the owner's cleanup, never a local cache.
func (s *Server) list(peer identity.Peer, st *quic.Stream, req *Request) error {
	if err := s.st.Available(); err != nil {
		return refuse(st, StatusUnavailable, err.Error())
	}
	page, more, err := s.st.ListOwner(peer.Member, req.After, ListPage)
	if err != nil {
		return refuse(st, StatusFailed, err.Error())
	}
	resp := Response{Status: StatusOK, More: more, Objects: make([]ListedObject, 0, len(page))}
	for _, l := range page {
		resp.Objects = append(resp.Objects, ListedObject{StorageIndex: l.StorageIndex, DescriptorHash: l.DescriptorHash, LastTouched: l.LastTouched, Tombstoned: l.Tombstoned})
	}
	return writeMsg(st, resp)
}

// put receives one share: the checks of "Who may store what", in order.
func (s *Server) put(peer identity.Peer, st *quic.Stream, req *Request) error {
	sd := req.Descriptor
	if sd == nil {
		return refuse(st, StatusBad, "put without a descriptor")
	}
	if err := sd.Verify(s.pol.Network()); err != nil {
		return refuse(st, StatusBad, err.Error())
	}
	owner := sd.Descriptor.Owner
	if sd.Descriptor.StorageIndex != req.StorageIndex {
		return refuse(st, StatusBad, "the storage index does not match the descriptor")
	}
	// A device that is not the owner's stores a share only as a repair
	// worker carrying a task signed by a hub of this network that names
	// it, this object under this descriptor, and this server for this
	// share number.
	repair := peer.Member != owner
	if repair {
		t := req.Task
		if t == nil {
			return refuse(st, StatusRefused, "only a device of the owner may store this object without a repair task")
		}
		if err := t.Verify(s.pol.Network(), time.Now()); err != nil {
			return refuse(st, StatusRefused, err.Error())
		}
		if !s.pol.IsHub(t.Hub) {
			return refuse(st, StatusRefused, "the repair task is not signed by a hub of this network")
		}
		if t.Worker != peer.Device {
			return refuse(st, StatusRefused, "the repair task names another worker")
		}
		dhash, err := sd.Descriptor.Hash()
		if err != nil {
			return refuse(st, StatusFailed, err.Error())
		}
		if object.StorageIndex(t.StorageIndex) != req.StorageIndex || object.Hash(t.DescriptorHash) != dhash || t.Owner != owner {
			return refuse(st, StatusRefused, "the repair task is for another object")
		}
		if !t.Places(s.pol.Device(), req.Number) {
			return refuse(st, StatusRefused, fmt.Sprintf("the repair task does not place share %d on this server", req.Number))
		}
	}
	// Writes need an active owner; repair on a leaving member's behalf
	// continues, so their data is still there if they return.
	if active, leaving := s.pol.MemberStatus(owner); !active && !(repair && leaving) {
		return refuse(st, StatusRefused, "the owner is not an active member")
	}
	geom := sd.Descriptor.Geometry()
	var p *store.Put
	var err error
	if repair {
		// Repair writes may exceed the owner's cap, using the reserve, but
		// never the free-space floor, which the store applies.
		p, err = s.st.BeginRepair(sd, req.Number)
	} else {
		usage, uerr := s.st.Usage(owner)
		if uerr != nil {
			return refuse(st, StatusFailed, uerr.Error())
		}
		if capBytes := s.pol.OwnerCap(owner); usage+object.ShareSize(geom, 0) > capBytes {
			return refuse(st, StatusQuota, fmt.Sprintf("the owner holds %d bytes here of %d allowed", usage, capBytes))
		}
		p, err = s.st.BeginPut(sd, req.Number)
	}
	if errors.Is(err, store.ErrAlreadyComplete) {
		return writeMsg(st, Response{Status: StatusOK, Complete: true, Resume: geom.Segments()})
	}
	if err != nil {
		code, msg := statusOf(err)
		return refuse(st, code, msg)
	}
	if err := writeMsg(st, Response{Status: StatusOK, Resume: p.Resume()}); err != nil {
		p.Abort()
		return err
	}
	for seg := p.Resume(); seg < p.Segments(); seg++ {
		b := make([]byte, geom.BlockSize(seg))
		if _, err := io.ReadFull(st, b); err != nil {
			p.Abort() // kept for a resume
			return err
		}
		if err := p.WriteBlock(seg, b); err != nil {
			p.Abort()
			code, msg := statusOf(err)
			return refuse(st, code, msg)
		}
	}
	if err := p.Complete(); err != nil {
		code, msg := statusOf(err)
		return refuse(st, code, msg)
	}
	s.log.Info("share stored", "si", req.StorageIndex.String()[:8], "n", req.Number, "owner", owner.Short(), "from", peer.Device.Short(), "repair", repair)
	return writeMsg(st, Response{Status: StatusOK, Complete: true, Resume: geom.Segments()})
}

// get serves a complete share's hashes and a range of its blocks.
func (s *Server) get(peer identity.Peer, st *quic.Stream, req *Request) error {
	if err := s.st.Available(); err != nil {
		return refuse(st, StatusUnavailable, err.Error())
	}
	pres, err := s.st.Has(req.StorageIndex)
	if err != nil {
		return refuse(st, StatusFailed, err.Error())
	}
	if pres != nil && pres.Tombstoned && peer.Member != pres.Owner {
		return refuse(st, StatusTombstoned, "deleted by its owner")
	}
	sh, err := s.st.OpenShare(req.StorageIndex, req.Number)
	if err != nil {
		code, msg := statusOf(err)
		return refuse(st, code, msg)
	}
	defer sh.Close()
	segments := len(sh.Hashes)
	from, to := req.From, req.To
	if to == 0 {
		to = segments
	}
	if from < 0 || to > segments || from > to {
		return refuse(st, StatusBad, fmt.Sprintf("segments %d to %d of %d", from, to, segments))
	}
	resp := Response{Status: StatusOK, Hashes: sh.Hashes, Segments: segments}
	if req.HashesOnly {
		resp.Descriptor = sh.Header.Descriptor
		return writeMsg(st, resp)
	}
	if err := writeMsg(st, resp); err != nil {
		return err
	}
	for seg := from; seg < to; seg++ {
		b, err := sh.Block(seg)
		if err != nil {
			return err
		}
		if _, err := st.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// has answers what the server holds; a hit by the owner's device touches
// the object's clock.
func (s *Server) has(peer identity.Peer, st *quic.Stream, req *Request) error {
	if err := s.st.Available(); err != nil {
		return refuse(st, StatusUnavailable, err.Error())
	}
	pres, err := s.st.Has(req.StorageIndex)
	if err != nil {
		return refuse(st, StatusFailed, err.Error())
	}
	if pres == nil {
		return writeMsg(st, Response{Status: StatusOK})
	}
	if peer.Member == pres.Owner {
		_ = s.st.Touch(req.StorageIndex)
	}
	return writeMsg(st, Response{Status: StatusOK, Presence: presenceOf(pres)})
}

// putRoot stores a device's root, from that device only.
func (s *Server) putRoot(peer identity.Peer, st *quic.Stream, req *Request) error {
	e := req.Root
	if e == nil {
		return refuse(st, StatusBad, "put-root without a root")
	}
	if err := e.Verify(s.pol.Network()); err != nil {
		return refuse(st, StatusBad, err.Error())
	}
	if e.Device != peer.Device || e.Member != peer.Member {
		return refuse(st, StatusRefused, "a root is accepted from its own device only")
	}
	if active, _ := s.pol.MemberStatus(e.Member); !active {
		return refuse(st, StatusRefused, "the member is not active")
	}
	enc, err := e.Encode()
	if err != nil {
		return refuse(st, StatusFailed, err.Error())
	}
	if err := s.st.PutRoot(store.RootRecord{Member: e.Member, Device: e.Device, Version: e.Version, Envelope: enc}); err != nil {
		code, msg := statusOf(err)
		return refuse(st, code, msg)
	}
	return writeMsg(st, Response{Status: StatusOK})
}

// getRoots lists a member's roots to that member's devices.
func (s *Server) getRoots(peer identity.Peer, st *quic.Stream, req *Request) error {
	if req.Member != peer.Member {
		return refuse(st, StatusRefused, "roots are listed to the member's own devices")
	}
	recs, err := s.st.Roots(req.Member)
	if err != nil {
		return refuse(st, StatusFailed, err.Error())
	}
	resp := Response{Status: StatusOK}
	for _, r := range recs {
		e, err := DecodeRoot(r.Envelope)
		if err != nil {
			continue
		}
		resp.Roots = append(resp.Roots, e)
	}
	return writeMsg(st, resp)
}

// RefuseAll is the stream handler of a device that stores nothing: every
// request gets a refusal, so a caller learns why instead of hanging.
func RefuseAll(_ identity.Peer, _ string, st *quic.Stream) {
	defer st.Close()
	var req Request
	if err := readMsg(st, &req); err != nil {
		st.CancelRead(1)
		return
	}
	_ = writeMsg(st, Response{Status: StatusRefused, Message: "this device is not a storage node"})
}
