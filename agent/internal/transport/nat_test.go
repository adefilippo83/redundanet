package transport

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// On one machine there is no router: the hub's knock gets in and both hub
// ports see the same address, which is the device's own. The device says
// so on its status and in its census, in labels and nothing else.
func TestTheProbeTellsADeviceHowItIsReached(t *testing.T) {
	n := newTestNetProbe(t, false, "127.0.0.1:0")
	ep, key := n.endpoint(seed(1), true, func(c *Config) { c.Census = func() wire.Census { return wire.Census{Version: "test"} } })
	eventually(t, 10*time.Second, "the probe to finish", func() bool { return !ep.NAT().Checked.IsZero() })
	nat := ep.NAT()
	if nat.Mapping != wire.MapNone || nat.Filtering != wire.FilterOpen || nat.Reach != wire.ReachOpen {
		t.Fatalf("on loopback: %+v", nat)
	}
	if st := ep.Status(); st.NAT == nil || st.NAT.Reach != wire.ReachOpen {
		t.Fatalf("status: %+v", st.NAT)
	}
	eventually(t, 10*time.Second, "the census to carry the labels", func() bool {
		for _, c := range n.censuses(key) {
			if c.NAT != nil && c.NAT.Mapping == wire.MapNone && c.NAT.Filtering == wire.FilterOpen {
				return true
			}
		}
		return false
	})
}

// A hub without the second port tells a device nothing, and the device
// reports nothing it did not measure.
func TestWithoutAProbeADeviceKnowsNothingOfItsRouter(t *testing.T) {
	n := newTestNet(t, false)
	ep, _ := n.endpoint(seed(1), true, nil)
	eventually(t, 5*time.Second, "the hub channel", func() bool {
		st := ep.Status()
		return len(st.Hubs) == 1 && st.Hubs[0].Up
	})
	time.Sleep(natKnockWait / 4)
	if nat := ep.NAT(); nat.Mapping != "" || nat.Filtering != "" || !nat.Checked.IsZero() || (nat.Reach != wire.ReachUnknown && !nat.PortMap) {
		t.Fatalf("without a probe: %+v", nat)
	}
}

// Only the hub's probe port with the run's own token is believed: a
// packet with another token, or from another address, changes nothing.
func TestAProbePacketFromAStrangerIsIgnored(t *testing.T) {
	n := newTestNet(t, false)
	ep, _ := n.endpoint(seed(1), true, nil)
	hub := net.UDPAddrFromAddrPort(mustAddrPort(t, "127.0.0.1:655"))
	ep.nat.mu.Lock()
	ep.nat.run, ep.nat.hub, ep.nat.asked = 7, hub.AddrPort(), true
	ep.nat.mu.Unlock()
	seen := mustAddrPort(t, "203.0.113.9:4000")
	ep.onProbe(wire.Probe{Kind: wire.ProbeTell, Token: 8, Addr: seen}, hub)
	ep.onProbe(wire.Probe{Kind: wire.ProbeTell, Token: 7, Addr: seen}, net.UDPAddrFromAddrPort(mustAddrPort(t, "127.0.0.1:656")))
	ep.onProbe(wire.Probe{Kind: wire.ProbeKnock, Token: 7}, hub) // after the device asked: proves nothing
	ep.nat.mu.Lock()
	if ep.nat.seen.IsValid() || ep.nat.knocked {
		t.Fatalf("a stranger's packet was believed: %+v %v", ep.nat.seen, ep.nat.knocked)
	}
	ep.nat.mu.Unlock()
	ep.onProbe(wire.Probe{Kind: wire.ProbeTell, Token: 7, Addr: seen}, hub)
	ep.nat.mu.Lock()
	defer ep.nat.mu.Unlock()
	if ep.nat.seen != seen {
		t.Fatalf("the hub's own answer was not taken: %v", ep.nat.seen)
	}
}

func mustAddrPort(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

// The hub's probe port answers a device's own token and nothing else: a
// stranger who knows a device key gets no packet back.
func TestTheProbePortAnswersNoStranger(t *testing.T) {
	n := newTestNetProbe(t, false, "127.0.0.1:0")
	ep, key := n.endpoint(seed(1), true, nil)
	eventually(t, 10*time.Second, "the probe to finish", func() bool { return !ep.NAT().Checked.IsZero() })
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for _, p := range []wire.Probe{
		{Kind: wire.ProbeAsk, Device: key, Token: 12345},
		{Kind: wire.ProbeAsk, Device: key},
		{Kind: wire.ProbeAsk, Token: 12345},
		{Kind: wire.ProbeKnock, Device: key, Token: 12345},
	} {
		if _, err := pc.WriteTo(p.Encode(), n.hub.ProbeAddr()); err != nil {
			t.Fatal(err)
		}
	}
	_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 256)
	if got, _, err := pc.ReadFrom(buf); err == nil {
		t.Fatalf("the probe port answered a stranger with %d bytes", got)
	}
}

