package shareproto

import (
	"context"
	"fmt"
	"io"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Opener opens a stream to a peer; the endpoint's OpenStream fits.
type Opener func(ctx context.Context, peer identity.Key) (wire.Stream, string, error)

// Client speaks the protocol to storage servers.
type Client struct{ open Opener }

// NewClient wraps an opener.
func NewClient(open Opener) *Client { return &Client{open: open} }

// PathLocal is the path to this device's own server: in process.
const PathLocal = "local"

// LocalOpener opens streams to this device's own server without the
// network: each is an in-process pair whose far end goes to handle, as the
// endpoint hands it a peer's stream. A storage node is a server of its own
// backups; dialled over the mesh it would be both ends of one connection,
// which a session cannot hold, so the engine routes its own key here.
func LocalOpener(self identity.Peer, handle func(identity.Peer, string, wire.Stream)) Opener {
	return func(ctx context.Context, _ identity.Key) (wire.Stream, string, error) {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		near, far := wire.Loopback()
		go handle(self, PathLocal, far)
		return near, PathLocal, nil
	}
}

// done releases a stream whose last byte has been read: the read half is
// cancelled with code 0 and the write half closed, so quic-go frees the
// stream on both ends and the server's stream limit gets its credit back.
// Without it a client leaks one half-open stream per request.
func done(st wire.Stream) {
	st.CancelRead(0)
	_ = st.Close()
}

func (c *Client) exchange(ctx context.Context, peer identity.Key, req *Request) (wire.Stream, *Response, error) {
	st, _, err := c.open(ctx, peer)
	if err != nil {
		return nil, nil, err
	}
	if err := writeMsg(st, req); err != nil {
		st.CancelRead(1)
		_ = st.Close()
		return nil, nil, err
	}
	var resp Response
	if err := readMsg(st, &resp); err != nil {
		st.CancelRead(1)
		_ = st.Close()
		return nil, nil, fmt.Errorf("shareproto: %s: %w", req.Type, err)
	}
	if resp.Status != StatusOK {
		st.CancelRead(1)
		_ = st.Close()
		return nil, nil, &StatusError{Status: resp.Status, Message: resp.Message}
	}
	return st, &resp, nil
}

// Has asks what a server holds of an owner's object; nil when nothing. A
// server from before objects were keyed by owner does not read the owner
// and answers for whoever holds the storage index there: what it holds
// for another member is nothing of this object.
func (c *Client) Has(ctx context.Context, peer identity.Key, owner identity.Key, si object.StorageIndex) (*Presence, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeHas, StorageIndex: si, Owner: owner})
	if err != nil {
		return nil, err
	}
	done(st)
	if resp.Presence != nil && resp.Presence.Owner != owner {
		return nil, nil
	}
	return resp.Presence, nil
}

// PutShare stores share n of an object, resuming from the segment the
// server asks for. block supplies the block of a segment.
func (c *Client) PutShare(ctx context.Context, peer identity.Key, sd *object.SignedDescriptor, n uint8, block func(segment int) ([]byte, error)) error {
	geom := sd.Descriptor.Geometry()
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypePut, StorageIndex: sd.Descriptor.StorageIndex, Number: n, Descriptor: sd})
	if err != nil {
		return err
	}
	defer done(st)
	if resp.Complete {
		return nil
	}
	for seg := resp.Resume; seg < geom.Segments(); seg++ {
		b, err := block(seg)
		if err != nil {
			st.CancelRead(1)
			return err
		}
		if len(b) != geom.BlockSize(seg) {
			st.CancelRead(1)
			return fmt.Errorf("shareproto: block %d has %d bytes, the geometry says %d", seg, len(b), geom.BlockSize(seg))
		}
		if _, err := st.Write(b); err != nil {
			return err
		}
	}
	var done Response
	if err := readMsg(st, &done); err != nil {
		return fmt.Errorf("shareproto: put: %w", err)
	}
	if done.Status != StatusOK {
		return &StatusError{Status: done.Status, Message: done.Message}
	}
	return nil
}

