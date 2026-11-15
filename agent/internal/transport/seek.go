package transport

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The port search's device side (wire/seek.go). A session that has had no
// direct connection for a while, on the side that dials, asks the peer
// through the hub how its router maps; the two then write punches to the
// ports where the other's mapping is or is about to be. A punch that gets
// through is answered like any other, and from there the session goes on
// as after an ordinary punch: the address is confirmed and dialled.
//
// Nothing is set on a router. Each search is a bounded number of packets
// as long as a punch, to the one public address the hub sees the peer at.

// seekBackoff is the wait after each search that found nothing.
var seekBackoff = [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

const (
	// seekAfter is how long ordinary punching gets before a search: a LAN
	// neighbour, IPv6 or an open router need none.
	seekAfter = 20 * time.Second
	// seekAnswer is how long the peer has to answer, and seekSettle how
	// long a search waits for the connection after its last packet.
	seekAnswer = 4 * time.Second
	seekSettle = 4 * time.Second
	// When both routers hand ports out in order, each side opens
	// seekPackets new mappings, and the side that answers skips a port
	// after every seekRun of them (inOrder). The side that started writes
	// seekSpare more: the other router may have given some of its ports
	// to something else.
	seekPackets = 96
	seekRun     = 2
	seekSpare   = 16
	seekEvery   = 15 * time.Millisecond
	// sprayEvery paces the packets of a device whose own port does not
	// move: they open nothing new on its router.
	sprayEvery = 4 * time.Millisecond
	// seekSoon is the wait when a search could not start: another one
	// under way here or at the peer, or the probe not finished.
	seekSoon = 10 * time.Second
	// soughtEvery is how often a peer may start a search of this device.
	soughtEvery = 5 * time.Second
	// mappingLife is the silence after which a router may have forgotten
	// a mapping nobody answered.
	mappingLife = 25 * time.Second
	// A session keeps maxMarks marks, and the mapping that began at a
	// mark is looked for from markBefore ports under it to markAfter
	// over it: the reading and the first punch leave together.
	maxMarks   = 3
	markBefore = 32
	markAfter  = 16
)

// seekAfter is how long a session without a direct connection waits
// before it searches.
func (e *Endpoint) seekAfter() time.Duration {
	if e.cfg.seekAfter > 0 {
		return e.cfg.seekAfter
	}
	return seekAfter
}

// mark notes where the router's port counter stands as this session
// begins writing to the peer. Only a router that hands ports out in order
// has one; before the probe has said which kind it is, the reading is
// taken anyway.
func (s *session) mark() {
	if me, _, _, _ := s.ep.router(); me != "" && me != wire.MapSequential {
		return
	}
	p, ok := s.ep.counter(s.ctx, time.Second)
	if !ok {
		return
	}
	s.mu.Lock()
	s.marks = append(s.marks, p)
	if len(s.marks) > maxMarks {
		s.marks = s.marks[len(s.marks)-maxMarks:]
	}
	s.mu.Unlock()
}

// seekDue reports that this session should start a search now, and claims
// it; otherwise in how long one may be due, zero for never.
func (s *session) seekDue() (due bool, in time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seekBusy || !s.roleKnown || !s.dialer || !s.online || s.directConn != nil || s.seekNext.IsZero() {
		return false, 0
	}
	if in = time.Until(s.seekNext); in > 0 {
		return false, in
	}
	s.seekBusy = true
	return true, 0
}

// seekIn puts the next search off: by wait, or after a search that found
// nothing by the next step of the backoff.
func (s *session) seekIn(wait time.Duration, failed bool) {
	s.mu.Lock()
	if failed {
		wait = seekBackoff[min(s.seekTries, len(seekBackoff)-1)]
		s.seekTries++
	}
	s.seekNext = time.Now().Add(wait + rand.N(wait/4+1))
	s.mu.Unlock()
}

// seek is one port search, started by the side that dials.
func (s *session) seek() {
	e := s.ep
	defer func() { s.mu.Lock(); s.seekBusy = false; s.mu.Unlock() }()
	if !e.seeking.TryLock() {
		s.seekIn(seekSoon, false)
		return
	}
	defer e.seeking.Unlock()
	s.mu.Lock()
	hub, try := s.hub, s.seekTries
	s.mu.Unlock()
	me, _, _, _ := e.router()
	if me == "" && e.probing() {
		s.seekIn(seekSoon, false)
		return
	}
	if me == "" || hub == nil {
		// No probe on this hub or over this carrier: nothing to search with.
		s.seekIn(0, true)
		return
	}
	var raw [8]byte
	if _, err := crand.Read(raw[:]); err != nil {
		s.seekIn(seekSoon, false)
		return
	}
	mine := s.offer(me, wire.Seek{Peer: s.peer, Round: binary.BigEndian.Uint64(raw[:]), Try: uint8(min(try, 255))}, "")
	answer := make(chan wire.Seek, 1)
	s.mu.Lock()
	s.seekRound, s.seekReply = mine.Round, answer
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.seekReply = nil; s.mu.Unlock() }()
	if err := hub.sendMessage(wire.FrameSeek, mine); err != nil {
		s.seekIn(seekSoon, false)
		return
	}
	var theirs wire.Seek
	select {
	case theirs = <-answer:
	case <-time.After(seekAnswer):
		s.seekIn(0, true) // a peer or a hub of an older version, or gone
		return
	case <-s.ctx.Done():
		return
	}
	if theirs.Busy {
		s.seekIn(seekSoon, false)
		return
	}
	if !s.search(me, mine, theirs, false) {
		s.seekIn(0, true)
		return
	}
	s.mu.Lock()
	s.seekTries, s.seekNext = 0, time.Time{}
	s.mu.Unlock()
}

