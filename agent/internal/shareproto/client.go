package shareproto

import (
	"context"
	"fmt"
	"io"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
)

// Opener opens a stream to a peer; the endpoint's OpenStream fits.
type Opener func(ctx context.Context, peer identity.Key) (*quic.Stream, string, error)

// Client speaks the protocol to storage servers.
type Client struct{ open Opener }

// NewClient wraps an opener.
func NewClient(open Opener) *Client { return &Client{open: open} }

// done releases a stream whose last byte has been read: the read half is
// cancelled with code 0 and the write half closed, so quic-go frees the
// stream on both ends and the server's stream limit gets its credit back.
// Without it a client leaks one half-open stream per request.
func done(st *quic.Stream) {
	st.CancelRead(0)
	_ = st.Close()
}

func (c *Client) exchange(ctx context.Context, peer identity.Key, req *Request) (*quic.Stream, *Response, error) {
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

// Has asks what a server holds of an object; nil when nothing.
func (c *Client) Has(ctx context.Context, peer identity.Key, si object.StorageIndex) (*Presence, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeHas, StorageIndex: si})
	if err != nil {
		return nil, err
	}
	done(st)
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
	st       *quic.Stream
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
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypePut, StorageIndex: sd.Descriptor.StorageIndex, Number: n, Descriptor: sd})
	if err != nil {
		return nil, err
	}
	p := &PutStream{st: st, geom: sd.Descriptor.Geometry(), resume: resp.Resume, complete: resp.Complete, next: resp.Resume}
	return p, nil
}

// GetShare fetches a share's block hashes and the blocks of segments
// [from, to); to of 0 means all. sink receives each block in order.
func (c *Client) GetShare(ctx context.Context, peer identity.Key, si object.StorageIndex, n uint8, from, to int, geom object.Geometry, sink func(segment int, block []byte) error) ([]object.Hash, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGet, StorageIndex: si, Number: n, From: from, To: to})
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

// GetInfo fetches a share's signed descriptor and block hash list without
// its blocks: what a reader needs before it asks for blocks.
func (c *Client) GetInfo(ctx context.Context, peer identity.Key, si object.StorageIndex, n uint8) (*object.SignedDescriptor, []object.Hash, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGet, StorageIndex: si, Number: n, HashesOnly: true})
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

// GetRoots lists the member's device roots a server holds.
func (c *Client) GetRoots(ctx context.Context, peer identity.Key, member identity.Key) ([]*RootEnvelope, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeGetRoots, Member: member})
	if err != nil {
		return nil, err
	}
	done(st)
	return resp.Roots, nil
}