// Sockets opened one after the other each learn their public port from
// the probe port. On one machine that port is the socket's own, so every
// one is kept and the gaps are the differences between the local ports.
func TestFreshSocketsLearnTheirPublicPorts(t *testing.T) {
	n := newTestNetProbe(t, false, "127.0.0.1:0")
	ep, _ := n.endpoint(seed(1), true, nil)
	eventually(t, 10*time.Second, "the probe to finish", func() bool { return !ep.NAT().Checked.IsZero() })
	ep.nat.mu.Lock()
	hub, token := ep.nat.hub, ep.nat.run
	ep.nat.mu.Unlock()
	ask := wire.Probe{Kind: wire.ProbeAsk, Device: ep.cfg.Device, Token: token}.Encode()
	ports, kept := freshPorts(context.Background(), hub, ask, ep.freshSocket)
	if len(ports) != natFresh || !kept {
		t.Fatalf("fresh sockets: ports %v, kept %v", ports, kept)
	}
	for _, a := range ports {
		if a.Addr() != hub.Addr() {
			t.Fatalf("on one machine every socket is seen at the machine's address: %v", ports)
		}
	}
	// With a token the hub did not give, no socket learns anything.
	wrong := wire.Probe{Kind: wire.ProbeAsk, Device: ep.cfg.Device, Token: token + 2}.Encode()
	if ports, _ := freshPorts(context.Background(), hub, wrong, ep.freshSocket); len(ports) != 0 {
		t.Fatalf("a wrong token was answered: %v", ports)
	}
	// With no router in between, the device measured none of this.
	if nat := ep.NAT(); len(nat.Gaps) != 0 || nat.Kept || nat.Router != "" || nat.Across != 0 {
		t.Fatalf("on loopback: %+v", nat)
	}
}

// Who holds the public address: the home router when the address it
// gives as its own outside is the one the hub sees, something beyond it
// otherwise, and the router is asked once an hour at most.
func TestTheRouterIsAskedWhoHoldsTheAddress(t *testing.T) {
	n := newTestNet(t, false)
	asked, pcpAsked := 0, 0
	answer, ok := netip.MustParseAddr("203.0.113.7"), true
	ep, _ := n.endpoint(seed(1), true, func(c *Config) {
		c.RouterAddress = func(context.Context) (netip.Addr, bool) { asked++; return answer, ok }
		c.CarrierPCP = func(context.Context) bool { pcpAsked++; return pcpAsked == 1 }
	})
	ctx := context.Background()
	if got, pcp := ep.routerHolds(ctx, netip.MustParseAddr("203.0.113.7")); got != wire.RouterPublic || !pcp {
		t.Fatalf("the router's own address: %q, pcp %v", got, pcp)
	}
	if got, pcp := ep.routerHolds(ctx, netip.MustParseAddr("198.51.100.1")); got != wire.RouterCarrier || !pcp || asked != 1 || pcpAsked != 1 {
		t.Fatalf("another address: %q after %d questions", got, asked)
	}
	ep.nat.mu.Lock()
	ep.nat.routerAsked = time.Now().Add(-2 * natRouterEvery)
	ep.nat.mu.Unlock()
	answer, ok = netip.Addr{}, false
	if got, pcp := ep.routerHolds(ctx, netip.MustParseAddr("203.0.113.7")); got != wire.RouterSilent || pcp || asked != 2 {
		t.Fatalf("a router that does not say: %q after %d questions", got, asked)
	}
	ep.cfg.RouterAddress = nil
	if got, _ := ep.routerHolds(ctx, netip.MustParseAddr("203.0.113.7")); got != "" {
		t.Fatalf("not asked: %q", got)
	}
}

// The band is the width of the ports seen at one address; another address
// starts it again, and a port seen at some other address is not counted.
func TestThePortBandIsPerAddress(t *testing.T) {
	a, b := netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("203.0.113.8")
	at := func(ip netip.Addr, port uint16) netip.AddrPort { return netip.AddrPortFrom(ip, port) }
	var band portBand
	if w, n := band.add(a, at(a, 40100), at(a, 40020), at(b, 9), netip.AddrPort{}); w != 80 || n != 2 {
		t.Fatalf("two ports at one address: width %d over %d", w, n)
	}
	if w, n := band.add(a, at(a, 40500)); w != 480 || n != 3 {
		t.Fatalf("a third: width %d over %d", w, n)
	}
	if w, n := band.add(b, at(b, 1200)); w != 0 || n != 1 {
		t.Fatalf("another address: width %d over %d", w, n)
	}
}
