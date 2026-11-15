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
}

// measurement is what one finished probe run found.
type measurement struct {
	mapping, filtering string
	checked            time.Time
	across             int32
	gaps               []int32
	kept, pooled       bool
	band               int32
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
				fresh, m.kept = freshPorts(ctx, hub, ask)
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
		if m.mapping != "" && m.mapping != wire.MapNone && m.mapping != wire.MapIndependent {
			m.band = band
		}
		m.mapping = wire.Refine(m.mapping, m.gaps, m.pooled, band, samples)
		n.measurement = m
		n.mu.Unlock()
		e.log.Info("router probed", "mapping", orUnknown(m.mapping), "filtering", orUnknown(m.filtering),
			"across", m.across, "gaps", m.gaps, "kept", m.kept, "band", m.band, "pooled", m.pooled)
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

// portBand is the lowest and highest public port a device was seen at on
// one public address. A carrier that shares an address between customers
// gives each a band of its ports, so the width says how far a search for
// this device's next port has to go. Another address starts it again.
type portBand struct {
	addr   netip.Addr
	lo, hi uint16
	n      int
}

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
	}
	return int32(b.hi) - int32(b.lo), b.n
}

// freshPorts opens sockets one after the other, asks the probe port from
// each and returns the public address each one got, in order, and whether
// every one of them had the socket's own port number. It stops at the
// first socket that gets no answer: a gap needs two neighbours.
func freshPorts(ctx context.Context, hub netip.AddrPort, ask []byte) (seen []netip.AddrPort, kept bool) {
	network := "udp4"
	if hub.Addr().Is6() {
		network = "udp6"
	}
	kept = true
	buf := make([]byte, 2048)
	for i := 0; i < natFresh && ctx.Err() == nil; i++ {
		conn, err := net.ListenUDP(network, nil)
		if err != nil {
			break
		}
		a, ok := askFrom(conn, hub, ask, buf)
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

// askFrom asks the probe port from one socket, twice at most, and returns
// the address the hub saw. Only the probe port's own answer to this ask
// counts.
func askFrom(conn *net.UDPConn, hub netip.AddrPort, ask, buf []byte) (netip.AddrPort, bool) {
	want, ok := wire.ParseProbe(ask)
	if !ok {
		return netip.AddrPort{}, false
	}
	for try := 0; try < 2; try++ {
		if _, err := conn.WriteToUDPAddrPort(ask, hub); err != nil {
			return netip.AddrPort{}, false
		}
		_ = conn.SetReadDeadline(time.Now().Add(natFreshWait))
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				break
			}
			p, ok := wire.ParseProbe(buf[:n])
			if ok && p.Kind == wire.ProbeTell && p.Token == want.Token && p.Addr.IsValid() &&
				netip.AddrPortFrom(from.Addr().Unmap(), from.Port()) == hub {
				return p.Addr, true
			}
		}
	}
	return netip.AddrPort{}, false
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
