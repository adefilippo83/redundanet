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
	n := &CensusNAT{Mapping: "x", Across: 1 << 30, Gaps: append(make([]int32, 20), 0), Kept: true, Router: "203.0.113.7", Band: -5, Pooled: true, PCP: true}
	n.Gaps[0] = -1 << 30
	m := n.Measured()
	if m.Mapping != "" || m.Across != 65535 || len(m.Gaps) != MaxPortGaps || m.Gaps[0] != -65535 || !m.Kept || m.Router != "" || m.Band != 0 || !m.Pooled || !m.PCP {
		t.Fatalf("measured: %+v", m)
	}
	if m := (&CensusNAT{Kept: true, Router: RouterCarrier, Band: 300}).Measured(); m.Kept || m.Router != RouterCarrier || m.Band != 300 {
		t.Fatalf("kept without gaps, or a known word lost: %+v", m)
	}
	if m := (*CensusNAT)(nil).Measured(); m.Across != 0 || m.Gaps != nil || m.Kept || m.Router != "" {
		t.Fatal("nothing measured")
	}
}

// A channel that has been up for hours makes a router that hands ports
// out in order look as if it picked an unrelated one: the gaps between
// sockets opened together correct the label.
func TestRefine(t *testing.T) {
	for _, c := range []struct {
		mapping string
		gaps    []int32
		pooled  bool
		band    int32
		samples int
		want    string
	}{
		{MapRandom, []int32{2, 1, 1}, false, 400, 20, MapSequential},
		{MapRandom, []int32{4, 2, 2}, false, 0, 0, MapSequential},
		{MapSequential, []int32{32, -15, 31}, false, 0, 0, MapRandom},
		{MapSequential, []int32{32, -15, 31}, false, 110, 12, MapConfined},
		{MapRandom, []int32{32, -15, 31}, false, 110, 3, MapRandom},
		{MapRandom, []int32{900, -15000, 31}, false, 27000, 30, MapRandom},
		{MapRandom, []int32{1, 1, 1}, true, 10, 30, MapRandom},
		{MapRandom, nil, false, 0, 0, MapRandom},
		{MapSequential, []int32{1}, false, 0, 0, MapSequential},
		{MapIndependent, []int32{700, -3, 9}, false, 0, 0, MapIndependent},
		{MapNone, nil, false, 0, 0, MapNone},
		{"", []int32{1, 1}, false, 0, 0, ""},
	} {
		if got := Refine(c.mapping, c.gaps, c.pooled, c.band, c.samples); got != c.want {
			t.Errorf("Refine(%s, %v, %v, %d, %d) = %s, want %s", c.mapping, c.gaps, c.pooled, c.band, c.samples, got, c.want)
		}
	}
	if r := (&CensusNAT{Mapping: MapConfined}).Reach(); r != ReachPredictable {
		t.Fatalf("a confined router: %s", r)
	}
}
