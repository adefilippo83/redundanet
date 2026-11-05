// Package wire is the hub channel protocol: the frames a device and a hub
// exchange over one stream (a QUIC stream, or a TLS connection over TCP 443
// when UDP is blocked), and the synthetic address that lets quic-go treat
// the relay as a path.
//
// Frames are one type byte, a 4-byte big-endian length and a payload.
// Control payloads are deterministic CBOR; a relay payload is the peer's
// 32-byte device key followed by one end-to-end QUIC packet. Over a QUIC
// channel relay payloads travel as datagrams instead, with the same layout.
//
// A receiver skips control fields it does not know (identity.UnmarshalLenient),
// so a device and a hub may run different versions: adding a field to a
// message is compatible both ways, and only a new frame type or a changed
// meaning needs a protocol version.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Frame types.
const (
	FrameHello      byte = 1 // device to hub: Hello
	FrameWelcome    byte = 2 // hub to device: Welcome
	FrameConnect    byte = 3 // device to hub: Connect
	FrameCandidates byte = 4 // hub to both devices: Candidates
	FrameRelay      byte = 5 // both ways: device key || QUIC packet (TCP carrier only)
	FrameError      byte = 6 // hub to device: Error
	FramePing       byte = 7
	FramePong       byte = 8
	FrameCensus     byte = 9  // device to hub: Census
	FrameShares     byte = 10 // device to hub: ShareCensus, chunked
	FrameTask       byte = 11 // hub to device: RepairTask, signed by the hub
	FrameTaskReport byte = 12 // device to hub: TaskReport
	FrameRelease    byte = 13 // hub to device: ReleaseHint
)

// MaxFrame bounds a frame payload.
const MaxFrame = 1 << 16

// Hello announces a device's own address candidates.
type Hello struct {
	Locals []string `cbor:"locals"`
}

// Welcome tells a device how the hub sees it.
type Welcome struct {
	Observed string    `cbor:"observed"` // the channel's source address as the hub sees it
	Carrier  string    `cbor:"carrier"`  // "udp" or "tcp"
	Time     time.Time `cbor:"time"`
}

// Connect asks for a rendezvous with a peer.
type Connect struct {
	Peer identity.Key `cbor:"peer"`
}

// Candidates gives a device a peer's addresses and the role to take.
type Candidates struct {
	Peer   identity.Key `cbor:"peer"`
	Addrs  []string     `cbor:"addrs"`  // UDP candidates, observed first
	Dialer bool         `cbor:"dialer"` // whether the receiver opens the connection
	Online bool         `cbor:"online"` // false when the peer has no channel right now
}

// Census is a device's heartbeat to the hub: what the status page may show
// about it. It carries no address and no path; the hub sees the channel's
// source address on its own and keeps it to itself.
type Census struct {
	Version  string         `cbor:"version" json:"version"`
	Uptime   uint64         `cbor:"uptime" json:"uptime"` // seconds
	OS       string         `cbor:"os" json:"os"`         // GOOS/GOARCH
	Roles    []string       `cbor:"roles" json:"roles"`
	Manifest uint64         `cbor:"manifest" json:"manifest"` // the revision the device applied
	Storage  *CensusStorage `cbor:"storage,omitempty" json:"storage,omitempty"`
	Peers    []CensusPeer   `cbor:"peers" json:"peers"`
	Update   string         `cbor:"update,omitempty" json:"update,omitempty"` // the self-updater's last word
	Backup   *CensusBackup  `cbor:"backup,omitempty" json:"backup,omitempty"`
	Worker   bool           `cbor:"worker,omitempty" json:"worker,omitempty"` // volunteers as a repair worker
}

// CensusBackup is what a device says about its backups: dates only, the
// public projection the RFC allows, at day granularity on the page.
type CensusBackup struct {
	LastBackup time.Time `cbor:"last_backup" json:"last_backup"`
	Partial    bool      `cbor:"partial" json:"partial"`
	LastVerify time.Time `cbor:"last_verify" json:"last_verify"`
	VerifyOK   bool      `cbor:"verify_ok" json:"verify_ok"`
	// VerifyStreak is how many weeks in a row, counting back from this
	// one, had a passing restore test: what the G2 gate's month counts.
	VerifyStreak int `cbor:"verify_streak,omitempty" json:"verify_streak,omitempty"`
}