// PutStream is a share being sent block by block: what one encoding pass
// over the spool feeds, one stream per share, all at once.
type PutStream struct {
	st       wire.Stream
	geom     object.Geometry
	resume   int
	complete bool
	next     int
	err      error
}

// Resume is the segment the server wants first; blocks before it are
// skipped by Write.
func (p *PutStream) Resume() int { return p.resume }

// Complete reports that the server already held the share: nothing to send.
func (p *PutStream) Complete() bool { return p.complete }

// Write sends the block of a segment; segments arrive in order.
func (p *PutStream) Write(segment int, b []byte) error {
	if p.err != nil {
		return p.err
	}
	if p.complete || segment < p.resume {
		return nil
	}
	if segment != p.next {
		p.err = fmt.Errorf("shareproto: segment %d out of order, expected %d", segment, p.next)
		return p.err
	}
	if len(b) != p.geom.BlockSize(segment) {
		p.err = fmt.Errorf("shareproto: block %d has %d bytes, the geometry says %d", segment, len(b), p.geom.BlockSize(segment))
		return p.err
	}
	if _, err := p.st.Write(b); err != nil {
		p.err = err
		return err
	}
	p.next++
	return nil
}

// Finish waits for the server's verdict and closes the stream.
func (p *PutStream) Finish() error {
	defer done(p.st)
	if p.err != nil {
		p.st.CancelRead(1)
		return p.err
	}
	if p.complete {
		return nil
	}
	if p.next != p.geom.Segments() {
		p.st.CancelRead(1)
		return fmt.Errorf("shareproto: only %d of %d segments were written", p.next, p.geom.Segments())
	}
	var done Response
	if err := readMsg(p.st, &done); err != nil {
		return fmt.Errorf("shareproto: put: %w", err)
	}
	if done.Status != StatusOK {
		return &StatusError{Status: done.Status, Message: done.Message}
	}
	return nil
}

// Abandon drops the stream; the server keeps the partial for a resume.
func (p *PutStream) Abandon() {
	p.st.CancelRead(1)
	_ = p.st.Close()
}

// BeginPut opens a put for share n and learns where to start.
func (c *Client) BeginPut(ctx context.Context, peer identity.Key, sd *object.SignedDescriptor, n uint8) (*PutStream, error) {
	return c.beginPut(ctx, peer, sd, n, nil)
}

// BeginRepairPut is BeginPut by a repair worker: the hub's signed task
// travels with the request, and the server checks it before it accepts a
// share from a device that is not the owner's.
func (c *Client) BeginRepairPut(ctx context.Context, peer identity.Key, sd *object.SignedDescriptor, n uint8, task *wire.RepairTask) (*PutStream, error) {
	return c.beginPut(ctx, peer, sd, n, task)
}

func (c *Client) beginPut(ctx context.Context, peer identity.Key, sd *object.SignedDescriptor, n uint8, task *wire.RepairTask) (*PutStream, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypePut, StorageIndex: sd.Descriptor.StorageIndex, Number: n, Descriptor: sd, Task: task})
	if err != nil {
		return nil, err
	}
	p := &PutStream{st: st, geom: sd.Descriptor.Geometry(), resume: resp.Resume, complete: resp.Complete, next: resp.Resume}
	return p, nil
}

