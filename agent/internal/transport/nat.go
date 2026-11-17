package transport

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The NAT probe's device side (wire/probe.go). On each hub channel over
// UDP whose hub offers a second port, the device first listens for the
// hub's knock, then asks from its mesh socket and learns the address the
// ask came from. Nothing here changes how the device connects: it is what
// the device, its member and the status page know about its router.

const (
	// natKnockWait is how long the device listens before it writes to
	// the probe port: a knock after that proves nothing, since by then
	// the device's own packet opened the way.
	natKnockWait = 1200 * time.Millisecond
	natAsks      = 5
	natAskEvery  = 400 * time.Millisecond
	// After the labels, the device opens natFresh sockets one after the
	// other and asks from each: the public ports the router hands to new
	// sockets show whether the next one can be told in advance.
	natFresh     = 4
	natFreshWait = 500 * time.Millisecond
	// Behind a router that hands ports out in order, natBurst sockets ask
	// at the same moment, natBursts times: the ports they get say whether
	// packets written together reach the router in the order written.
	natBurst  = 4
	natBursts = 2
	// natRouterEvery is how often the router is asked for its outside
	// address: channels come and go more often than routers change.
	natRouterEvery = time.Hour
)

type natState struct {
	mu      sync.Mutex
	run     uint64         // the probe run under way, by its token
	hub     netip.AddrPort // the probe port
	channel netip.AddrPort // the channel's source address as the hub sees it
	asked   bool
	knocked bool
	seen    netip.AddrPort // the ask's source address as the hub sees it
	// The last finished run's labels; they stay while a new run is under
	// way, so a reconnect does not blank the page.
	measurement
	ports  portBand
	router string
	pcp    bool
	// What the router last said its outside address is, and when.
	routerAddr  netip.Addr
	routerPCP   bool
	routerAsked time.Time
	// The last reading of the router's port counter (counter), kept for
	// a moment so that sessions that start together share one.
	readMu   sync.Mutex // one reading at a time
	readAt   time.Time
	readPort uint16
	// The public address new sockets came out at when it was not the
	// channel's, and how many readings in a row said so: see sameAddress.
	elseAddr netip.Addr
	elseSeen int
}

// measurement is what one finished probe run found.
type measurement struct {
	mapping, filtering string
	checked            time.Time
	across             int32
	gaps               []int32
	kept, pooled       bool
	band               int32
	// unordered says packets this device writes within milliseconds of
	// each other do not get their ports in the order written.
	unordered bool
}

// NATStatus is what the device knows about its router.
type NATStatus struct {
	wire.CensusNAT
	Reach   string    `json:"reach"`
	Checked time.Time `json:"checked,omitempty"`
}

