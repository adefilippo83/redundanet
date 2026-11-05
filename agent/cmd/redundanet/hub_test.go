package main

import (
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func TestHubHealthy(t *testing.T) {
	var a, b identity.Key
	a[0], b[0] = 1, 2
	none := hub.Status{}
	both := hub.Status{Clients: []hub.ClientStatus{{Device: a.String()}, {Device: b.String()}}}
	onlyA := hub.Status{Clients: []hub.ClientStatus{{Device: a.String()}}}
	if !hubHealthy(none, nil) || !hubHealthy(both, []identity.Key{a, b}) || !hubHealthy(both, []identity.Key{a}) {
		t.Fatal("a serving hub with its devices back is healthy")
	}
	if hubHealthy(onlyA, []identity.Key{a, b}) || hubHealthy(none, []identity.Key{a}) {
		t.Fatal("a device that did not come back means not healthy yet")
	}
}

func TestCandidateArgsInheritTheServiceFlags(t *testing.T) {
	got := candidateArgs([]string{"agent", "--network", "redundanet", "--data-dir", "/var/lib/redundanet3", "--channel=early",
		"--storage-path", "/mnt/storage", "--api", "127.0.0.1:4470", "--no-update", "--probation", "--probation-peers", "a,b"}, []string{"x", "y"})
	want := "agent --network redundanet --data-dir /var/lib/redundanet3 --channel=early --storage-path /mnt/storage --api 127.0.0.1:4470 --probation --no-update --probation-peers x,y"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %q", strings.Join(got, " "))
	}
	got = candidateArgs([]string{"hub", "--udp", ":8443", "--probation-peers=a"}, nil)
	if strings.Join(got, " ") != "hub --udp :8443 --probation --no-update" {
		t.Fatalf("hub form: %q", strings.Join(got, " "))
	}
}
