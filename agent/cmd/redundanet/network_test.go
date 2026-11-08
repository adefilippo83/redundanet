package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// The network built into the binary is the live one: its name, hubs and
// operator keys are the manifest's, and its manifest is served from the
// repository the release index is served from. A hub or operator change in
// the manifest fails here until the descriptor is regenerated with
// `redundanet network descriptor`.
func TestBuiltinNetworkMatchesTheManifest(t *testing.T) {
	d, err := builtinDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "manifests", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if d.Network != m.Network.Name {
		t.Errorf("network %q, manifest %q", d.Network, m.Network.Name)
	}
	if len(d.Hubs) != len(m.Hubs) {
		t.Fatalf("%d hubs, manifest %d", len(d.Hubs), len(m.Hubs))
	}
	for i, h := range m.Hubs {
		if d.Hubs[i].Key != h.Key || d.Hubs[i].Relay != h.Relay {
			t.Errorf("hub %d: %s at %s, manifest %s at %s", i, d.Hubs[i].Key, d.Hubs[i].Relay, h.Key, h.Relay)
		}
	}
	if len(d.Operators) != len(m.Operators) {
		t.Fatalf("%d operators, manifest %d", len(d.Operators), len(m.Operators))
	}
	for i, o := range m.Operators {
		if d.Operators[i] != o.Key {
			t.Errorf("operator %d: %s, manifest %s", i, d.Operators[i], o.Key)
		}
	}
	if want := strings.TrimSuffix(version.ReleaseIndexURL, "/releases/index.yaml"); d.Repository != want {
		t.Errorf("repository %q, the release index is under %q", d.Repository, want)
	}
	if defaultNetwork() != m.Network.Name {
		t.Errorf("defaultNetwork %q", defaultNetwork())
	}
}

func TestNetworkArg(t *testing.T) {
	if got := networkArg(defaultNetwork()); got != "" {
		t.Errorf("built-in network: %q, want nothing", got)
	}
	if got := networkArg("other"); got != " --network other" {
		t.Errorf("another network: %q", got)
	}
}

// A device that never joined by hand is on the built-in network and follows
// the descriptor across releases; one that joined by hand, or asks for
// another network, keeps what it has.
func TestMembershipConfigUsesTheBuiltinNetwork(t *testing.T) {
	dd := t.TempDir()
	cfg := membershipConfig(dd, defaultNetwork())
	if cfg.Descriptor == nil || cfg.Descriptor.Network != defaultNetwork() {
		t.Fatalf("no network joined: got %+v, want the built-in descriptor", cfg.Descriptor)
	}
	if b, _ := os.ReadFile(filepath.Join(dd, builtinMarker)); string(b) != version.Network {
		t.Errorf("marker %q, want the built-in descriptor", b)
	}
	if cfg := membershipConfig(dd, "other"); cfg.Descriptor != nil {
		t.Errorf("another network: got %+v, want none", cfg.Descriptor)
	}
	// Seeded and saved: nothing to do while the build carries the same.
	if err := membership.SaveDescriptor(filepath.Join(dd, membership.DescriptorFile), cfg.Descriptor); err != nil {
		t.Fatal(err)
	}
	if cfg := membershipConfig(dd, defaultNetwork()); cfg.Descriptor != nil {
		t.Errorf("seeded and current: got %+v, want the saved one to be read instead", cfg.Descriptor)
	}
	// A later build with a changed descriptor: followed, since the device
	// never joined by hand.
	changed := *cfg.Descriptor
	changed.Hubs = []membership.HubHint{{Key: cfg.Descriptor.Hubs[0].Key, Relay: "hub2.example.org:8443"}}
	enc, err := changed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	saved := version.Network
	version.Network = enc
	t.Cleanup(func() { version.Network = saved })
	if cfg := membershipConfig(dd, defaultNetwork()); cfg.Descriptor == nil || cfg.Descriptor.Hubs[0].Relay != "hub2.example.org:8443" {
		t.Errorf("changed build: got %+v, want the new descriptor", cfg.Descriptor)
	}
	// Joined by hand (no marker): pinned, whatever the build carries.
	hand := t.TempDir()
	other := &membership.Descriptor{Network: defaultNetwork(), Operators: cfg.Descriptor.Operators}
	if err := membership.SaveDescriptor(filepath.Join(hand, membership.DescriptorFile), other); err != nil {
		t.Fatal(err)
	}
	if cfg := membershipConfig(hand, defaultNetwork()); cfg.Descriptor != nil {
		t.Errorf("joined by hand: got %+v, want the saved one to be read instead", cfg.Descriptor)
	}
}

// A build without a network built in requires the flag and the descriptor.
func TestWithoutBuiltinNetwork(t *testing.T) {
	saved := version.Network
	version.Network = ""
	t.Cleanup(func() { version.Network = saved })
	if _, err := builtinDescriptor(); err == nil {
		t.Error("builtinDescriptor: want an error")
	}
	if defaultNetwork() != "" {
		t.Errorf("defaultNetwork %q, want empty", defaultNetwork())
	}
	if got := networkArg("redundanet"); got != " --network redundanet" {
		t.Errorf("networkArg %q", got)
	}
	if cfg := membershipConfig(t.TempDir(), "redundanet"); cfg.Descriptor != nil {
		t.Errorf("got %+v, want none", cfg.Descriptor)
	}
}
