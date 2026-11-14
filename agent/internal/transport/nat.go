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
	mapping, filtering string
	checked            time.Time
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
		defer n.mu.Unlock()
		if n.run != token {
			return
		}
		n.mapping, n.filtering, n.checked = "", "", time.Now()
		if n.seen.IsValid() {
			n.mapping = wire.Mapping(n.channel, n.seen, ownAddr)
			n.filtering = wire.FilterStrict
		}
		if n.knocked {
			n.filtering = wire.FilterOpen
		}
		e.log.Info("router probed", "mapping", orUnknown(n.mapping), "filtering", orUnknown(n.filtering))
	}()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
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
	st := NATStatus{CensusNAT: wire.CensusNAT{Mapping: n.mapping, Filtering: n.filtering}, Checked: n.checked}
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