// onSeek takes a search message the hub passed on from the peer: the
// answer to a search of this session, or the peer's own search.
func (s *session) onSeek(c *hubClient, sk wire.Seek) {
	sk.Spans = wire.Spans(sk.Spans)
	s.mu.Lock()
	if sk.Reply {
		answer := s.seekReply
		if s.seekRound != sk.Round {
			answer = nil
		}
		s.mu.Unlock()
		if answer != nil {
			select {
			case answer <- sk:
			default:
			}
		}
		return
	}
	if s.directConn != nil || time.Since(s.soughtAt) < soughtEvery {
		s.mu.Unlock()
		return
	}
	s.soughtAt = time.Now()
	s.mu.Unlock()
	go s.sought(c, sk)
}

// sought answers the peer's search and does this device's half of it.
func (s *session) sought(c *hubClient, theirs wire.Seek) {
	e := s.ep
	answer := wire.Seek{Peer: s.peer, Round: theirs.Round, Reply: true, Try: theirs.Try}
	me, _, _, _ := e.router()
	if (me == "" && e.probing()) || !e.seeking.TryLock() {
		answer.Busy = true
		_ = c.sendMessage(wire.FrameSeek, answer)
		return
	}
	defer e.seeking.Unlock()
	mine := s.offer(me, answer, theirs.Mapping)
	if c.sendMessage(wire.FrameSeek, mine) != nil {
		return
	}
	s.search(me, mine, theirs, true)
}

// offer fills in this device's half of a search: its router's label, the
// ports its mapping toward the peer may have, and where its router's
// counter stands when the peer's may hand ports out in order too.
func (s *session) offer(me string, sk wire.Seek, theirs string) wire.Seek {
	sk.Mapping = me
	sk.Spans = s.spans(me, int(sk.Try))
	if me == wire.MapSequential && (theirs == "" || theirs == wire.MapSequential) {
		if p, ok := s.ep.counter(s.ctx, 0); ok {
			sk.Next = p
		}
	}
	return sk
}

// spans is where this device's mapping toward the peer may be: around
// each mark behind a router that hands ports out in order, the band of
// ports the device has been seen at behind one that picks them out of a
// narrow band. Each failed search before this one doubles the width, up
// to eight times.
func (s *session) spans(me string, try int) []wire.PortSpan {
	widen := 1 << min(try, 3)
	var out []wire.PortSpan
	add := func(first, count int) {
		last := min(first+count, 65536)
		first = max(first, 1)
		if last > first {
			out = append(out, wire.PortSpan{First: uint16(first), Count: uint16(min(last-first, 65535))})
		}
	}
	switch me {
	case wire.MapSequential:
		s.mu.Lock()
		marks := append([]uint16(nil), s.marks...)
		s.mu.Unlock()
		for i := len(marks) - 1; i >= 0; i-- {
			add(int(marks[i])-markBefore*widen, (markBefore+markAfter)*widen)
		}
		if len(marks) == 0 {
			// No mark was taken: the mapping is somewhere under where
			// the counter stands now.
			if p, ok := s.ep.counter(s.ctx, time.Second); ok {
				add(int(p)-4*markBefore*widen, 4*markBefore*widen+markAfter)
			}
		}
	case wire.MapConfined:
		if _, _, lo, hi := s.ep.router(); hi != 0 {
			margin := max(16, (int(hi)-int(lo))/4) * widen
			add(int(lo)-margin, int(hi)-int(lo)+1+2*margin)
		}
	}
	return wire.Spans(out)
}