// startProbe begins a probe run for a channel that just came up.
func (e *Endpoint) startProbe(ctx context.Context, hub net.Addr, w wire.Welcome) {
	udp, ok := hub.(*net.UDPAddr)
	channel, err := netip.ParseAddrPort(w.Observed)
	if !ok || err != nil || w.Probe == 0 || w.ProbeToken == 0 {
		return
	}
	n := &e.nat
	n.mu.Lock()
	n.run, n.hub, n.channel = w.ProbeToken, netip.AddrPortFrom(udp.AddrPort().Addr().Unmap(), w.Probe), channel
	n.asked, n.knocked, n.seen = false, false, netip.AddrPort{}
	token, to := n.run, net.UDPAddrFromAddrPort(n.hub)
	n.mu.Unlock()
	go func() {
		current := func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.run == token }
		select {
		case <-ctx.Done():
			return
		case <-time.After(natKnockWait):
		}
		n.mu.Lock()
		if n.run == token {
			n.asked = true
		}
		n.mu.Unlock()
		ask := wire.Probe{Kind: wire.ProbeAsk, Device: e.cfg.Device, Token: token}.Encode()
		for i := 0; i < natAsks && current(); i++ {
			n.mu.Lock()
			told := n.seen.IsValid()
			n.mu.Unlock()
			if told {
				break
			}
			_, _ = e.tr.WriteTo(ask, to)
			select {
			case <-ctx.Done():
				return
			case <-time.After(natAskEvery):
			}
		}
		n.mu.Lock()
		if n.run != token {
			n.mu.Unlock()
			return
		}
		hub, channel, seen, knocked := n.hub, n.channel, n.seen, n.knocked
		n.mu.Unlock()
		// Sockets opened now, within milliseconds of each other, say how
		// the router hands ports out; with no answer on the mesh socket,
		// or no router in between, there is nothing to open them for.
		m := measurement{checked: time.Now()}
		var fresh []netip.AddrPort
		if seen.IsValid() {
			m.mapping, m.filtering = wire.Mapping(channel, seen, ownAddr), wire.FilterStrict
			if seen.Addr().Unmap() == channel.Addr().Unmap() {
				m.across = int32(seen.Port()) - int32(channel.Port())
			}
			if m.mapping != wire.MapNone {
				fresh, m.kept = freshPorts(ctx, hub, ask, e.freshSocket)
			}
		}
		if knocked {
			m.filtering = wire.FilterOpen
		}
		ports := make([]uint16, 0, len(fresh))
		for _, a := range fresh {
			ports = append(ports, a.Port())
			m.pooled = m.pooled || a.Addr().Unmap() != channel.Addr().Unmap()
		}
		m.pooled = m.pooled || (seen.IsValid() && seen.Addr().Unmap() != channel.Addr().Unmap())
		m.gaps, m.kept = wire.PortGaps(ports), m.kept && len(ports) > 1
		n.mu.Lock()
		if n.run != token {
			n.mu.Unlock()
			return
		}
		band, samples := n.ports.add(channel.Addr().Unmap(), append(append(fresh, channel), seen)...)
		label := wire.Refine(m.mapping, m.gaps, m.pooled, band, samples)
		if label == wire.MapRandom && !m.pooled && len(fresh) > 0 {
			// Unrelated ports so far, and too few of them to say whether
			// the router keeps to a narrow band: a few more sockets tell.
			n.mu.Unlock()
			more, _ := freshPorts(ctx, hub, ask, e.freshSocket)
			n.mu.Lock()
			if n.run != token {
				n.mu.Unlock()
				return
			}
			band, samples = n.ports.add(channel.Addr().Unmap(), more...)
			label = wire.Refine(m.mapping, m.gaps, m.pooled, band, samples)
		}
		if label == wire.MapSequential {
			// A search counts on which port each of its packets gets.
			n.mu.Unlock()
			ordered, told := burstInOrder(ctx, hub, ask, e.freshSocket)
			n.mu.Lock()
			if n.run != token {
				n.mu.Unlock()
				return
			}
			m.unordered = told && !ordered
		}
		if m.mapping != "" && m.mapping != wire.MapNone && m.mapping != wire.MapIndependent {
			m.band = band
		}
		m.mapping = label
		n.measurement = m
		n.mu.Unlock()
		e.log.Info("router probed", "mapping", orUnknown(m.mapping), "filtering", orUnknown(m.filtering),
			"across", m.across, "gaps", m.gaps, "kept", m.kept, "band", m.band, "pooled", m.pooled, "ordered", !m.unordered)
		if m.mapping == "" || m.mapping == wire.MapNone {
			return
		}
		router, pcp := e.routerHolds(ctx, channel.Addr().Unmap())
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.run != token {
			return
		}
		n.router, n.pcp = router, pcp
		e.log.Info("router asked", "router", orUnknown(router), "pcp", pcp)
	}()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// portBand is the public ports a device was seen at on one public
// address: the lowest and the highest, and the ranges they fall in. A
// carrier that shares an address between customers gives each a band of
// its ports, and some routers pick out of a few narrow ranges far from
// each other, so the ranges' width says how far a search for this
// device's next port has to go. Another address starts it again.
type portBand struct {
	addr   netip.Addr
	lo, hi uint16
	n      int
	// ranges are in order and more than rangeJoin apart. A device seen in
	// more of them than a search may name has none: its ports are
	// scattered, and what counts is the lowest and the highest.
	ranges    []portRange
	scattered bool
}