// GetShare fetches the block hashes of a share of an owner's object and
// the blocks of segments [from, to); to of 0 means all. sink receives each
// block in order. Nothing here says the blocks are that owner's: the
// reader checks every block against hashes it took from a descriptor it
// verified.
func (c *Client) GetShare(ctx context.Context, peer identity.Key, owner identity.Key, si object.StorageIndex, n uint8, from, to int, geom object.Geometry, sink func(segment int, block []byte) error) ([]object.Hash, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGet, StorageIndex: si, Owner: owner, Number: n, From: from, To: to})
	if err != nil {
		return nil, err
	}
	defer done(st)
	if to == 0 {
		to = resp.Segments
	}
	for seg := from; seg < to; seg++ {
		b := make([]byte, geom.BlockSize(seg))
		if _, err := io.ReadFull(st, b); err != nil {
			st.CancelRead(1)
			return nil, fmt.Errorf("shareproto: get segment %d: %w", seg, err)
		}
		if err := sink(seg, b); err != nil {
			st.CancelRead(1)
			return nil, err
		}
	}
	return resp.Hashes, nil
}

// GetInfo fetches the signed descriptor and block hash list of a share of
// an owner's object without its blocks: what a reader needs before it
// asks for blocks. The caller verifies the descriptor, its owner included.
func (c *Client) GetInfo(ctx context.Context, peer identity.Key, owner identity.Key, si object.StorageIndex, n uint8) (*object.SignedDescriptor, []object.Hash, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGet, StorageIndex: si, Owner: owner, Number: n, HashesOnly: true})
	if err != nil {
		return nil, nil, err
	}
	done(st)
	return resp.Descriptor, resp.Hashes, nil
}

// PutRoot publishes this device's root to a server.
func (c *Client) PutRoot(ctx context.Context, peer identity.Key, e *RootEnvelope) error {
	st, _, err := c.exchange(ctx, peer, &Request{Type: TypePutRoot, Root: e})
	if err != nil {
		return err
	}
	done(st)
	return nil
}

// Usage asks a server how many share bytes it holds for the asking member.
func (c *Client) Usage(ctx context.Context, peer identity.Key) (int64, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeUsage})
	if err != nil {
		return 0, err
	}
	done(st)
	return resp.Usage, nil
}

// Revoke hands a server a member's signed device revocation: the direct
// path of the RFC's "Losing a device", seconds instead of the next signed
// manifest. The server verifies the record itself; who carries it does
// not matter.
func (c *Client) Revoke(ctx context.Context, peer identity.Key, r *identity.SignedRevocation) error {
	st, _, err := c.exchange(ctx, peer, &Request{Type: TypeRevoke, Revocation: r})
	if err != nil {
		return err
	}
	done(st)
	return nil
}

// GetRoots lists the member's device roots a server holds.
func (c *Client) GetRoots(ctx context.Context, peer identity.Key, member identity.Key) ([]*RootEnvelope, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGetRoots, Member: member})
	if err != nil {
		return nil, err
	}
	done(st)
	return resp.Roots, nil
}

// Delete delivers a delete token to a server; the presence answers what
// the server holds, nil for nothing. A refusal for a recent touch or a
// stale token comes back as a *StatusError with StatusRecent or
// StatusStale.
func (c *Client) Delete(ctx context.Context, peer identity.Key, t *DeleteToken) (*Presence, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeDelete, StorageIndex: t.StorageIndex, Token: t})
	if err != nil {
		return nil, err
	}
	done(st)
	return resp.Presence, nil
}

// Undelete clears this member's tombstone on a server within the window.
func (c *Client) Undelete(ctx context.Context, peer identity.Key, si object.StorageIndex) error {
	st, _, err := c.exchange(ctx, peer, &Request{Type: TypeUndelete, StorageIndex: si})
	if err != nil {
		return err
	}
	done(st)
	return nil
}

// List fetches every object a server holds for this member, page by page.
func (c *Client) List(ctx context.Context, peer identity.Key) ([]ListedObject, error) {
	var out []ListedObject
	var after object.StorageIndex
	for {
		st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeList, After: after})
		if err != nil {
			return nil, err
		}
		done(st)
		out = append(out, resp.Objects...)
		if !resp.More || len(resp.Objects) == 0 {
			return out, nil
		}
		after = resp.Objects[len(resp.Objects)-1].StorageIndex
	}
}
