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
// Two devices of this version do not guess where the other router's
// counter will stand: one opens and measures, the other reads its own
// counter and aims (open, fire, try). What each did is told to the other,
// so one device's log shows both halves of a search.
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
	// A measured search. The side that opens writes openPorts packets
	// toward a router that counts, in runs of openRun with a reading of
	// its own router's counter between two runs and a breath of openGap;
	// openKnown is how many ports of a run the router may already have
	// had a mapping to.
	// The side that aims reads its counter up to fireRounds times and
	// writes firePackets after each reading; after every fireRun of them
	// it aims one port further, for what its router gave to something
	// else since the reading. seekStep is how long each waits for the
	// other's next step.
	openPorts   = 160
	openRun     = 16
	openGap     = 5 * time.Millisecond
	openKnown   = 2
	fireRounds  = 6
	firePackets = 12
	fireRun     = 3
	fireWait    = 250 * time.Millisecond
	seekStep    = 12 * time.Second
	// A device whose packets do not get their ports in the order written
	// when they leave together (Endpoint.paced) writes them paceGap
	// apart, the asks that read the counter too. It opens pacedPorts in
	// runs of pacedRun: fewer ports, because each takes that long, and
	// short runs, because one port the router gave to something else
	// leaves the rest of a run estimated.
	paceGap    = 30 * time.Millisecond
	pacedPorts = 96
	pacedRun   = 4
	// A device behind a router that picks asks from bandAsks new sockets
	// where it picks now, and names that with bandMargin ports on either
	// side at least. Toward a router that picks, the side that opens reads
	// its counter after every bandRun packets, and up to bandLast times
	// more, bandWait apart, when the reading after the last one got no
	// answer.
	bandAsks   = 5
	bandMargin = 32
	bandRun    = 64
	bandLast   = 3
	bandWait   = 150 * time.Millisecond
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
	if me, _, _ := s.ep.router(); me != "" && me != wire.MapSequential {
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
	if s.seekBusy {
		return false, time.Second // it says when the next one is due as it ends
	}
	if !s.roleKnown || !s.dialer || !s.online || s.directConn != nil || s.seekNext.IsZero() {
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
	me, _, _ := e.router()
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
	answer := s.listen(mine.Round)
	defer s.unlisten(answer)
	send := func(sk wire.Seek) error { return hub.sendMessage(wire.FrameSeek, sk) }
	if err := send(mine); err != nil {
		s.seekIn(seekSoon, false)
		return
	}
	var theirs wire.Seek
	for timeout := time.After(seekAnswer); !theirs.Reply; {
		select {
		case theirs = <-answer:
		case <-timeout:
			s.seekIn(0, true) // a peer or a hub of an older version, or gone
			return
		case <-s.ctx.Done():
			return
		}
	}
	if theirs.Busy {
		s.seekIn(seekSoon, false)
		return
	}
	if !s.search(me, mine, theirs, false, answer, send) {
		s.seekIn(0, true)
		return
	}
	s.mu.Lock()
	s.seekTries, s.seekNext = 0, time.Time{}
	s.mu.Unlock()
}

// listen has the peer's messages of one search come to the channel it
// returns, until unlisten.
func (s *session) listen(round uint64) chan wire.Seek {
	in := make(chan wire.Seek, 4)
	s.mu.Lock()
	s.seekRound, s.seekReply = round, in
	s.mu.Unlock()
	return in
}

func (s *session) unlisten(in chan wire.Seek) {
	s.mu.Lock()
	if s.seekReply == in {
		s.seekReply = nil
	}
	s.mu.Unlock()
}

// onSeek takes a search message the hub passed on from the peer: the
// answer to a search of this session or a later step of one under way,
// or the peer's own search.
func (s *session) onSeek(c *hubClient, sk wire.Seek) {
	sk.Spans, sk.Opened = wire.Spans(sk.Spans), wire.Openings(sk.Opened)
	s.mu.Lock()
	if sk.Reply || sk.Step != 0 {
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
	me, _, _ := e.router()
	if (me == "" && e.probing()) || !e.seeking.TryLock() {
		answer.Busy = true
		_ = c.sendMessage(wire.FrameSeek, answer)
		return
	}
	defer e.seeking.Unlock()
	in := s.listen(theirs.Round)
	defer s.unlisten(in)
	send := func(sk wire.Seek) error { return c.sendMessage(wire.FrameSeek, sk) }
	mine := s.offer(me, answer, theirs.Mapping)
	if send(mine) != nil {
		return
	}
	s.search(me, mine, theirs, true, in, send)
}

// offer fills in this device's half of a search: its router's label, the
// ports its mapping toward the peer may have, and where its router's
// counter stands when the peer's may hand ports out in order too.
func (s *session) offer(me string, sk wire.Seek, theirs string) wire.Seek {
	sk.Mapping = me
	if !s.ep.cfg.seekOld {
		sk.Ways = wire.SeekMeasured
	}
	sk.Spans = s.spans(me, int(sk.Try))
	if me == wire.MapSequential && (theirs == "" || theirs == wire.MapSequential) {
		if p, ok := s.ep.counter(s.ctx, 0); ok {
			sk.Next = p
		}
	}
	return sk
}

// spans is where this device's mapping toward the peer may be. Behind a
// router that hands ports out in order: around the last marks, twice as
// wide after a failed search, and then everything the router handed out
// before where its counter stands now, as far as a search may go. A
// mapping that is kept alive is older than any mark taken since, and it
// is never under the lowest port this device was seen at, unless the
// counter has come round: then the ports this device was seen at above
// the counter are the ones just before it. Behind a router that picks
// ports out of narrow ranges: the ranges new sockets get their ports in
// at this moment, with a margin that a failed search doubles, and then
// every range the device has been seen in before.
func (s *session) spans(me string, try int) []wire.PortSpan {
	var out []wire.PortSpan
	left := wire.MaxSeekPorts
	add := func(first, count int) {
		last := min(first+count, 65536)
		first = max(first, 1)
		if n := min(last-first, left); n > 0 && len(out) < wire.MaxSeekSpans {
			out = append(out, wire.PortSpan{First: uint16(first), Count: uint16(n)})
			left -= n
		}
	}
	switch me {
	case wire.MapSequential:
		widen := 1 << min(try, 1)
		s.mu.Lock()
		marks := append([]uint16(nil), s.marks...)
		s.mu.Unlock()
		p, ok := s.ep.counter(s.ctx, time.Second)
		seen, known := s.ep.seenAround(p)
		round := ok && known && int(seen.hi) > int(p)+markBefore
		keep := wire.MaxSeekSpans - 1
		if round {
			keep--
		}
		for i := len(marks) - 1; i >= 0 && keep > 0; i, keep = i-1, keep-1 {
			add(int(marks[i])-markBefore*widen, (markBefore+markAfter)*widen)
		}
		if !ok {
			break
		}
		floor := 1
		if known {
			floor = max(1, int(seen.lo)-markAfter)
		}
		first := max(floor, int(p)+markAfter-left)
		if round {
			// Half of what is left for each side of the turn, and to the
			// far side whatever the near one does not need.
			first = max(floor, int(p)+markAfter-left/2)
		}
		add(first, int(p)+markAfter-first)
		if round {
			add(int(seen.hi)+1-left, left)
		}
	case wire.MapConfined:
		// Where the router picks right now comes first, with a margin of
		// half its width since a handful of ports do not show all of it;
		// then the ranges the device was seen in before, where a mapping
		// made earlier may still be.
		widen := 1 << min(try, 1)
		now := s.ep.seenNow(s.ctx, bandAsks)
		for _, r := range now {
			margin := max(bandMargin, (int(r.hi)-int(r.lo))/2) * widen
			add(int(r.lo)-margin, int(r.hi)-int(r.lo)+1+2*margin)
		}
		current := wire.MaxSeekPorts - left
		_, _, ranges := s.ep.router()
	earlier:
		for _, r := range ranges {
			for _, n := range now {
				if n.lo >= r.lo && n.hi <= r.hi {
					continue earlier
				}
			}
			margin := max(16, (int(r.hi)-int(r.lo))/4)
			add(int(r.lo)-margin, int(r.hi)-int(r.lo)+1+2*margin)
		}
		s.mu.Lock()
		s.bandNow = current
		s.mu.Unlock()
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

// after is the n ports after next, the ones a router that counts hands
// out from there on.
func after(next uint16, n int) []uint16 {
	out := make([]uint16, 0, n)
	for p := int(next) + 1; p <= 65535 && len(out) < n; p++ {
		out = append(out, uint16(p))
	}
	return out
}

// open writes one packet to each of the peer's ports, and says which of
// this device's public ports each came out of. Every packet goes to a
// port this device has not written to, so the router gives each the next
// port of its counter; a reading before a run of them and one after tell
// where the run began and whether anything else took a port in between.
// A run the readings cannot place is named without a port, and so is one
// that took fewer ports than it has packets, because the router still
// had mappings to there. One or two fewer are let pass as an estimate:
// the device has written to a port or two of the peer's before, when it
// punched. A run that took more ports than it has packets is named with
// how many more: something else in the household took those.
func (s *session) open(addr netip.Addr, ports []uint16, nonce uint64, direct func() bool, paced, band bool) (runs []wire.Opening, written int) {
	e := s.ep
	pause := func(d time.Duration) {
		select {
		case <-s.ctx.Done():
		case <-time.After(d):
		}
	}
	size, gap := openRun, openGap
	if paced {
		size, gap = pacedRun, paceGap
	}
	readings := []func() (uint16, bool){e.reading(s.ctx)}
	at := []int{0} // toward a band: how many packets were written before each reading
	if paced {
		pause(gap)
	}
	for i := 0; i < len(ports) && s.ctx.Err() == nil && !direct(); {
		j := i + 1
		for j < len(ports) && j-i < size && ports[j] == ports[j-1]+1 {
			j++
		}
		for _, p := range ports[i:j] {
			e.sendPunch(&net.UDPAddr{IP: addr.AsSlice(), Port: int(p)}, punchOpen, nonce)
			written++
			if paced {
				pause(gap)
			}
		}
		runs = append(runs, wire.Opening{To: ports[i], Count: uint16(j - i)})
		if !band {
			readings = append(readings, e.reading(s.ctx))
		} else if written/bandRun > at[len(at)-1]/bandRun {
			readings, at = append(readings, e.reading(s.ctx)), append(at, written)
		}
		i = j
		pause(gap)
	}
	if band {
		// Toward a router that picks, which packet got which port does
		// not matter: every port between two readings is as good as
		// another. A reading every bandRun packets keeps what one lost
		// answer costs small, and so does a counter that comes round to
		// the start of the router's ports between two of them: what lies
		// between those two is left out.
		if at[len(at)-1] != written {
			readings, at = append(readings, e.reading(s.ctx)), append(at, written)
		}
		for more := 0; more < bandLast && s.ctx.Err() == nil; more++ {
			if _, ok := readings[len(readings)-1](); ok {
				break
			}
			pause(bandWait)
			readings, at = append(readings, e.reading(s.ctx)), append(at, written)
		}
		var named []wire.Opening
		before, from := 0, 0
		for i, r := range readings {
			p, ok := r()
			if !ok {
				continue
			}
			if n := int(p) - before - 1; before != 0 && n > 0 && from < len(ports) {
				named = append(named, wire.Opening{To: ports[from], Count: uint16(min(n, wire.MaxSeekPorts)), From: uint16(before + 1)})
			}
			before, from = int(p), at[i]
		}
		return named, written
	}
	for i := range runs {
		before, okBefore := readings[i]()
		behind, okBehind := readings[i+1]()
		n := int(runs[i].Count)
		used := int(behind) - int(before) - 1
		if !okBefore || !okBehind || used < n-openKnown || used < 1 || int(before)+n > 65535 {
			continue
		}
		runs[i].From, runs[i].Exact = before+1, used == n
		if used > n {
			runs[i].Over = uint8(min(used-n, wire.MaxSeekOver))
		}
	}
	return runs, written
}

// openedSum sums up what a device opened: packets, runs, the runs whose
// ports are measured and not estimated, and the ports its router gave to
// something else during the others.
func openedSum(opened []wire.Opening) (packets, runs, exact, over int) {
	for _, o := range opened {
		packets += int(o.Count)
		runs++
		if o.Exact {
			exact++
		}
		over += int(o.Over)
	}
	return packets, runs, exact, over
}

// fire aims at what the peer opened, from behind a router that counts.
// The peer wrote to this router's ports from To on and named the port of
// its own each of those packets came out of. A reading of this router's
// counter says which port the next packet to a new destination takes:
// that packet goes to the peer's port that was opened toward exactly it.
// The reading is some milliseconds old by then, so after every fireRun
// packets the aim moves one port further; then the counter is read
// again. A port the peer could not name is used up on a packet that
// opens nothing, so that the count stays true. Where the peer's router
// gave ports to something else during a run, the packets of that run came
// out some ports higher than the run's first port says, the later ones of
// the run more likely than the first: the aim is moved up by the likely
// number, and by one more or one fewer in the readings after.
func (s *session) fire(addr netip.Addr, opened []wire.Opening, nonce uint64, direct func() bool, paced bool) (sent, reads, moved int) {
	e := s.ep
	last := 0
	for _, o := range opened {
		last = max(last, int(o.To)+int(o.Count))
	}
	toward := func(q, round int) int {
		for _, o := range opened {
			if q >= int(o.To) && q < int(o.To)+int(o.Count) && o.From != 0 {
				return min(int(o.From)+q-int(o.To)+strays(q-int(o.To), int(o.Count), int(o.Over), round), 65535)
			}
		}
		return q // opens nothing, takes a port
	}
	first := 0
	for round := 0; round < fireRounds && !direct(); round++ {
		c, ok := e.counter(s.ctx, 0)
		if !ok {
			break
		}
		reads++
		if first == 0 {
			first = int(c)
		}
		moved = max(0, int(c)-first)
		for n := 0; n < firePackets; n++ {
			q := int(c) + 1 + n + n/fireRun
			if q >= last {
				return sent, reads, moved
			}
			if paced {
				select {
				case <-s.ctx.Done():
					return sent, reads, moved
				case <-time.After(paceGap):
				}
			}
			e.sendPunch(&net.UDPAddr{IP: addr.AsSlice(), Port: toward(q, round)}, punchOpen, nonce)
			sent++
		}
		select {
		case <-s.ctx.Done():
			return sent, reads, moved
		case <-time.After(fireWait):
		}
	}
	return sent, reads, moved
}

// strays is how many ports up the packet at place i of a run of n came
// out, when the router gave over ports to something else during the run:
// the likely number for that place in one round, one more in the next and
// one fewer in the one after, never under none or over all of them.
func strays(i, n, over, round int) int {
	if over <= 0 || n <= 0 {
		return 0
	}
	likely := (over*(2*i+1) + n) / (2 * n) // over*(i+1/2)/n, rounded
	switch round % 3 {
	case 1:
		likely++
	case 2:
		likely--
	}
	return max(0, min(likely, over))
}

// try writes once to every port the peer opened, from behind a router
// that picks each port out of a narrow band. The peer wrote to every port
// of that band, each from a port of its own that it names: a packet to
// one of those opens the way when this router happens to give it the
// port that one was opened toward, one chance in the band's width for
// each packet and about two in three for all of them together.
// The peer opened toward where this router picks now first, and the
// device writes to the first most of the peer's ports and no more, spread
// over them from the first packet on: a router that hands a household a
// small block of ports gives a packet past the block's last free port one
// from another block, which nothing was opened toward, and goes on
// picking there.
func (s *session) try(addr netip.Addr, opened []wire.Opening, nonce uint64, direct func() bool, most int) (sent int) {
	var ports []int
	for _, o := range opened {
		for j := 0; o.From != 0 && j < int(o.Count); j++ {
			ports = append(ports, int(o.From)+j)
		}
	}
	if most > 0 && len(ports) > most {
		ports = ports[:most]
	}
	for _, i := range spread(len(ports)) {
		if direct() {
			return sent
		}
		select {
		case <-s.ctx.Done():
			return sent
		case <-time.After(sprayEvery):
		}
		s.ep.sendPunch(&net.UDPAddr{IP: addr.AsSlice(), Port: ports[i]}, punchOpen, nonce)
		sent++
	}
	return sent
}

// spread is the numbers under n in an order whose every beginning is
// spread evenly over all of them: steps of about n over the golden ratio,
// round and round.
func spread(n int) []int {
	if n <= 0 {
		return nil
	}
	gcd := func(a, b int) int {
		for b != 0 {
			a, b = b, a%b
		}
		return a
	}
	step := max(1, n*618/1000)
	for gcd(step, n) != 1 {
		step++
	}
	out := make([]int, n)
	for i := range out {
		out[i] = i * step % n
	}
	return out
}

// search does this device's half of a search once both halves are known,
// and reports whether a direct connection came of it.
func (s *session) search(me string, mine, theirs wire.Seek, answering bool, in <-chan wire.Seek, send func(wire.Seek) error) bool {
	e := s.ep
	fixed := func(m string) bool { return m == wire.MapIndependent || m == wire.MapNone }
	moving := func(m string) bool { return m == wire.MapSequential || m == wire.MapConfined }
	counts := func(m string) bool { return m == wire.MapSequential }
	measured := mine.Ways >= wire.SeekMeasured && theirs.Ways >= wire.SeekMeasured
	_, public, _ := e.router()
	addr, err := netip.ParseAddr(theirs.Addr)
	addr = addr.Unmap()
	const (
		writes = iota // write to ports, or wait for the peer's packets
		opens         // open ports and say which
		aims          // aim at what the peer opened
		tries         // write to everything the peer opened
	)
	part := writes
	var ports []uint16
	var pace time.Duration
	var how string
	counted, band, paced := false, false, e.paced()
	switch {
	case err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast():
		return false
	case addr == public && !e.cfg.LoopbackCandidates:
		// Behind the same router: the path between the two is the LAN's.
		return false
	case measured && counts(me) && counts(theirs.Mapping) && !answering && theirs.Next != 0:
		part, ports, how = opens, after(theirs.Next, openPorts), "opening the peer's next ports"
		if paced {
			ports, how = ports[:min(len(ports), pacedPorts)], "opening the peer's next ports one by one"
		}
	case measured && counts(me) && counts(theirs.Mapping) && answering && mine.Next != 0:
		part, how = aims, "aiming at the ports the peer opened"
		if paced {
			how = "aiming one by one at the ports the peer opened"
		}
	case measured && counts(me) && theirs.Mapping == wire.MapConfined && len(theirs.Spans) > 0:
		part, ports, how, band = opens, spanPorts(theirs.Spans), "opening the peer's band of ports", true
	case measured && me == wire.MapConfined && counts(theirs.Mapping) && len(mine.Spans) > 0:
		part, how = tries, "trying every port the peer opened"
	case !measured && counts(me) && counts(theirs.Mapping) && mine.Next != 0 && theirs.Next != 0:
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
	// step waits for the peer's next message of this search. It counts
	// only from the address the search began with.
	step := func(want uint8) (wire.Seek, bool) {
		for timeout := time.After(seekStep); ; {
			select {
			case m := <-in:
				if m.Step == want && m.Addr == theirs.Addr {
					return m, true
				}
			case <-timeout:
				return wire.Seek{}, false
			case <-s.ctx.Done():
				return wire.Seek{}, false
			}
		}
	}
	next := wire.Seek{Peer: s.peer, Round: mine.Round, Ways: wire.SeekMeasured}
	nonce, ok := s.newNonce()
	if !ok {
		return false
	}
	sent := 0
	note := ""
	switch part {
	case opens:
		next.Step = wire.SeekOpened
		next.Opened, sent = s.open(addr, ports, nonce, direct, paced && !band, band)
		named, runs, exact, over := openedSum(next.Opened)
		note = fmt.Sprintf(" in %d runs, %d of them measured exactly and %d ports over in the others", runs, exact, over)
		if band {
			note = fmt.Sprintf(", which took %d of this router's ports", named)
		}
		if send(next) != nil {
			note += "; the peer could not be told"
		} else if fired, ok := step(wire.SeekFired); ok {
			note += fmt.Sprintf("; the peer wrote %d packets and read its counter %d times, which moved by %d", fired.Sent, fired.Reads, fired.Moved)
		} else {
			note += "; the peer did not say what it wrote"
		}
	case aims, tries:
		opened, ok := step(wire.SeekOpened)
		if !ok {
			note = "; the peer did not say what it opened"
			break
		}
		next.Step = wire.SeekFired
		if part == aims {
			var reads, moved int
			sent, reads, moved = s.fire(addr, opened.Opened, nonce, direct, paced)
			next.Reads, next.Moved = uint8(min(reads, 255)), uint16(min(moved, 65535))
			note = fmt.Sprintf(", the counter read %d times and moved by %d", reads, moved)
		} else {
			s.mu.Lock()
			most := s.bandNow
			s.mu.Unlock()
			sent = s.try(addr, opened.Opened, nonce, direct, most)
		}
		next.Sent = uint16(min(sent, 65535))
		_ = send(next)
		packets, runs, exact, over := openedSum(opened.Opened)
		note += fmt.Sprintf("; the peer opened %d ports in %d runs, %d of them measured exactly and %d ports over in the others", packets, runs, exact, over)
	default:
		for _, p := range ports {
			if direct() || !wait(pace) {
				break
			}
			e.sendPunch(&net.UDPAddr{IP: addr.AsSlice(), Port: int(p)}, punchOpen, nonce)
			sent++
		}
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
	if counted {
		if behind, ok := e.counter(s.ctx, 0); ok && behind > mine.Next {
			note = fmt.Sprintf(", the router gave out %d ports meanwhile", int(behind)-int(mine.Next)-1)
		}
	}
	s.event("port search by %s: %s, %d packets sent%s", how, result, sent, note)
	return found
}
