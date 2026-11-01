// Package mux owns the agent's one UDP socket and hands quic-go several
// net.PacketConn views of it. This is the seed of the path layer RFC 0001
// prices at thousands of lines: every path a QUIC connection may use is a
// view, and a view maps the connection's fixed logical remote address to a
// physical route.
//
//   - The default view receives every UDP packet nobody else claims and every
//     relayed packet from peers nobody else claims. The listener and the hub
//     channel live on it.
//   - A relay view for peer P carries P's packets through the hub channel.
//   - A direct view for peer P writes to P's confirmed UDP address and claims
//     packets coming from it.
//
// A dialer opens the connection on P's relay view, then migrates it to P's
// direct view with quic-go's path API, and back when the direct path dies.
// The logical remote address is the same on both views (wire.RelayAddr), so
// quic-go sees one peer and two paths. The accepting side needs no views: a
// migrated client simply shows up from a new source address.
//
// Punch packets, used to open NAT mappings before QUIC packets flow, are
// intercepted here. They are unauthenticated; only the QUIC handshake proves
// who is at an address.
package mux

import (
	"encoding/binary"
	"errors"
	"log"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/spike/internal/identity"
	"github.com/adefilippo83/redundanet/spike/internal/wire"
)

const punchMagic = "\x00RNPUNCH" // a first byte with the QUIC fixed bit clear

// Punch kinds.
const (
	PunchOpen      byte = 0 // "I am ID, open your mapping toward me"
	PunchAck       byte = 1 // answer to PunchOpen
	PunchKeepalive byte = 2 // liveness probe on an active direct path
	PunchKeepAck   byte = 3 // answer to PunchKeepalive
)

const punchLen = len(punchMagic) + 1 + len(identity.ID{}) + 4

// PunchHandler is told about every punch packet, after the mux has answered it.
type PunchHandler func(from identity.ID, addr *net.UDPAddr, kind byte, seq uint32)

type packet struct {
	b    []byte
	addr net.Addr
}

// RelayFunc sends one packet to a peer through the hub channel.
type RelayFunc func(peer identity.ID, pkt []byte) error

// Mux is the socket owner.
type Mux struct {
	self identity.ID
	udp  *net.UDPConn
	lg   *log.Logger

	mu      sync.RWMutex
	relay   RelayFunc
	byAddr  map[string]*View
	byPeer  map[[8]byte]*View
	peers   map[[8]byte]identity.ID
	def     *View
	onPunch PunchHandler

	closed chan struct{}
	once   sync.Once

	dropped uint64          // packets dropped for a full inbox or a missing relay
	black   map[string]bool // addresses treated as unreachable, for path-loss tests
}

// Blackhole makes addr unreachable both ways, or reachable again: the test
// for a direct path that dies mid-transfer.
func (m *Mux) Blackhole(addr string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.black == nil {
		m.black = map[string]bool{}
	}
	if on {
		m.black[addr] = true
	} else {
		delete(m.black, addr)
	}
}

func (m *Mux) isBlack(addr net.Addr) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.black[addr.String()]
}

// New starts reading from the socket.
func New(self identity.ID, udp *net.UDPConn, lg *log.Logger) *Mux {
	m := &Mux{
		self:   self,
		udp:    udp,
		lg:     lg,
		byAddr: map[string]*View{},
		byPeer: map[[8]byte]*View{},
		peers:  map[[8]byte]identity.ID{},
		closed: make(chan struct{}),
	}
	m.def = m.newView(identity.ID{}, viewDefault)
	go m.readLoop()
	return m
}

// Default is the view for the listener and the hub channel.
func (m *Mux) Default() *View { return m.def }

// SetPunchHandler installs the session layer's callback.
func (m *Mux) SetPunchHandler(h PunchHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPunch = h
}

// SetRelay installs the hub channel sender; nil means the hub is down and
// relayed packets are dropped, as a lossy network would.
func (m *Mux) SetRelay(f RelayFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relay = f
}

