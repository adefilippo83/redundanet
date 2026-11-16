package wire

import (
	"encoding/binary"
	"net/netip"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// The NAT probe. A hub that listens on a second UDP port lets a device
// learn how its router maps and filters, with no setting on the router:
//
//   - the hub knocks: a packet from the second port to the address the
//     device's channel comes from, before the device ever wrote to that
//     port. A device that receives it sits behind a router that lets a
//     port it did not write to in;
//   - the device asks: a packet from its mesh socket to the second port,
//     and the hub tells it the address that packet came from. Compared
//     with the address the channel comes from, that says whether the
//     router gives one public port to every destination.
//
// The packets are not QUIC and not authenticated. Each carries the token
// the hub gave the device in its Welcome, over the authenticated channel:
// the hub answers only a device that has a channel and knows its token,
// with a packet no longer than the one it got, so the port reflects and
// amplifies nothing for a stranger. A forged answer costs a device a
// wrong label on its own status, nothing else.

// ProbeMagic opens every probe packet; the zero byte keeps it apart from
// QUIC, like a punch.
const ProbeMagic = "\x00RN3PRBE"

// Probe kinds.
const (
	ProbeKnock byte = 0 // hub to device, unasked
	ProbeAsk   byte = 1 // device to hub
	ProbeTell  byte = 2 // hub to device: the address the ask came from
)

// ProbeLen is the length of every probe packet: magic, kind, device key,
// token, a 16-byte address and a port.
const ProbeLen = len(ProbeMagic) + 1 + 32 + 8 + 16 + 2

// Probe is one probe packet.
type Probe struct {
	Kind   byte
	Device identity.Key
	Token  uint64
	Addr   netip.AddrPort // ProbeTell only
}

// Encode is the packet's bytes.
func (p Probe) Encode() []byte {
	b := make([]byte, ProbeLen)
	n := copy(b, ProbeMagic)
	b[n] = p.Kind
	n++
	n += copy(b[n:], p.Device[:])
	binary.BigEndian.PutUint64(b[n:], p.Token)
	n += 8
	if p.Addr.IsValid() {
		a := p.Addr.Addr().As16()
		copy(b[n:], a[:])
		binary.BigEndian.PutUint16(b[n+16:], p.Addr.Port())
	}
	return b
}

// ParseProbe reads a probe packet; ok is false for anything else.
func ParseProbe(b []byte) (p Probe, ok bool) {
	if len(b) != ProbeLen || string(b[:len(ProbeMagic)]) != ProbeMagic {
		return p, false
	}
	n := len(ProbeMagic)
	p.Kind = b[n]
	n++
	n += copy(p.Device[:], b[n:n+32])
	p.Token = binary.BigEndian.Uint64(b[n:])
	n += 8
	var a [16]byte
	copy(a[:], b[n:])
	if port := binary.BigEndian.Uint16(b[n+16:]); port != 0 {
		p.Addr = netip.AddrPortFrom(netip.AddrFrom16(a).Unmap(), port)
	}
	return p, p.Kind <= ProbeTell
}

// How a router maps a device's socket to public ports, as the probe saw it.
const (
	MapNone        = "none"        // the public address is the device's own: no translation
	MapIndependent = "independent" // one public port for both hub ports
	MapSequential  = "sequential"  // the next port, or one close after it, per destination
	MapConfined    = "confined"    // an unrelated port per destination, out of a narrow band
	MapRandom      = "random"      // an unrelated port per destination
)

// How a router filters what comes back.
const (
	FilterOpen   = "open"   // a hub port the device never wrote to got in
	FilterStrict = "strict" // it did not
)

// sequentialWithin is how far apart two public ports may be and still
// count as handed out one after the other: other traffic of the same
// household takes ports in between.
const sequentialWithin = 16

// Mapping classifies two public ports of one socket, seen by two
// destinations, with the device's own addresses to tell "no NAT".
func Mapping(channel, probe netip.AddrPort, own func(netip.Addr) bool) string {
	if !channel.IsValid() || !probe.IsValid() {
		return ""
	}
	switch d := int(probe.Port()) - int(channel.Port()); {
	case channel.Addr().Unmap() != probe.Addr().Unmap():
		return MapRandom // another public address per destination
	case d == 0 && own != nil && own(channel.Addr().Unmap()):
		return MapNone
	case d == 0:
		return MapIndependent
	case d >= -sequentialWithin && d <= sequentialWithin:
		return MapSequential
	}
	return MapRandom
}

// confinedWithin is how wide the band of public ports a device was seen
// at may be, over at least confinedSamples of them, and still count as
// narrow: a search of it ends in minutes, one of all the ports never.
const (
	confinedWithin  = 2048
	confinedSamples = 8
)

// Refine corrects the label Mapping gave with what sockets opened one
// after the other showed. Mapping compares the channel's port with the
// probe's, and a channel that has been up for hours got its port long
// ago: a router that hands ports out in order then looks as if it picked
// an unrelated one. The gaps between sockets opened within milliseconds
// do not have that flaw. pooled says those sockets got more than one
// public address; band and samples are the width of the ports seen at
// one address and how many they were.
func Refine(mapping string, gaps []int32, pooled bool, band int32, samples int) string {
	if mapping != MapSequential && mapping != MapRandom {
		return mapping
	}
	if pooled {
		return MapRandom
	}
	if len(gaps) >= 2 {
		inOrder := true
		for _, g := range gaps {
			inOrder = inOrder && g >= 1 && g <= sequentialWithin
		}
		if inOrder {
			return MapSequential
		}
		mapping = MapRandom
	}
	if mapping == MapRandom && samples >= confinedSamples && band > 0 && band <= confinedWithin {
		return MapConfined
	}
	return mapping
}

// CensusNAT is what a device learned about its own router: labels only,
// never an address.
type CensusNAT struct {
	Mapping   string `cbor:"mapping,omitempty" json:"mapping,omitempty"`
	Filtering string `cbor:"filtering,omitempty" json:"filtering,omitempty"`
	IPv6      bool   `cbor:"ipv6,omitempty" json:"ipv6,omitempty"`       // has a global IPv6 address
	PortMap   bool   `cbor:"portmap,omitempty" json:"portmap,omitempty"` // the router granted a port mapping
	// The numbers behind the labels: differences between public ports,
	// never a port. Across is the mesh socket's public port toward the
	// probe port minus the one toward the channel. Gaps are the
	// differences between the public ports the router gave to sockets
	// the device opened one after the other, and Kept says each of those
	// had the socket's own port number outside.
	Across int32   `cbor:"across,omitempty" json:"across,omitempty"`
	Gaps   []int32 `cbor:"gaps,omitempty" json:"gaps,omitempty"`
	Kept   bool    `cbor:"kept,omitempty" json:"kept,omitempty"`
	// Router is who holds the device's public address, from asking the
	// router for its outside address (wire.Router*); empty when not asked.
	Router string `cbor:"router,omitempty" json:"router,omitempty"`
	// Band is the width of the public ports the device has been seen at
	// on its present address, a number of ports and not a port: of up to
	// four ranges together when the ports fall in ranges far apart. Pooled
	// says sockets opened one after the other got more than one public
	// address. PCP says something upstream answered at the port control
	// protocol's well-known address, which a carrier's translator may.
	Band   int32 `cbor:"band,omitempty" json:"band,omitempty"`
	Pooled bool  `cbor:"pooled,omitempty" json:"pooled,omitempty"`
	PCP    bool  `cbor:"pcp,omitempty" json:"pcp,omitempty"`
}

// Who holds the public address the hub sees a device at.
const (
	RouterPublic  = "public"  // the home router: it can be asked for a port
	RouterCarrier = "carrier" // something beyond the home router: a port on the router leads nowhere
	RouterSilent  = "silent"  // the router answered no protocol that tells
)

// MaxPortGaps is how many gaps a device reports and a hub keeps.
const MaxPortGaps = 8

// PortGaps is the difference between each public port and the one before.
func PortGaps(ports []uint16) []int32 {
	if len(ports) < 2 {
		return nil
	}
	gaps := make([]int32, 0, len(ports)-1)
	for i := 1; i < len(ports) && len(gaps) < MaxPortGaps; i++ {
		gaps = append(gaps, int32(ports[i])-int32(ports[i-1]))
	}
	return gaps
}

// Measured is the measurement as a hub shows it: the numbers and words a
// device sent, cut to what a measurement can be, and no label. They come
// from the device, so a hub takes nothing else from them.
func (n *CensusNAT) Measured() CensusNAT {
	if n == nil {
		return CensusNAT{}
	}
	port := func(d int32) int32 { return max(-65535, min(65535, d)) }
	m := CensusNAT{Across: port(n.Across), Band: max(0, port(n.Band)), Pooled: n.Pooled, PCP: n.PCP}
	for i, g := range n.Gaps {
		if i == MaxPortGaps {
			break
		}
		m.Gaps = append(m.Gaps, port(g))
	}
	m.Kept = n.Kept && len(m.Gaps) > 0
	switch n.Router {
	case RouterPublic, RouterCarrier, RouterSilent:
		m.Router = n.Router
	}
	return m
}

// How reachable a device is for a direct connection, from what it
// learned: the one word the status page and the device's own page show.
const (
	ReachOpen        = "open"        // no NAT, a granted port, or a router that lets anyone reach the mapped port
	ReachEasy        = "easy"        // one public port for every destination: a punch finds it
	ReachPredictable = "predictable" // the next port per destination, or one out of a narrow band: it can be searched for
	ReachHard        = "hard"        // an unrelated port per destination: relay unless the other side is open
	ReachUnknown     = "unknown"
)

// Reach is the word for a device's labels. Labels come from the device,
// so anything that is not one of the known ones counts as unknown.
func (n *CensusNAT) Reach() string {
	if n == nil {
		return ReachUnknown
	}
	switch {
	case n.PortMap, n.Mapping == MapNone:
		return ReachOpen
	case n.Mapping == MapIndependent && n.Filtering == FilterOpen:
		return ReachOpen
	case n.Mapping == MapIndependent:
		return ReachEasy
	case n.Mapping == MapSequential, n.Mapping == MapConfined:
		return ReachPredictable
	case n.Mapping == MapRandom:
		return ReachHard
	}
	return ReachUnknown
}
