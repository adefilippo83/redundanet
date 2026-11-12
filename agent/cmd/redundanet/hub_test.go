package main

import (
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func TestHubHealthy(t *testing.T) {
	var a, b identity.Key
	a[0], b[0] = 1, 2
	none := hub.Status{}
	both := hub.Status{Clients: []hub.ClientStatus{{Device: a.String()}, {Device: b.String()}}}
	onlyA := hub.Status{Clients: []hub.ClientStatus{{Device: a.String()}}}
	healthy := func(st hub.Status, peers []identity.Key, elapsed time.Duration) bool {
		ok, _ := hubHealthy(st, peers, nil, elapsed)
		return ok
	}
	if !healthy(none, nil, 0) || !healthy(both, []identity.Key{a, b}, 0) || !healthy(both, []identity.Key{a}, 0) {
		t.Fatal("a serving hub with its devices back is healthy")
	}
	if healthy(onlyA, []identity.Key{a, b}, 0) || healthy(none, []identity.Key{a}, 0) {
		t.Fatal("a device that did not come back means not healthy yet")
	}
	if !healthy(onlyA, []identity.Key{a, b}, probationGrace) {
		t.Fatal("after the grace, half of the devices back is healthy")
	}
	if healthy(none, []identity.Key{a}, probationDeadline) {
		t.Fatal("the only device away is never healthy")
	}
}

// The rule a candidate's devices are judged by: all back passes at once;
// after the grace, a laptop that stayed away is forgiven when every
// storage device and at least half of all are back; a storage device that
// stays away never is. The missing devices are named for the log.
func TestPeersBack(t *testing.T) {
	keys := make([]identity.Key, 5)
	for i := range keys {
		keys[i][0] = byte(i + 1)
	}
	have := func(ks ...identity.Key) map[string]bool {
		m := map[string]bool{}
		for _, k := range ks {
			m[k.String()] = true
		}
		return m
	}
	storage := have(keys[0])
	all := keys
	cases := []struct {
		name    string
		have    map[string]bool
		elapsed time.Duration
		ok      bool
		missing int
	}{
		{"all back at once", have(all...), 0, true, 0},
		{"one laptop away, before the grace", have(keys[0], keys[1], keys[2], keys[3]), probationGrace - time.Second, false, 1},
		{"one laptop away, after the grace", have(keys[0], keys[1], keys[2], keys[3]), probationGrace, true, 1},
		{"two laptops away of five, after the grace: three back is half rounded up", have(keys[0], keys[1], keys[2]), probationGrace, true, 2},
		{"three away of five: two back is not half", have(keys[0], keys[1]), probationDeadline, false, 3},
		{"the only storage device away, everyone else back, after the grace", have(keys[1], keys[2], keys[3], keys[4]), probationDeadline, false, 1},
	}
	for _, c := range cases {
		ok, missing := peersBack(all, c.have, storage, c.elapsed)
		if ok != c.ok || len(missing) != c.missing {
			t.Errorf("%s: ok=%v missing=%v", c.name, ok, missing)
		}
	}
	_, missing := peersBack(all, have(keys[1], keys[2], keys[3], keys[4]), storage, probationDeadline)
	if len(missing) != 1 || !strings.HasSuffix(missing[0], " (storage)") {
		t.Fatalf("the storage device away is named as such: %v", missing)
	}
	// Several storage devices: half of them is enough, so one alone does
	// not decide; most of them away does.
	three := have(keys[0], keys[1], keys[2])
	for _, c := range []struct {
		name    string
		have    map[string]bool
		elapsed time.Duration
		ok      bool
	}{
		{"one storage device of three away, before the grace", have(keys[1], keys[2], keys[3], keys[4]), probationGrace - time.Second, false},
		{"one storage device of three away, after the grace", have(keys[1], keys[2], keys[3], keys[4]), probationGrace, true},
		{"two storage devices of three away", have(keys[2], keys[3], keys[4]), probationDeadline, false},
		{"every storage device back, both laptops away: three of five is half", have(keys[0], keys[1], keys[2]), probationGrace, true},
	} {
		if ok, _ := peersBack(all, c.have, three, c.elapsed); ok != c.ok {
			t.Errorf("%s: ok=%v", c.name, ok)
		}
	}
	two := have(keys[0], keys[1])
	if ok, _ := peersBack(all, have(keys[1], keys[2], keys[3], keys[4]), two, probationGrace); !ok {
		t.Error("one storage device of two away: the other is half")
	}
	if ok, _ := peersBack(all, have(keys[2], keys[3], keys[4]), two, probationDeadline); ok {
		t.Error("both storage devices away passed")
	}
	if ok, _ := peersBack(nil, nil, nil, 0); !ok {
		t.Fatal("no devices to wait for is healthy")
	}
	if ok, _ := peersBack(keys[:1], have(), nil, probationDeadline); ok {
		t.Fatal("the only device away: one back of one is needed")
	}
	if ok, _ := peersBack(keys[:2], have(keys[1]), nil, probationGrace); !ok {
		t.Fatal("one of two back after the grace is half")
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
