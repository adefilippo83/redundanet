package transport

import (
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