// LocalPort is the socket's UDP port.
func (m *Mux) LocalPort() int { return m.udp.LocalAddr().(*net.UDPAddr).Port }

// Dropped counts packets the mux could not deliver or send.
func (m *Mux) Dropped() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dropped
}

// Close closes the socket and every view.
func (m *Mux) Close() error {
	m.once.Do(func() { close(m.closed) })
	return m.udp.Close()
}

// SendPunch writes a punch packet of the given kind straight to the socket.
func (m *Mux) SendPunch(to *net.UDPAddr, kind byte, seq uint32) error {
	if m.isBlack(to) {
		return nil
	}
	b := make([]byte, punchLen)
	copy(b, punchMagic)
	b[len(punchMagic)] = kind
	copy(b[len(punchMagic)+1:], m.self[:])
	binary.BigEndian.PutUint32(b[len(punchMagic)+1+len(m.self):], seq)
	_, err := m.udp.WriteTo(b, to)
	return err
}

// trace logs every relayed packet when SPIKE_TRACE is set.
var trace = os.Getenv("SPIKE_TRACE") != ""

// DeliverRelayed hands a packet that arrived through the hub channel to the
// view that claims the sending peer, or to the default view.
func (m *Mux) DeliverRelayed(from identity.ID, pkt []byte) {
	s := wire.Suffix(from)
	m.mu.Lock()
	m.peers[s] = from
	v := m.byPeer[s]
	m.mu.Unlock()
	if v == nil {
		v = m.def
	}
	if trace {
		m.lg.Printf("TRACE relay in  from %s %d bytes to view %d, inbox %d", from.Short(), len(pkt), v.kind, len(v.inbox))
	}
	v.push(packet{b: pkt, addr: wire.RelayAddr(from)})
}

func (m *Mux) sendRelay(peer identity.ID, pkt []byte) (int, error) {
	m.mu.RLock()
	f := m.relay
	m.mu.RUnlock()
	if trace {
		m.lg.Printf("TRACE relay out to %s %d bytes (relay up: %v)", peer.Short(), len(pkt), f != nil)
	}
	if f == nil {
		m.drop()
		return len(pkt), nil
	}
	if err := f(peer, pkt); err != nil {
		m.drop()
	}
	return len(pkt), nil
}

func (m *Mux) drop() {
	m.mu.Lock()
	m.dropped++
	m.mu.Unlock()
}

func (m *Mux) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := m.udp.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.closed:
			default:
				if !errors.Is(err, net.ErrClosed) {
					m.lg.Printf("mux: udp read: %v", err)
				}
			}
			return
		}
		if m.isBlack(addr) {
			continue
		}
		if n >= punchLen && string(buf[:len(punchMagic)]) == punchMagic {
			m.handlePunch(buf[:n], addr)
			continue
		}
		b := make([]byte, n)
		copy(b, buf[:n])
		m.mu.RLock()
		v := m.byAddr[addr.String()]
		m.mu.RUnlock()
		if v == nil {
			v = m.def
		}
		v.push(packet{b: b, addr: addr})
	}
}

func (m *Mux) handlePunch(b []byte, from *net.UDPAddr) {
	kind := b[len(punchMagic)]
	var id identity.ID
	copy(id[:], b[len(punchMagic)+1:])
	seq := binary.BigEndian.Uint32(b[len(punchMagic)+1+len(id):])
	switch kind {
	case PunchOpen:
		_ = m.SendPunch(from, PunchAck, seq)
	case PunchKeepalive:
		_ = m.SendPunch(from, PunchKeepAck, seq)
	}
	m.mu.RLock()
	h := m.onPunch
	m.mu.RUnlock()
	if h != nil {
		h(id, from, kind, seq)
	}
}

type viewKind int

const (
	viewDefault viewKind = iota
	viewRelay
	viewDirect
)