// inOrder is the ports a device writes to when both routers hand ports
// out in order. next is a port the peer's router just gave: its next
// mappings get the ports after it, one for each packet the peer sends to
// a port it has not written to before, and this device's own mappings
// come about the same way. The side that started writes to those ports
// one by one. The side that answers does too, and skips one port after
// every seekRun packets.
//
// A packet opens the way only when it lands on the peer's mapping that
// points back at the mapping the packet came out of. With nothing else
// on either router that is every packet, the first one included. Every
// port a router gave to something else, since it said where its counter
// stood, moves that pairing by one, and the skipped ports move it back:
// the skips add up faster than a household opens connections, so the
// two meet, and stay met for seekRun packets.
func inOrder(next uint16, answering bool) []uint16 {
	n := seekPackets
	if !answering {
		n += seekSpare
	}
	out := make([]uint16, 0, n)
	for i := 1; i <= n; i++ {
		p := int(next) + i
		if answering {
			p += (i - 1) / seekRun
		}
		if p > 65535 {
			break
		}
		out = append(out, uint16(p))
	}
	return out
}

func spanPorts(spans []wire.PortSpan) []uint16 {
	var out []uint16
	for _, sp := range wire.Spans(spans) {
		for i := 0; i < int(sp.Count); i++ {
			out = append(out, uint16(int(sp.First)+i))
		}
	}
	return out
}

// search does this device's half of a search once both halves are known,
// and reports whether a direct connection came of it.
func (s *session) search(me string, mine, theirs wire.Seek, answering bool) bool {
	e := s.ep
	fixed := func(m string) bool { return m == wire.MapIndependent || m == wire.MapNone }
	moving := func(m string) bool { return m == wire.MapSequential || m == wire.MapConfined }
	_, public, _, _ := e.router()
	addr, err := netip.ParseAddr(theirs.Addr)
	addr = addr.Unmap()
	var ports []uint16
	var pace time.Duration
	var how string
	counted := false
	switch {
	case err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast():
		return false
	case addr == public && !e.cfg.LoopbackCandidates:
		// Behind the same router: the path between the two is the LAN's.
		return false
	case me == wire.MapSequential && theirs.Mapping == wire.MapSequential && mine.Next != 0 && theirs.Next != 0:
		ports, pace, how, counted = inOrder(theirs.Next, answering), seekEvery, "both routers' next ports", true
	case fixed(me) && moving(theirs.Mapping) && len(theirs.Spans) > 0:
		ports, pace, how = spanPorts(theirs.Spans), sprayEvery, "the peer's ports"
	case moving(me) && fixed(theirs.Mapping) && len(mine.Spans) > 0:
		how = "this device's ports" // the peer writes; its packets are answered as they come
	default:
		s.event("port search: no way between a router that is %s and one that is %s", orUnknown(me), orUnknown(theirs.Mapping))
		return false
	}
	direct := func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.directConn != nil }
	wait := func(d time.Duration) bool {
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	nonce, ok := s.newNonce()
	if !ok {
		return false
	}
	sent := 0
	for _, p := range ports {
		if direct() || !wait(pace) {
			break
		}
		e.sendPunch(&net.UDPAddr{IP: addr.AsSlice(), Port: int(p)}, punchOpen, nonce)
		sent++
	}
	for until := time.Now().Add(seekSettle); !direct() && time.Now().Before(until); {
		if !wait(100 * time.Millisecond) {
			return false
		}
	}
	found := direct()
	result := "no path"
	if found {
		result = "direct"
	}
	// How many ports the router handed out while this device wrote says
	// whether the search's picture of it holds: one for each packet.
	used := ""
	if counted {
		if after, ok := e.counter(s.ctx, 0); ok && after > mine.Next {
			used = fmt.Sprintf(", the router gave out %d ports meanwhile", int(after)-int(mine.Next)-1)
		}
	}
	s.event("port search by %s: %s, %d packets sent%s", how, result, sent, used)
	return found
}