type portRange struct{ lo, hi uint16 }

// rangeJoin is how far from a range a port may be and still widen it.
const rangeJoin = 1024

func (b *portBand) add(addr netip.Addr, seen ...netip.AddrPort) (width int32, samples int) {
	if addr != b.addr {
		*b = portBand{addr: addr}
	}
	for _, a := range seen {
		if !a.IsValid() || a.Addr().Unmap() != addr {
			continue
		}
		if b.n == 0 || a.Port() < b.lo {
			b.lo = a.Port()
		}
		if b.n == 0 || a.Port() > b.hi {
			b.hi = a.Port()
		}
		b.n++
		b.place(a.Port())
	}
	return b.width(), b.n
}

// place puts a port into the range it is near, or into one of its own,
// and joins ranges that have grown close.
func (b *portBand) place(p uint16) {
	if b.scattered {
		return
	}
	at := len(b.ranges)
	for i, r := range b.ranges {
		if int(p) <= int(r.hi)+rangeJoin {
			at = i
			break
		}
	}
	if at < len(b.ranges) && int(p) >= int(b.ranges[at].lo)-rangeJoin {
		r := &b.ranges[at]
		r.lo, r.hi = min(r.lo, p), max(r.hi, p)
	} else {
		b.ranges = append(b.ranges, portRange{})
		copy(b.ranges[at+1:], b.ranges[at:])
		b.ranges[at] = portRange{p, p}
	}
	for i := 0; i+1 < len(b.ranges); {
		if int(b.ranges[i+1].lo)-int(b.ranges[i].hi) <= rangeJoin {
			b.ranges[i].hi = b.ranges[i+1].hi
			b.ranges = append(b.ranges[:i+1], b.ranges[i+2:]...)
			continue
		}
		i++
	}
	if len(b.ranges) > wire.MaxSeekSpans {
		b.ranges, b.scattered = nil, true
	}
}

// width is how many ports wide the ranges are together, or from the
// lowest port to the highest when the ports are scattered.
func (b *portBand) width() int32 {
	if b.scattered || len(b.ranges) == 0 {
		return int32(b.hi) - int32(b.lo)
	}
	var w int32
	for _, r := range b.ranges {
		w += int32(r.hi) - int32(r.lo)
	}
	return w
}

// around is the range a port falls in.
func (b *portBand) around(p uint16) (portRange, bool) {
	for _, r := range b.ranges {
		if p >= r.lo && p <= r.hi {
			return r, true
		}
	}
	return portRange{}, false
}

// freshPorts opens sockets one after the other, asks the probe port from
// each and returns the public address each one got, in order, and whether
// every one of them had the socket's own port number. It stops at the
// first socket that gets no answer: a gap needs two neighbours.
func freshPorts(ctx context.Context, hub netip.AddrPort, ask []byte, open func(netip.AddrPort) (net.PacketConn, error)) (seen []netip.AddrPort, kept bool) {
	kept = true
	buf := make([]byte, 2048)
	for i := 0; i < natFresh && ctx.Err() == nil; i++ {
		conn, err := open(hub)
		if err != nil {
			break
		}
		a, ok := askFrom(conn, hub, ask, buf, false)
		local := conn.LocalAddr().(*net.UDPAddr).Port
		_ = conn.Close()
		if !ok {
			break
		}
		seen = append(seen, netip.AddrPortFrom(a.Addr().Unmap(), a.Port()))
		kept = kept && int(a.Port()) == local
	}
	return seen, kept
}

