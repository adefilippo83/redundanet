package pairing

import (
	"bytes"
	"context"
	"fmt"
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
	l, err := Listen("testnet", dev, "127.0.0.1:0", []string{hub.URL})
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
	l, err := Listen("testnet", dev, "127.0.0.1:0", []string{old.URL})
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
	l2, err := Listen("testnet", right, "127.0.0.1:0", []string{hub.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	rc, _ := Parse(l2.Code())
	wrong := &Code{Version: version, Addrs: []string{"127.0.0.1:1"}, Secret: make([]byte, secretBytes), Relays: rc.Relays, Device: rc.Device}
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
	// A room holds a bounded amount nobody has read: two frames of the
	// largest size, not a full queue of them.
	big := roomID([]byte("big frames"))
	for i, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		if code := post("/pair/"+big+"/new", make([]byte, maxFrame)); code != want {
			t.Errorf("large frame %d: %d, want %d", i, code, want)
		}
	}
	resp, err = http.Get(hub.URL + "/pair/" + big + "/new?wait=0")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if code := post("/pair/"+big+"/existing", make([]byte, maxFrame)); code != http.StatusNoContent {
		t.Errorf("a frame after one was read: %d", code)
	}
	// One address opens a bounded number of rooms; a room that exists is
	// still found from it. The test server is reached over loopback, as
	// the hub is behind its proxy, so the forwarded address names the source.
	from := func(src, path string) int {
		req, _ := http.NewRequest(http.MethodPost, hub.URL+path, bytes.NewReader([]byte("x")))
		req.Header.Set("X-Forwarded-For", "198.51.100.1, "+src)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < maxPerSource; i++ {
		if code := from("203.0.113.9", "/pair/"+roomID([]byte{'s', byte(i)})+"/new"); code != http.StatusNoContent {
			t.Fatalf("room %d of one address: %d", i, code)
		}
	}
	if code := from("203.0.113.9", "/pair/"+roomID([]byte("one more"))+"/new"); code != http.StatusServiceUnavailable {
		t.Errorf("a room beyond the bound of one address: %d", code)
	}
	if code := from("203.0.113.9", "/pair/"+roomID([]byte{'s', 0})+"/existing"); code != http.StatusNoContent {
		t.Errorf("an open room from an address at its bound: %d", code)
	}
	if code := from("2001:db8:0:1::7", "/pair/"+roomID([]byte("v6 a"))+"/new"); code != http.StatusNoContent {
		t.Errorf("another address: %d", code)
	}
	for i := 0; relay.Rooms() < maxRooms; i++ {
		if code := from(fmt.Sprintf("192.0.2.%d", i/maxPerSource+1), "/pair/"+roomID([]byte{byte(i), byte(i >> 8)})+"/new"); code != http.StatusNoContent {
			t.Fatalf("filling the hub, room %d: %d", i, code)
		}
	}
	if code := from("192.0.2.250", "/pair/"+roomID([]byte("past the cap"))+"/new"); code != http.StatusServiceUnavailable {
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
	var dev identity.Key
	dev[0] = 1
	if _, err := Parse((&Code{Version: version + 1, Addrs: c.Addrs, Secret: c.Secret, Device: dev}).String()); err == nil || !strings.Contains(err.Error(), "update this device") {
		t.Fatalf("a code from the future: %v", err)
	}
	// From version 3 a code names the new device's key, and an older one does not.
	if _, err := Parse((&Code{Version: version, Addrs: c.Addrs, Secret: c.Secret}).String()); err == nil {
		t.Fatal("a version 3 code without the device key parsed")
	}
	if _, err := Parse((&Code{Version: 2, Addrs: c.Addrs, Secret: c.Secret, Device: dev}).String()); err == nil {
		t.Fatal("a version 2 code with a device key parsed")
	}
	if back, err := Parse((&Code{Version: version, Addrs: c.Addrs, Secret: c.Secret, Device: dev}).String()); err != nil || !back.Bound() || back.Device != dev {
		t.Fatalf("version 3: %+v %v", back, err)
	}
}

// The source of a request is the connection's address, and the address
// the proxy on the same host wrote when the connection is from loopback.
// A forwarded address on a connection from elsewhere is not believed.
func TestSourceOfARequest(t *testing.T) {
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.5:4000", "", "203.0.113.5"},
		{"203.0.113.5:4000", "198.51.100.1", "203.0.113.5"},
		{"127.0.0.1:4000", "198.51.100.1", "198.51.100.1"},
		{"127.0.0.1:4000", "10.9.9.9, 198.51.100.2", "198.51.100.2"},
		{"127.0.0.1:4000", "not an address", "127.0.0.1"},
		{"127.0.0.1:4000", "", "127.0.0.1"},
		{"[::1]:4000", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2::9]:4000", "", "2001:db8:1:2::/64"},
		{"[::ffff:203.0.113.5]:4000", "", "203.0.113.5"},
		{"nonsense", "", "unknown"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/pair/x/new", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := source(req); got != c.want {
			t.Errorf("source of %s forwarded %q: %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}
