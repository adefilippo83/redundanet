package transport

import (
	"net"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// relayConn is the net.PacketConn behind relayed connections: packets to a
// peer's relay address go to the hub, packets the hub delivers come back
// with the peer's relay address as their source. One per hub channel; the
// quic-go Transport on top of it hosts the relayed listener and dials.
type relayConn struct {
	local net.Addr
	send  func(peer identity.Key, pkt []byte) error
	inbox chan packet

	mu     sync.RWMutex
	peers  map[[8]byte]identity.Key
	closed chan struct{}
	once   sync.Once

	dropped uint64
}

type packet struct {
	b    []byte
	addr net.Addr
}

// relayInbox bounds packets waiting for quic-go; UDP semantics, so a full
// inbox drops.
const relayInbox = 1024

func newRelayConn(local net.Addr, send func(identity.Key, []byte) error) *relayConn {
	return &relayConn{local: local, send: send, inbox: make(chan packet, relayInbox), peers: map[[8]byte]identity.Key{}, closed: make(chan struct{})}
}

// register makes a peer's relay address routable before anything is sent.
func (r *relayConn) register(k identity.Key) {
	r.mu.Lock()
	r.peers[wire.Suffix(k)] = k
	r.mu.Unlock()
}

// deliver hands a packet the hub relayed to quic-go.
func (r *relayConn) deliver(from identity.Key, pkt []byte) {
	r.register(from)
	select {
	case r.inbox <- packet{b: pkt, addr: wire.RelayAddr(from)}:
	default:
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
	}
}

func (r *relayConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case p := <-r.inbox:
		return copy(b, p.b), p.addr, nil
	case <-r.closed:
		return 0, nil, net.ErrClosed
	}
}

func (r *relayConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	s, ok := wire.RelaySuffix(addr)
	if !ok {
		return 0, net.ErrClosed
	}
	r.mu.RLock()
	k, known := r.peers[s]
	r.mu.RUnlock()
	if !known {
		return len(b), nil // unknown peer: dropped, as a lossy path would
	}
	if err := r.send(k, b); err != nil {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
	}
	return len(b), nil
}

func (r *relayConn) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (r *relayConn) LocalAddr() net.Addr              { return r.local }
func (r *relayConn) SetDeadline(time.Time) error      { return nil }
func (r *relayConn) SetReadDeadline(time.Time) error  { return nil }
func (r *relayConn) SetWriteDeadline(time.Time) error { return nil }

// SetReadBuffer and SetWriteBuffer satisfy quic-go's socket buffer sizing,
// which otherwise logs once that the relay is not a UDP socket. There is no
// kernel buffer behind the relay, so there is nothing to size.
func (r *relayConn) SetReadBuffer(int) error  { return nil }
func (r *relayConn) SetWriteBuffer(int) error { return nil }