// burstInOrder opens natBurst sockets, asks the probe port from all of
// them without waiting and reports whether the router gave them their
// ports in the order the asks were written, natBursts times over. A
// wireless link, or a router with more than one processor, may pass
// packets of different connections on in another order than they were
// written when they are milliseconds apart: a search then has to space
// its packets (paceGap). told is false when an answer was missing.
func burstInOrder(ctx context.Context, hub netip.AddrPort, ask []byte, open func(netip.AddrPort) (net.PacketConn, error)) (ordered, told bool) {
	to := net.UDPAddrFromAddrPort(hub)
	buf := make([]byte, 2048)
	ordered = true
	for round := 0; round < natBursts && ctx.Err() == nil; round++ {
		var conns []net.PacketConn
		for i := 0; i < natBurst; i++ {
			conn, err := open(hub)
			if err != nil {
				break
			}
			conns = append(conns, conn)
		}
		sent := len(conns) == natBurst
		for _, conn := range conns {
			if _, err := conn.WriteTo(ask, to); err != nil {
				sent = false
			}
		}
		last, all := 0, sent
		for _, conn := range conns {
			if all {
				a, ok := askFrom(conn, hub, ask, buf, true)
				all = ok
				ordered = ordered && int(a.Port()) > last
				last = int(a.Port())
			}
			_ = conn.Close()
		}
		if !all {
			return true, false
		}
	}
	return ordered, ctx.Err() == nil
}

// askFrom asks the probe port from one socket, twice at most, and returns
// the address the hub saw. Only the probe port's own answer to this ask
// counts. With sent, the first ask has left already.
func askFrom(conn net.PacketConn, hub netip.AddrPort, ask, buf []byte, sent bool) (netip.AddrPort, bool) {
	want, ok := wire.ParseProbe(ask)
	if !ok {
		return netip.AddrPort{}, false
	}
	to := net.UDPAddrFromAddrPort(hub)
	for try := 0; try < 2; try++ {
		if try > 0 || !sent {
			if _, err := conn.WriteTo(ask, to); err != nil {
				return netip.AddrPort{}, false
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(natFreshWait))
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			udp, isUDP := addr.(*net.UDPAddr)
			if !isUDP {
				continue
			}
			from := udp.AddrPort()
			p, ok := wire.ParseProbe(buf[:n])
			if ok && p.Kind == wire.ProbeTell && p.Token == want.Token && p.Addr.IsValid() &&
				netip.AddrPortFrom(from.Addr().Unmap(), from.Port()) == hub {
				return p.Addr, true
			}
		}
	}
	return netip.AddrPort{}, false
}

// freshSocket opens a socket of its own toward the probe port.
func (e *Endpoint) freshSocket(hub netip.AddrPort) (net.PacketConn, error) {
	if e.cfg.fresh != nil {
		return e.cfg.fresh()
	}
	network := "udp4"
	if hub.Addr().Is6() {
		network = "udp6"
	}
	return net.ListenUDP(network, nil)
}

// natMoved is how many readings in a row have to come out at one other
// public address before the device takes that address for its own.
const natMoved = 3

// sameAddress says whether a new socket came out at the public address
// the hub sees the channel at, which is what makes its port a reading of
// the router's counter. The address is the one the hub named when the
// channel came up, and a channel outlives a change of it: the connection
// moves to the new address and nothing is said. So when natMoved readings
// in a row come out at one other address, and the router is not one that
// spreads a household over several, that address is the device's now: the
// ranges it was seen in at the old one are dropped and the router is asked
// again who holds it. Called with n.mu held.
func (n *natState) sameAddress(a netip.Addr) bool {
	a = a.Unmap()
	if a == n.channel.Addr().Unmap() {
		n.elseAddr, n.elseSeen = netip.Addr{}, 0
		return true
	}
	if a != n.elseAddr {
		n.elseAddr, n.elseSeen = a, 0
	}
	n.elseSeen++
	if n.elseSeen < natMoved || n.pooled {
		return false
	}
	n.channel = netip.AddrPortFrom(a, n.channel.Port())
	n.elseAddr, n.elseSeen = netip.Addr{}, 0
	n.ports = portBand{}
	n.readAt, n.routerAsked = time.Time{}, time.Time{}
	return true
}