// View is one net.PacketConn over the shared socket.
type View struct {
	m     *Mux
	kind  viewKind
	peer  identity.ID
	inbox chan packet

	mu     sync.Mutex
	target *net.UDPAddr // direct views: where the logical address goes
	claims []string

	closed chan struct{}
	once   sync.Once
}

func (m *Mux) newView(peer identity.ID, kind viewKind) *View {
	return &View{m: m, kind: kind, peer: peer, inbox: make(chan packet, 1024), closed: make(chan struct{})}
}

// NewRelayView claims peer's relayed packets.
func (m *Mux) NewRelayView(peer identity.ID) *View {
	v := m.newView(peer, viewRelay)
	m.mu.Lock()
	m.byPeer[wire.Suffix(peer)] = v
	m.peers[wire.Suffix(peer)] = peer
	m.mu.Unlock()
	return v
}

// NewDirectView routes peer's logical address to a UDP address set later
// with SetTarget.
func (m *Mux) NewDirectView(peer identity.ID) *View {
	return m.newView(peer, viewDirect)
}

// SetTarget makes addr the physical destination of a direct view and claims
// packets coming from it. Earlier claims stay, so a peer that answers from an
// older address is still heard.
func (v *View) SetTarget(addr *net.UDPAddr) {
	key := addr.String()
	v.mu.Lock()
	v.target = addr
	if !slices.Contains(v.claims, key) {
		v.claims = append(v.claims, key)
	}
	v.mu.Unlock()
	v.m.mu.Lock()
	v.m.byAddr[key] = v
	v.m.mu.Unlock()
}

// Target is the current physical destination of a direct view.
func (v *View) Target() *net.UDPAddr {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.target
}

func (v *View) push(p packet) {
	select {
	case v.inbox <- p:
	default:
		v.m.drop()
	}
}

// ReadFrom implements net.PacketConn.
func (v *View) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case p := <-v.inbox:
		n := copy(b, p.b)
		return n, p.addr, nil
	case <-v.closed:
		return 0, nil, net.ErrClosed
	case <-v.m.closed:
		return 0, nil, net.ErrClosed
	}
}

// WriteTo implements net.PacketConn.
func (v *View) WriteTo(b []byte, addr net.Addr) (int, error) {
	switch v.kind {
	case viewDirect:
		t := v.Target()
		if t == nil || v.m.isBlack(t) {
			v.m.drop()
			return len(b), nil
		}
		return v.m.udp.WriteTo(b, t)
	case viewRelay:
		return v.m.sendRelay(v.peer, b)
	default:
		if s, ok := wire.RelaySuffix(addr); ok {
			v.m.mu.RLock()
			id, known := v.m.peers[s]
			v.m.mu.RUnlock()
			if !known {
				v.m.drop()
				return len(b), nil
			}
			return v.m.sendRelay(id, b)
		}
		if v.m.isBlack(addr) {
			return len(b), nil
		}
		return v.m.udp.WriteTo(b, addr)
	}
}

// Close releases the view's claims; the socket stays open.
func (v *View) Close() error {
	v.once.Do(func() {
		close(v.closed)
		v.m.mu.Lock()
		for _, c := range v.claims {
			if v.m.byAddr[c] == v {
				delete(v.m.byAddr, c)
			}
		}
		if v.kind == viewRelay && v.m.byPeer[wire.Suffix(v.peer)] == v {
			delete(v.m.byPeer, wire.Suffix(v.peer))
		}
		v.m.mu.Unlock()
	})
	return nil
}

// LocalAddr implements net.PacketConn.
func (v *View) LocalAddr() net.Addr { return v.m.udp.LocalAddr() }

// SetDeadline implements net.PacketConn; quic-go does not rely on deadlines.
func (v *View) SetDeadline(time.Time) error { return nil }

// SetReadDeadline implements net.PacketConn.
func (v *View) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline implements net.PacketConn.
func (v *View) SetWriteDeadline(time.Time) error { return nil }
