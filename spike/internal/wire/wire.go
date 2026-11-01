// Package wire is the hub channel protocol and the relay address encoding.
//
// The channel between a device and the hub is one byte stream (a QUIC stream
// over UDP, or a TLS connection over TCP 443 when UDP is blocked) carrying
// frames: one type byte, a 4-byte big-endian length, the payload. Control
// payloads are JSON for the spike; the product uses CBOR. A relay frame's
// payload is the peer's 32-byte device ID followed by one end-to-end QUIC
// packet, which the hub forwards after rewriting the ID to the sender's.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/spike/internal/identity"
)

// Frame types.
const (
	FrameHello      byte = 1 // device -> hub: Hello
	FrameWelcome    byte = 2 // hub -> device: Welcome
	FrameConnect    byte = 3 // device -> hub: Connect
	FrameCandidates byte = 4 // hub -> device: Candidates (to both parties)
	FrameRelay      byte = 5 // both ways: ID || QUIC packet
	FrameError      byte = 6 // hub -> device: Error
	FramePing       byte = 7
	FramePong       byte = 8
)

// MaxFrame bounds a frame payload; QUIC packets are far smaller.
const MaxFrame = 1 << 16

// Hello announces a device's local address candidates.
type Hello struct {
	Locals []string `json:"locals"`
}

// Welcome tells a device how the hub sees it.
type Welcome struct {
	HubID    string    `json:"hub"`
	Observed string    `json:"observed"` // the source address of the channel as the hub sees it
	Carrier  string    `json:"carrier"`  // "udp" or "tcp"
	Time     time.Time `json:"time"`
}

// Connect asks the hub for a rendezvous with a peer.
type Connect struct {
	Peer string `json:"peer"`
}

// Candidates gives a device the addresses of a peer and the role to take.
type Candidates struct {
	Peer   string   `json:"peer"`
	Addrs  []string `json:"addrs"`  // UDP candidates: observed first, then locals
	Dialer bool     `json:"dialer"` // whether the receiver opens the QUIC connection
	Online bool     `json:"online"` // false when the peer has no hub channel right now
}

// Error carries a hub-side refusal.
type Error struct {
	Msg string `json:"msg"`
}

// Writer serialises frames on one stream.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter wraps a stream.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Frame writes one frame atomically.
func (w *Writer) Frame(typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(payload), MaxFrame)
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.w.Write(buf)
	return err
}

// JSON writes a control frame.
func (w *Writer) JSON(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.Frame(typ, b)
}

// Relay writes a relay frame addressed to (or coming from) peer.
func (w *Writer) Relay(peer identity.ID, pkt []byte) error {
	if len(pkt)+len(peer) > MaxFrame {
		return fmt.Errorf("relay packet of %d bytes exceeds %d", len(pkt), MaxFrame)
	}
	buf := make([]byte, 5+len(peer)+len(pkt))
	buf[0] = FrameRelay
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(peer)+len(pkt)))
	copy(buf[5:], peer[:])
	copy(buf[5+len(peer):], pkt)
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.w.Write(buf)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds %d", n, MaxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// SplitRelay separates the ID from the packet of a relay payload.
func SplitRelay(payload []byte) (identity.ID, []byte, error) {
	var id identity.ID
	if len(payload) < len(id) {
		return id, nil, errors.New("relay frame shorter than a device id")
	}
	copy(id[:], payload[:len(id)])
	return id, payload[len(id):], nil
}

// Relay addresses. A relayed peer is given a synthetic UDP address so that
// quic-go, which addresses packets by net.Addr, can treat the relay like any
// other path: fd72:6564:756e:6461::/64 ("redunda") with the first eight bytes
// of the peer's device ID in the host part, port 1. The multiplexer routes
// packets for such addresses through the hub channel.
var relayPrefix = [8]byte{0xfd, 'r', 'e', 'd', 'u', 'n', 'd', 'a'}

// RelayAddr is the synthetic address of a peer reached through the relay.
func RelayAddr(id identity.ID) *net.UDPAddr {
	ip := make(net.IP, 16)
	copy(ip[:8], relayPrefix[:])
	copy(ip[8:], id[:8])
	return &net.UDPAddr{IP: ip, Port: 1}
}

// RelaySuffix returns the ID bytes carried by a relay address.
func RelaySuffix(a net.Addr) ([8]byte, bool) {
	var s [8]byte
	u, ok := a.(*net.UDPAddr)
	if !ok || len(u.IP) != 16 || [8]byte(u.IP[:8]) != relayPrefix {
		return s, false
	}
	copy(s[:], u.IP[8:])
	return s, true
}

// IsRelayAddr reports whether an address names a relayed peer.
func IsRelayAddr(a net.Addr) bool { _, ok := RelaySuffix(a); return ok }

// Suffix is the relay-address key of an ID.
func Suffix(id identity.ID) [8]byte { return [8]byte(id[:8]) }
