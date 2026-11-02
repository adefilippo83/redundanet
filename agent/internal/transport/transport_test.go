package transport

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// testNet is a hub and a membership policy that knows a fixed set of
// devices, all on loopback.
type testNet struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	hub     *hub.Server
	hubKey  identity.Key
	mu      sync.RWMutex
	allowed map[identity.Key]bool
	census  map[identity.Key][]wire.Census
	log     *slog.Logger
}

func (n *testNet) censuses(k identity.Key) []wire.Census {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return append([]wire.Census(nil), n.census[k]...)
}

func (n *testNet) permit(k identity.Key) {
	n.mu.Lock()
	n.allowed[k] = true
	n.mu.Unlock()
}

func seed(b byte) identity.Seed {
	var s identity.Seed
	s[0] = b
	return s
}

func device(t *testing.T, s identity.Seed, roles ...string) (*identity.DeviceKey, *identity.TLSIdentity) {
	t.Helper()
	d, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := identity.Issue(s, identity.Certificate{Network: "testnet", Device: d.ID(), Roles: roles, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.NewTLSIdentity(d, sc)
	if err != nil {
		t.Fatal(err)
	}
	return d, id
}

func newTestNet(t *testing.T, tcp bool) *testNet {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n := &testNet{t: t, ctx: ctx, cancel: cancel, allowed: map[identity.Key]bool{}, census: map[identity.Key][]wire.Census{}}
	n.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if os.Getenv("TRANSPORT_TEST_LOG") != "" {
		n.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	hd, hid := device(t, seed(100), identity.RoleHub)
	n.hubKey = hd.ID()
	n.permit(n.hubKey)
	tcpAddr := ""
	if tcp {
		tcpAddr = "127.0.0.1:0"
	}
	onCensus := func(p identity.Peer, _ string, c wire.Census) {
		n.mu.Lock()
		n.census[p.Device] = append(n.census[p.Device], c)
		n.mu.Unlock()
	}
	h, err := hub.Start(ctx, hub.Config{Identity: hid, Device: hd.ID(), Allow: n.allow, UDP: "127.0.0.1:0", TCP: tcpAddr, OnCensus: onCensus, Log: n.log})
	if err != nil {
		t.Fatal(err)
	}
	n.hub = h
	t.Cleanup(func() { cancel(); _ = h.Close() })
	return n
}

func (n *testNet) allow(p identity.Peer) error {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.allowed[p.Device] {
		return errNotAllowed
	}
	return nil
}

var errNotAllowed = &notAllowed{}

type notAllowed struct{}

func (*notAllowed) Error() string { return "not in the manifest" }

// echo answers every stream with its own bytes.
func echo(_ identity.Peer, _ string, s *quic.Stream) {
	_, _ = io.Copy(s, s)
	_ = s.Close()
}

func (n *testNet) endpoint(s identity.Seed, allowed bool, edit func(*Config)) (*Endpoint, identity.Key) {
	n.t.Helper()
	d, id := device(n.t, s, identity.RoleClient, identity.RoleStorage)
	if allowed {
		n.permit(d.ID())
	}
	hubAddr := HubAddr{Key: n.hubKey, UDP: n.hub.UDPAddr().String()}
	if a := n.hub.TCPAddr(); a != nil {
		hubAddr.TCP = a.String()
	}
	cfg := Config{Identity: id, Device: d.ID(), Allow: n.allow, Hubs: []HubAddr{hubAddr}, Listen: "127.0.0.1:0", LoopbackCandidates: true, OnStream: echo, Log: n.log}
	if edit != nil {
		edit(&cfg)
	}
	ep, err := Start(n.ctx, cfg)
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { _ = ep.Close() })
	return ep, d.ID()
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func roundTrip(t *testing.T, ep *Endpoint, peer identity.Key, msg string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, path, err := ep.OpenStream(ctx, peer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	b, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != msg {
		t.Fatalf("echo over %s: got %q", path, b)
	}
	return path
}

func TestRelayThenDirect(t *testing.T) {
	n := newTestNet(t, false)
	a, _ := n.endpoint(seed(1), true, nil)
	b, bID := n.endpoint(seed(2), true, nil)
	_ = b
	s := a.Connect(bID)
	eventually(t, 10*time.Second, "a relay connection", func() bool { return s.Best() != "" })
	first := roundTrip(t, a, bID, "hello over whatever is up")
	eventually(t, 10*time.Second, "the direct path", func() bool { return s.Best() == PathDirect })
	if p := roundTrip(t, a, bID, "hello over the direct path"); p != PathDirect {
		t.Fatalf("stream went over %s", p)
	}
	eventually(t, 10*time.Second, "both paths", func() bool {
		st := a.Status()
		return len(st.Sessions) == 1 && st.Sessions[0].Relay && st.Sessions[0].Direct != ""
	})
	if st := a.Status(); st.Hubs[0].Carrier != "udp" || st.Hubs[0].Observed == "" {
		t.Fatalf("hub status: %+v", st.Hubs[0])
	}
	t.Logf("first stream over %s, then direct", first)
	// The listener side learned who the peer is.
	eventually(t, 5*time.Second, "the peer's member on the listener side", func() bool {
		bs := b.Status()
		return len(bs.Sessions) == 1 && bs.Sessions[0].Member == seed(1).MemberKey().String()
	})
}

func TestDirectLossFallsBackToRelay(t *testing.T) {
	n := newTestNet(t, false)
	a, _ := n.endpoint(seed(1), true, nil)
	_, bID := n.endpoint(seed(2), true, nil)
	s := a.Connect(bID)
	eventually(t, 10*time.Second, "the direct path", func() bool { return s.Best() == PathDirect })
	s.mu.Lock()
	direct := s.directConn
	s.mu.Unlock()
	_ = direct.CloseWithError(0, "cable pulled")
	eventually(t, 5*time.Second, "fallback to the relay", func() bool { return s.Best() == PathRelay })
	if p := roundTrip(t, a, bID, "still here"); p != PathRelay {
		t.Fatalf("stream went over %s", p)
	}
}

func TestStrangerIsRefused(t *testing.T) {
	n := newTestNet(t, false)
	a, aID := n.endpoint(seed(1), true, nil)
	x, xID := n.endpoint(seed(3), false, nil)
	eventually(t, 10*time.Second, "the stranger's hub channel to fail", func() bool {
		st := x.Status()
		return len(st.Hubs) == 1 && !st.Hubs[0].Up && st.Hubs[0].LastError != ""
	})
	// The stranger cannot reach a member directly either: a's listener
	// refuses the handshake, so no session ever gets a connection.
	x.Connect(aID)
	time.Sleep(2 * time.Second)
	if len(a.Status().Sessions) != 0 {
		t.Fatal("a member accepted a stranger")
	}
	_ = xID
}

func TestTCPCarrier(t *testing.T) {
	n := newTestNet(t, true)
	_, aID := n.endpoint(seed(1), true, nil)
	c, _ := n.endpoint(seed(4), true, func(cfg *Config) { cfg.TCPOnly = true })
	eventually(t, 10*time.Second, "the TCP channel", func() bool {
		st := c.Status()
		return len(st.Hubs) == 1 && st.Hubs[0].Up && st.Hubs[0].Carrier == "tcp"
	})
	s := c.Connect(aID)
	eventually(t, 10*time.Second, "a connection through the TCP-carried relay", func() bool { return s.Best() != "" })
	roundTrip(t, c, aID, "over tcp 443, so to speak")
	hs := n.hub.Status()
	if len(hs.Clients) != 2 {
		t.Fatalf("hub clients: %d", len(hs.Clients))
	}
}

func TestRecheckClosesTheNoLongerAdmitted(t *testing.T) {
	n := newTestNet(t, false)
	a, _ := n.endpoint(seed(1), true, nil)
	_, bID := n.endpoint(seed(2), true, nil)
	s := a.Connect(bID)
	eventually(t, 10*time.Second, "the direct path", func() bool { return s.Best() == PathDirect })
	n.mu.Lock()
	delete(n.allowed, bID)
	n.mu.Unlock()
	a.Recheck()
	eventually(t, 5*time.Second, "both connections to close", func() bool { return s.Best() == "" })
}

func TestCensusReachesTheHub(t *testing.T) {
	old := censusEvery.Load()
	censusEvery.Store(int64(200 * time.Millisecond))
	t.Cleanup(func() { censusEvery.Store(old) })
	n := newTestNet(t, false)
	a, ak := n.endpoint(seed(1), true, func(c *Config) {
		c.Census = func() wire.Census {
			return wire.Census{Version: "test", Roles: []string{identity.RoleStorage}, Storage: &wire.CensusStorage{FreeBytes: 5, TotalBytes: 10, State: "ok"}}
		}
	})
	_, bk := n.endpoint(seed(2), true, nil)
	a.Connect(bk)
	eventually(t, 10*time.Second, "a census from a naming b", func() bool {
		for _, c := range n.censuses(ak) {
			if c.Version != "test" || c.Storage == nil || c.Storage.FreeBytes != 5 {
				continue
			}
			for _, p := range c.Peers {
				if p.Device == bk && (p.Relay || p.Direct) {
					return true
				}
			}
		}
		return false
	})
	if len(n.censuses(bk)) != 0 {
		t.Fatal("an endpoint without a census source must send none")
	}
}
