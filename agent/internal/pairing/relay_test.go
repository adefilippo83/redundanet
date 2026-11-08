package pairing

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func quietRelay() *Relay { return NewRelay(slog.New(slog.NewTextHandler(io.Discard, nil))) }

// Two devices that cannot reach each other pair through the hub: the code
// names a dead address and the relay, and the exchange completes over the
// mailbox with the same sealed frames.
func TestPairThroughTheRelay(t *testing.T) {
	relay := quietRelay()
	hub := httptest.NewServer(relay)
	defer hub.Close()
	dev, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", dev.ID(), "127.0.0.1:0", []string{hub.URL})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(l.Code())
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != version || len(c.Relays) != 1 {
		t.Fatalf("code %+v", c)
	}
	c.Addrs = []string{"127.0.0.1:1"} // nobody listens there
	code := c.String()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		offer *Offer
		err   error
	}
	got := make(chan result, 1)
	go func() { o, err := l.Wait(ctx); got <- result{o, err} }()
	paired, name, err := Pair(ctx, code, "testnet", func(d identity.Key, _ string) (*Offer, error) {
		if d != dev.ID() {
			t.Errorf("certifying %s, the new device is %s", d.Short(), dev.ID().Short())
		}
		return &Offer{State: []byte("the identity"), Descriptor: "rn3:net:xyz"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil || string(r.offer.State) != "the identity" || r.offer.Descriptor != "rn3:net:xyz" {
		t.Fatalf("listener got %+v %v", r.offer, r.err)
	}
	if paired != dev.ID() || name == "" {
		t.Fatalf("paired %s named %q", paired.Short(), name)
	}
	if relay.Rooms() != 1 {
		t.Fatalf("%d rooms after one pairing", relay.Rooms())
	}
}

// A hub without the mailbox (an older release) is reported, not retried
// forever; a wrong code through the relay reveals nothing and fails.
func TestRelayRefusals(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	dev, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", dev.ID(), "127.0.0.1:0", []string{old.URL})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Parse(l.Code())
	c.Addrs = []string{"127.0.0.1:1"}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second) // the wrong code below waits for a hello that never comes
	defer cancel()
	_, _, err = Pair(ctx, c.String(), "testnet", nil)
	if err == nil || !strings.Contains(err.Error(), "3.0.10") {
		t.Fatalf("against an old hub: %v", err)
	}
	l.Close()

	relay := quietRelay()
	hub := httptest.NewServer(relay)
	defer hub.Close()
	right, _ := identity.NewDeviceKey()
	l2, err := Listen("testnet", right.ID(), "127.0.0.1:0", []string{hub.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	rc, _ := Parse(l2.Code())
	wrong := &Code{Version: version, Addrs: []string{"127.0.0.1:1"}, Secret: make([]byte, secretBytes), Relays: rc.Relays}
	called := false
	_, _, err = Pair(ctx, wrong.String(), "testnet", func(identity.Key, string) (*Offer, error) { called = true; return nil, nil })
	if err == nil || called {
		t.Fatalf("a wrong code through the relay: err %v, certify called %v", err, called)
	}
}

// The mailbox itself: bad rooms, oversized frames, full queues, empty
// polls and the cap on rooms.
func TestRelayMailboxLimits(t *testing.T) {
	relay := quietRelay()
	hub := httptest.NewServer(relay)
	defer hub.Close()
	post := func(path string, body []byte) int {
		resp, err := http.Post(hub.URL+path, "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	room := roomID([]byte("a secret"))
	if code := post("/pair/not-a-room/new", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("bad room: %d", code)
	}
	if code := post("/pair/"+room+"/sideways", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("bad side: %d", code)
	}
	if code := post("/pair/"+room+"/new", make([]byte, maxFrame+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized frame: %d", code)
	}
	for i := 0; i < queueDepth; i++ {
		if code := post("/pair/"+room+"/new", []byte("frame")); code != http.StatusNoContent {
			t.Fatalf("frame %d: %d", i, code)
		}
	}
	if code := post("/pair/"+room+"/new", []byte("one too many")); code != http.StatusTooManyRequests {
		t.Errorf("full queue: %d", code)
	}
	resp, err := http.Get(hub.URL + "/pair/" + room + "/existing?wait=0")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("empty poll: %d", resp.StatusCode)
	}
	resp, err = http.Get(hub.URL + "/pair/" + room + "/new?wait=0")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "frame" {
		t.Errorf("first frame back: %d %q", resp.StatusCode, b)
	}
	for i := 0; relay.Rooms() < maxRooms; i++ {
		post("/pair/"+roomID([]byte{byte(i), byte(i >> 8)})+"/new", []byte("x"))
	}
	if code := post("/pair/"+roomID([]byte("one more"))+"/new", []byte("x")); code != http.StatusServiceUnavailable {
		t.Errorf("room cap: %d", code)
	}
	// Rooms expire.
	relay.now = func() time.Time { return time.Now().Add(roomTTL + time.Minute) }
	if relay.Rooms() != 0 {
		t.Errorf("%d rooms after the TTL", relay.Rooms())
	}
}

// A version 1 code, from a device that predates the relays, still parses.
func TestCodeVersionOneStillParses(t *testing.T) {
	c := &Code{Version: 1, Addrs: []string{"192.0.2.1:4000"}, Secret: make([]byte, secretBytes)}
	back, err := Parse(c.String())
	if err != nil || back.Version != 1 || len(back.Relays) != 0 {
		t.Fatalf("version 1: %+v %v", back, err)
	}
	if _, err := Parse((&Code{Version: version + 1, Addrs: c.Addrs, Secret: c.Secret}).String()); err == nil {
		t.Fatal("a code from the future parsed")
	}
}