// CensusStorage is the storage half of a heartbeat.
type CensusStorage struct {
	FreeBytes  uint64 `cbor:"free" json:"free_bytes"`
	TotalBytes uint64 `cbor:"total" json:"total_bytes"`
	State      string `cbor:"state" json:"state"` // "ok", or why the store is not serving
	Shares     uint64 `cbor:"shares,omitempty" json:"shares,omitempty"`
	Bytes      uint64 `cbor:"bytes,omitempty" json:"bytes,omitempty"` // bytes of complete shares as accounted
	// Verified is the oldest integrity check among the complete shares:
	// every share was read back and checked since then. Zero while any
	// share awaits its first check.
	Verified time.Time `cbor:"verified" json:"verified"`
	Floor    uint64    `cbor:"floor,omitempty" json:"floor,omitempty"` // free bytes below which the node refuses writes
}

// ShareCensus is a storage node's share census since a generation, in
// chunks that fit a frame; More says another chunk follows. This is the
// delta protocol of RFC 0001's Health section.
type ShareCensus struct {
	Since      uint64           `cbor:"since" json:"since"`
	Generation uint64           `cbor:"generation" json:"generation"`
	Added      []ShareEntry     `cbor:"added" json:"added"`
	Removed    []ShareKey       `cbor:"removed" json:"removed"`
	Tombstones []TombstoneEntry `cbor:"tombstones" json:"tombstones"`
	More       bool             `cbor:"more" json:"more"`
}

// ShareEntry is one complete share.
type ShareEntry struct {
	StorageIndex   [16]byte     `cbor:"si" json:"si"`
	Number         uint8        `cbor:"n" json:"n"`
	Owner          identity.Key `cbor:"owner" json:"owner"`
	Size           int64        `cbor:"size" json:"size"`
	K              uint8        `cbor:"k" json:"k"`
	N              uint8        `cbor:"nn" json:"nn"`
	DescriptorHash [32]byte     `cbor:"dhash" json:"dhash"`
}

// ShareKey names a share that went away.
type ShareKey struct {
	StorageIndex [16]byte `cbor:"si" json:"si"`
	Number       uint8    `cbor:"n" json:"n"`
}

// TombstoneEntry is a deleted object a node still records.
type TombstoneEntry struct {
	StorageIndex   [16]byte     `cbor:"si" json:"si"`
	Owner          identity.Key `cbor:"owner" json:"owner"`
	DescriptorHash [32]byte     `cbor:"dhash" json:"dhash"`
	Purged         bool         `cbor:"purged" json:"purged"`
}

// ShareChunk bounds entries per frame so a chunk fits MaxFrame.
const ShareChunk = 400

// CensusPeer is one session the device holds.
type CensusPeer struct {
	Device identity.Key `cbor:"device" json:"device"`
	Direct bool         `cbor:"direct" json:"direct"`
	Relay  bool         `cbor:"relay" json:"relay"`
}

// Error carries a hub-side refusal.
type Error struct {
	Msg string `cbor:"msg"`
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

// Message writes a control frame.
func (w *Writer) Message(typ byte, v any) error {
	b, err := identity.Marshal(v)
	if err != nil {
		return err
	}
	return w.Frame(typ, b)
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

// RelayPayload joins a device key and a packet.
func RelayPayload(peer identity.Key, pkt []byte) []byte {
	b := make([]byte, len(peer)+len(pkt))
	copy(b, peer[:])
	copy(b[len(peer):], pkt)
	return b
}

// SplitRelay separates the key from the packet.
func SplitRelay(payload []byte) (identity.Key, []byte, error) {
	var k identity.Key
	if len(payload) < len(k) {
		return k, nil, errors.New("relay payload shorter than a device key")
	}
	copy(k[:], payload[:len(k)])
	return k, payload[len(k):], nil
}

// Relay addresses: a relayed peer is given a synthetic UDP address,
// fd72:6564:756e:6461::/64 ("redunda") with the first eight bytes of its
// device key in the host part, port 1, so quic-go addresses it like any peer
// and the relay connection routes it through the hub.
var relayPrefix = [8]byte{0xfd, 'r', 'e', 'd', 'u', 'n', 'd', 'a'}

// RelayAddr is the synthetic address of a relayed peer.
func RelayAddr(k identity.Key) *net.UDPAddr {
	ip := make(net.IP, 16)
	copy(ip[:8], relayPrefix[:])
	copy(ip[8:], k[:8])
	return &net.UDPAddr{IP: ip, Port: 1}
}

// RelaySuffix returns the key bytes carried by a relay address.
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

// Suffix is the relay-address key of a device key.
func Suffix(k identity.Key) [8]byte { return [8]byte(k[:8]) }
