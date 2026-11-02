package main

import (
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
