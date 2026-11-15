package wire

import (
	"net/netip"
	"testing"
)

func TestProbeRoundTrip(t *testing.T) {
	for _, p := range []Probe{
		{Kind: ProbeKnock, Token: 1},
		{Kind: ProbeAsk, Token: 1<<63 + 5, Device: [32]byte{1, 2, 3}},
		{Kind: ProbeTell, Token: 9, Addr: netip.MustParseAddrPort("203.0.113.7:40001")},
		{Kind: ProbeTell, Token: 9, Addr: netip.MustParseAddrPort("[2001:db8::1]:655")},
	} {
		b := p.Encode()
		if len(b) != ProbeLen || b[0] != 0 {
			t.Fatalf("encoding of %+v: %d bytes, first %d", p, len(b), b[0])
		}
		got, ok := ParseProbe(b)
		if !ok || got != p {
			t.Errorf("round trip: %+v became %+v (%v)", p, got, ok)
		}
	}
	bad := Probe{Kind: ProbeAsk}.Encode()
	bad[len(ProbeMagic)] = 9
	for _, b := range [][]byte{nil, bad, Probe{}.Encode()[:ProbeLen-1], append([]byte("\x00RN3PNCH"), make([]byte, ProbeLen-8)...)} {
		if _, ok := ParseProbe(b); ok {
			t.Errorf("%d bytes were taken for a probe", len(b))
		}
	}
}

func TestMappingAndReach(t *testing.T) {
	ap := netip.MustParseAddrPort
	own := func(a netip.Addr) bool { return a == netip.MustParseAddr("198.51.100.4") }
	for _, c := range []struct {
		channel, probe string
		want           string
	}{
		{"203.0.113.7:40001", "203.0.113.7:40001", MapIndependent},
		{"198.51.100.4:40001", "198.51.100.4:40001", MapNone},
		{"203.0.113.7:40001", "203.0.113.7:40003", MapSequential},
		{"203.0.113.7:40001", "203.0.113.7:39990", MapSequential},
		{"203.0.113.7:40001", "203.0.113.7:51234", MapRandom},
		{"203.0.113.7:40001", "203.0.113.8:40001", MapRandom},
	} {
		if got := Mapping(ap(c.channel), ap(c.probe), own); got != c.want {
			t.Errorf("%s and %s: %q, want %q", c.channel, c.probe, got, c.want)
		}
	}
	if got := Mapping(netip.AddrPort{}, ap("203.0.113.7:1"), own); got != "" {
		t.Errorf("without a channel address: %q", got)
	}
	for _, c := range []struct {
		nat  *CensusNAT
		want string
	}{
		{nil, ReachUnknown},
		{&CensusNAT{}, ReachUnknown},
		{&CensusNAT{IPv6: true}, ReachUnknown},
		{&CensusNAT{Mapping: "something else"}, ReachUnknown},
		{&CensusNAT{Mapping: MapNone}, ReachOpen},
		{&CensusNAT{Mapping: MapRandom, PortMap: true}, ReachOpen},
		{&CensusNAT{Mapping: MapIndependent, Filtering: FilterOpen}, ReachOpen},
		{&CensusNAT{Mapping: MapIndependent, Filtering: FilterStrict}, ReachEasy},
		{&CensusNAT{Mapping: MapSequential, Filtering: FilterOpen}, ReachPredictable},
		{&CensusNAT{Mapping: MapRandom, Filtering: FilterOpen}, ReachHard},
	} {
		if got := c.nat.Reach(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.nat, got, c.want)
		}
	}
}

// Gaps are differences between ports, and a hub keeps of a device's
// numbers only what a measurement can be.
func TestPortGapsAndNumbers(t *testing.T) {
	if g := PortGaps([]uint16{40000, 40001, 40003, 39000}); len(g) != 3 || g[0] != 1 || g[1] != 2 || g[2] != -1003 {
		t.Fatalf("gaps: %v", g)
	}
	if g := PortGaps([]uint16{40000}); g != nil {
		t.Fatalf("one port has no gap: %v", g)
	}
	if g := PortGaps(make([]uint16, 40)); len(g) != MaxPortGaps {
		t.Fatalf("gaps are capped: %d", len(g))
	}
	n := &CensusNAT{Across: 1 << 30, Gaps: append(make([]int32, 20), 0), Kept: true, Router: "203.0.113.7"}
	n.Gaps[0] = -1 << 30
	across, gaps, kept, router := n.Numbers()
	if across != 65535 || len(gaps) != MaxPortGaps || gaps[0] != -65535 || !kept || router != "" {
		t.Fatalf("numbers: %d %v %v %q", across, gaps, kept, router)
	}
	if _, _, kept, router := (&CensusNAT{Kept: true, Router: RouterCarrier}).Numbers(); kept || router != RouterCarrier {
		t.Fatalf("kept without gaps, or a known router label lost: %v %q", kept, router)
	}
	if a, g, k, r := (*CensusNAT)(nil).Numbers(); a != 0 || g != nil || k || r != "" {
		t.Fatal("nothing measured")
	}
}
