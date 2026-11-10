package wire

import (
	"fmt"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// An address list comes from another device. Only literal addresses with
// a port survive, once each, and no more than the bound.
func TestAddrsKeepsLiteralAddressesOnly(t *testing.T) {
	in := []string{
		"192.0.2.7:4471",
		"example.com:53",
		"192.0.2.7:4471",
		"[2001:db8::1]:4471",
		"[fe80::1%en0]:4471",
		"192.0.2.8:0",
		"192.0.2.8",
		"0.0.0.0:4471",
		"[::]:4471",
		"224.0.0.1:4471",
		"[::ffff:192.0.2.9]:4471",
		RelayAddr(identity.Key{1, 2, 3}).String(),
		"",
		"10.0.0.2:4471",
	}
	want := []string{"192.0.2.7:4471", "[2001:db8::1]:4471", "192.0.2.9:4471", "10.0.0.2:4471"}
	got := Addrs(in, MaxCandidates)
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	var many []string
	for i := 0; i < 5000; i++ {
		many = append(many, fmt.Sprintf("192.0.%d.%d:4471", i/250, i%250+1))
	}
	if got := Addrs(many, MaxCandidates); len(got) != MaxCandidates || got[0] != many[0] {
		t.Fatalf("a long list kept %d entries", len(got))
	}
	if got := Addrs(nil, MaxCandidates); len(got) != 0 {
		t.Fatalf("an empty list gave %q", got)
	}
}