// counter reads where the router's port counter stands: the public port
// it gives a socket opened now. A reading no older than fresh is used
// again. It says nothing without a probe port, or when the socket came
// out at another public address than the channel.
func (e *Endpoint) counter(ctx context.Context, fresh time.Duration) (uint16, bool) {
	n := &e.nat
	n.readMu.Lock()
	defer n.readMu.Unlock()
	n.mu.Lock()
	hub, token := n.hub, n.run
	port, recent := n.readPort, fresh > 0 && !n.readAt.IsZero() && time.Since(n.readAt) < fresh
	n.mu.Unlock()
	if recent {
		return port, true
	}
	if !hub.IsValid() || token == 0 || ctx.Err() != nil {
		return 0, false
	}
	conn, err := e.freshSocket(hub)
	if err != nil {
		return 0, false
	}
	ask := wire.Probe{Kind: wire.ProbeAsk, Device: e.cfg.Device, Token: token}.Encode()
	a, ok := askFrom(conn, hub, ask, make([]byte, 2048), false)
	_ = conn.Close()
	if !ok {
		return 0, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.run != token || !n.sameAddress(a.Addr()) {
		return 0, false
	}
	n.readAt, n.readPort = time.Now(), a.Port()
	n.ports.add(n.channel.Addr().Unmap(), a)
	return a.Port(), true
}

// reading takes a reading of the router's port counter whose place among
// the packets this device writes is known: the ask leaves from a socket
// of its own before reading returns, so the router gives that socket its
// port after everything written before and ahead of everything written
// after. The answer is waited for by the function it returns, which says
// nothing when there was none.
func (e *Endpoint) reading(ctx context.Context) func() (uint16, bool) {
	none := func() (uint16, bool) { return 0, false }
	n := &e.nat
	n.mu.Lock()
	hub, token := n.hub, n.run
	n.mu.Unlock()
	if !hub.IsValid() || token == 0 || ctx.Err() != nil {
		return none
	}
	conn, err := e.freshSocket(hub)
	if err != nil {
		return none
	}
	ask := wire.Probe{Kind: wire.ProbeAsk, Device: e.cfg.Device, Token: token}.Encode()
	if _, err := conn.WriteTo(ask, net.UDPAddrFromAddrPort(hub)); err != nil {
		_ = conn.Close()
		return none
	}
	got := make(chan uint16, 1)
	go func() {
		a, ok := askFrom(conn, hub, ask, make([]byte, 2048), true)
		_ = conn.Close()
		if !ok {
			got <- 0
			return
		}
		n.mu.Lock()
		same := n.run == token && n.sameAddress(a.Addr())
		if same {
			n.ports.add(n.channel.Addr().Unmap(), a)
		}
		n.mu.Unlock()
		if !same {
			got <- 0
			return
		}
		got <- a.Port()
	}()
	var port uint16
	var once sync.Once
	return func() (uint16, bool) {
		once.Do(func() { port = <-got })
		return port, port != 0
	}
}

// seenNow asks the probe port from n new sockets at once and returns the
// ranges the public ports they got fall in: where a router that picks is
// picking at this moment. Some hand each household a small block of ports
// and another one when that is used up, so the ranges a device was seen in
// an hour ago may not be where its next port comes from.
func (e *Endpoint) seenNow(ctx context.Context, n int) []portRange {
	readings := make([]func() (uint16, bool), 0, n)
	for i := 0; i < n; i++ {
		readings = append(readings, e.reading(ctx))
	}
	ports := make([]uint16, 0, n)
	for _, r := range readings {
		if p, ok := r(); ok {
			ports = append(ports, p)
		}
	}
	// The address after the readings: they are what notices a change of it.
	_, public, _ := e.router()
	var now portBand
	for _, p := range ports {
		now.add(public, netip.AddrPortFrom(public, p))
	}
	return now.ranges
}

// paced reports that this device has to space the packets of a search:
// written together, they do not get their ports in the order written.
func (e *Endpoint) paced() bool {
	n := &e.nat
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.unordered
}

// probing reports that the first probe run on a channel has not finished:
// the label for the router is still to come.
func (e *Endpoint) probing() bool {
	n := &e.nat
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.run != 0 && n.checked.IsZero()
}

// router is the label for this device's router, the address the hub sees
// the device at and the ranges of public ports it was seen at there.
func (e *Endpoint) router() (mapping string, public netip.Addr, ranges []portRange) {
	n := &e.nat
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ports.n > 0 && n.ports.addr == n.channel.Addr().Unmap() {
		ranges = append(ranges, n.ports.ranges...)
	}
	return n.mapping, n.channel.Addr().Unmap(), ranges
}

// seenAround is the range of public ports, among those this device was
// seen at, that a port falls in.
func (e *Endpoint) seenAround(p uint16) (portRange, bool) {
	n := &e.nat
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ports.addr != n.channel.Addr().Unmap() {
		return portRange{}, false
	}
	return n.ports.around(p)
}

// routerHolds says who holds the public address the hub sees this device
// at: the home router, when the address it reports as its own outside is
// that one, or something beyond it; and whether anything upstream answers
// the port control protocol. The router is asked, nothing is opened on
// it, and the answers are kept for an hour.
func (e *Endpoint) routerHolds(ctx context.Context, public netip.Addr) (router string, pcp bool) {
	if e.cfg.RouterAddress == nil {
		return "", false
	}
	n := &e.nat
	n.mu.Lock()
	addr, pcp, fresh := n.routerAddr, n.routerPCP, !n.routerAsked.IsZero() && time.Since(n.routerAsked) < natRouterEvery
	n.mu.Unlock()
	if !fresh {
		actx, cancel := context.WithTimeout(ctx, 12*time.Second)
		addr, _ = e.cfg.RouterAddress(actx)
		pcp = e.cfg.CarrierPCP != nil && e.cfg.CarrierPCP(actx)
		cancel()
		if ctx.Err() != nil {
			return "", false
		}
		n.mu.Lock()
		n.routerAddr, n.routerPCP, n.routerAsked = addr, pcp, time.Now()
		n.mu.Unlock()
	}
	switch {
	case !addr.IsValid() || addr.IsUnspecified():
		return wire.RouterSilent, pcp
	case addr.Unmap() == public:
		return wire.RouterPublic, pcp
	}
	return wire.RouterCarrier, pcp
}

// onProbe takes a probe packet off the mesh socket. Only the hub's probe
// port, with this run's token, is believed.
func (e *Endpoint) onProbe(p wire.Probe, from *net.UDPAddr) {
	n := &e.nat
	n.mu.Lock()
	defer n.mu.Unlock()
	src := from.AddrPort()
	if n.run == 0 || p.Token != n.run || netip.AddrPortFrom(src.Addr().Unmap(), src.Port()) != n.hub {
		return
	}
	switch p.Kind {
	case wire.ProbeKnock:
		if !n.asked {
			n.knocked = true
		}
	case wire.ProbeTell:
		if n.asked && p.Addr.IsValid() {
			n.seen = p.Addr
		}
	}
}

// NAT is what the device knows about its router right now.
func (e *Endpoint) NAT() NATStatus {
	n := &e.nat
	n.mu.Lock()
	st := NATStatus{CensusNAT: wire.CensusNAT{
		Mapping: n.mapping, Filtering: n.filtering,
		Across: n.across, Gaps: append([]int32(nil), n.gaps...), Kept: n.kept, Router: n.router,
		Band: n.band, Pooled: n.pooled, PCP: n.pcp,
	}, Checked: n.checked}
	n.mu.Unlock()
	e.mu.Lock()
	st.PortMap = len(e.mapped) > 0
	e.mu.Unlock()
	st.IPv6 = hasGlobalIPv6()
	st.Reach = st.CensusNAT.Reach()
	return st
}

// ownAddr reports whether an address is on one of this device's
// interfaces: a public address that is means there is no NAT.
func ownAddr(a netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, x := range addrs {
		if ipn, ok := x.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok && ip.Unmap() == a {
				return true
			}
		}
	}
	return false
}

// hasGlobalIPv6 reports a public IPv6 address on an interface that is up:
// not a private one (fc00::/7), not link-local, not an overlay's.
func hasGlobalIPv6() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() != nil {
				continue
			}
			ip := ipn.IP
			if ip.IsGlobalUnicast() && !ip.IsPrivate() && !isOverlay(ip) {
				return true
			}
		}
	}
	return false
}
