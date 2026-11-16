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
// Neither works by guessing alone: a household opens connections of its
// own, and every one of them moves the router's counter. So between two
// routers that count, one device opens and the other aims. The first
// writes a run of packets to the ports the other router is about to hand
// out, and reads its own router's counter before and after every few of
// them: it then knows which of its public ports each packet came out of,
// and says so (Opening). The second reads its own counter, which tells it
// the port its next packet will come out of, and writes that packet to
// the port the first device opened toward exactly that one. A router that
// picks each port out of a narrow band cannot aim; its device writes once
// to every port the other opened, and each packet has one chance in the
// band's width of coming out of the port its target points at.
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
	// Ways is how much of the search the sender knows: SeekMeasured from
	// the version that opens and aims. Two devices use the newest way
	// both know.
	Ways uint8 `cbor:"ways,omitempty"`
	// Step is zero in the question and its answer, and Seek* in what
	// follows them under the same Round.
	Step uint8 `cbor:"step,omitempty"`
	// Opened, in a SeekOpened, is what the sender wrote to and which of
	// its own public ports those packets came out of.
	Opened []Opening `cbor:"opened,omitempty"`
	// In a SeekFired, what the sender did with it, for the other side's
	// log: the packets it wrote, how often it read its router's counter,
	// and how far that counter moved between the first and the last
	// reading. Numbers of ports and never a port.
	Sent  uint16 `cbor:"sent,omitempty"`
	Reads uint8  `cbor:"reads,omitempty"`
	Moved uint16 `cbor:"moved,omitempty"`
}

// What follows the question and the answer of a measured search.
const (
	SeekMeasured uint8 = 2 // Ways: the sender opens and aims

	SeekOpened uint8 = 1 // Step: the ports the sender opened
	SeekFired  uint8 = 2 // Step: the sender wrote to them
)

// Opening is one run of packets a device wrote to open the way: Count of
// them, to the receiver's public ports from To on, one each. From is the
// sender's own public port the first one came out of, the next ones after
// it; zero when the sender could not tell. Exact says the sender's router
// gave out exactly Count ports during the run, so every packet's port is
// known and not estimated. Over is how many ports more than Count it gave
// out, to something else in the household: each of them moved the packets
// written after it one port up, and where in the run they fell the sender
// cannot tell. A number of ports and never a port.
type Opening struct {
	To    uint16 `cbor:"to"`
	Count uint16 `cbor:"count"`
	From  uint16 `cbor:"from,omitempty"`
	Exact bool   `cbor:"exact,omitempty"`
	Over  uint8  `cbor:"over,omitempty"`
}

// MaxSeekOver is the most ports over a run may name: past that the run's
// ports are anybody's guess.
const MaxSeekOver = 8

// MaxSeekOpenings is how many runs a device may name.
const MaxSeekOpenings = 128

// Openings cuts a list of runs to what a search may cover: MaxSeekOpenings
// of them, MaxSeekPorts ports in all, none past the last port on either
// router. The list comes from another device, so the hub and the receiver
// both cut it.
func Openings(in []Opening) []Opening {
	var out []Opening
	left := MaxSeekPorts
	for _, o := range in {
		if len(out) == MaxSeekOpenings || left == 0 {
			break
		}
		if o.To == 0 || o.Count == 0 {
			continue
		}
		n := min(int(o.Count), left, 65536-int(o.To))
		if o.From != 0 {
			n = min(n, 65536-int(o.From))
		}
		o.Count = uint16(n)
		o.Exact = o.Exact && o.From != 0
		if o.From == 0 || o.Exact {
			o.Over = 0
		}
		o.Over = min(o.Over, MaxSeekOver)
		out = append(out, o)
		left -= n
	}
	return out
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
