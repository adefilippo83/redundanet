package wire

import (
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// The port search. Two devices whose routers both let in only the port
// they wrote to never meet by punching at the address the hub saw: a
// router that gives another public port per destination has a port toward
// the peer that nobody was ever told. With what the NAT probe measured
// (probe.go) that port can be found, and the two devices agree on how
// through the hub:
//
//   - a router that keeps one public port (independent) has nothing to
//     search for on its own side. Its device writes to every port where
//     the peer's mapping toward it may be: the peer names those ports;
//   - two routers that hand ports out in order each say where their
//     counter stands, and each device then writes to the ports the other
//     router is about to hand out, so that both open a run of new
//     mappings that point at each other.
//
// A Seek carries ports of the sender's own public address and nothing
// else. The hub sets the address from the sender's channel, so a device
// can ask a peer to write only to the address the hub sees the device
// itself at, a bounded number of packets no longer than a punch.

// Seek is one side's half of a port search, passed on by the hub.
type Seek struct {
	// Peer is whom the message is for on the way to the hub, and whom it
	// is from on the way to a device.
	Peer  identity.Key `cbor:"peer"`
	Round uint64       `cbor:"round"`           // ties an answer to its question
	Reply bool         `cbor:"reply,omitempty"` // the answer
	Busy  bool         `cbor:"busy,omitempty"`  // answered, and not now: another search is under way
	Try   uint8        `cbor:"try,omitempty"`   // how many searches of this pair failed before this one
	// Addr is the sender's public address as the hub sees it, set by the
	// hub; what a device put there is thrown away.
	Addr string `cbor:"addr,omitempty"`
	// Mapping is the sender's label for its router (Map*).
	Mapping string `cbor:"mapping,omitempty"`
	// Next is a public port the sender's router handed out a moment ago,
	// when it hands them out in order: the next ones follow it.
	Next uint16 `cbor:"next,omitempty"`
	// Spans are the public ports the sender's mapping toward the receiver
	// may have.
	Spans []PortSpan `cbor:"spans,omitempty"`
}

// PortSpan is Count ports from First on.
type PortSpan struct {
	First uint16 `cbor:"first"`
	Count uint16 `cbor:"count"`
}

// What a Seek may ask a device to write to.
const (
	MaxSeekSpans = 4
	MaxSeekPorts = 1024
)

// Spans cuts a list of spans to what a search may cover: at most
// MaxSeekSpans of them and MaxSeekPorts ports in all, none past the last
// port. The list comes from another device, so the hub and the receiver
// both cut it.
func Spans(in []PortSpan) []PortSpan {
	var out []PortSpan
	left := MaxSeekPorts
	for _, s := range in {
		if len(out) == MaxSeekSpans || left == 0 {
			break
		}
		if s.First == 0 || s.Count == 0 {
			continue
		}
		n := min(int(s.Count), left, 65536-int(s.First))
		out = append(out, PortSpan{First: s.First, Count: uint16(n)})
		left -= n
	}
	return out
}

// Allowance is a count of things a peer may ask for that fills up again
// with time: Burst at once, one more every Every.
type Allowance struct {
	Burst int
	Every time.Duration

	left float64
	at   time.Time
}

// Take reports whether one more is allowed at now, and counts it.
func (a *Allowance) Take(now time.Time) bool {
	if a.at.IsZero() {
		a.left = float64(a.Burst)
	} else if a.Every > 0 {
		a.left = min(float64(a.Burst), a.left+float64(now.Sub(a.at))/float64(a.Every))
	}
	a.at = now
	if a.left < 1 {
		return false
	}
	a.left--
	return true
}
