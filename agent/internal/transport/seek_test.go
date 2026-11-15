package transport

import (
	"errors"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// simRouter is a home router for tests: the devices behind it have no
// address of their own on the loopback, and every public port the router
// hands out is a real socket there. It lets a packet in only from an
// address the socket behind it wrote to. One that counts gives a socket
// the next port for every destination; a fixed one gives a socket one
// port for all of them.
type simRouter struct {
	fixed bool

	mu     sync.Mutex
	next   int // the last public port handed out
	inside int // sockets opened behind it
}

func newSimRouter(fixed bool, base int) *simRouter {
	return &simRouter{fixed: fixed, next: base + rand.IntN(1500)}
}

// open hands out the next public port that is free.
func (r *simRouter) open() *net.UDPConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		r.next++
		if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.next}); err == nil {
			return c
		}
	}
}

// burn gives n ports to something else in the household.
func (r *simRouter) burn(n int) {
	r.mu.Lock()
	r.next += n
	r.mu.Unlock()
}

// socket is a socket of a device behind the router.
func (r *simRouter) socket() (net.PacketConn, error) {
	r.mu.Lock()
	r.inside++
	local := &net.UDPAddr{IP: net.IPv4(10, 99, 0, 2), Port: 4000 + r.inside}
	r.mu.Unlock()
	return &simSocket{r: r, local: local, inbox: make(chan packet, 1024), closed: make(chan struct{}),
		public: map[string]*net.UDPConn{}, sent: map[string]bool{}, moved: make(chan struct{})}, nil
}

type simSocket struct {
	r      *simRouter
	local  *net.UDPAddr
	inbox  chan packet
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	public   map[string]*net.UDPConn // the public port by destination; by "" behind a fixed router
	sent     map[string]bool         // the destinations written to
	deadline time.Time
	moved    chan struct{} // closed when the deadline changes
}

func (s *simSocket) WriteTo(b []byte, addr net.Addr) (int, error) {
	to, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, net.ErrClosed
	}
	key := to.String()
	if s.r.fixed {
		key = ""
	}
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return 0, net.ErrClosed
	default:
	}
	pub := s.public[key]
	if pub == nil {
		pub = s.r.open()
		s.public[key] = pub
		go s.read(pub, key)
	}
	s.sent[to.String()] = true
	s.mu.Unlock()
	// The world of these tests is the loopback: a packet for anywhere
	// else takes a port on the router and goes nowhere.
	if to.IP.IsLoopback() {
		_, _ = pub.WriteToUDP(b, to)
	}
	return len(b), nil
}

func (s *simSocket) read(pub *net.UDPConn, key string) {
	buf := make([]byte, 4096)
	for failed := 0; failed < 1000; {
		n, from, err := pub.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			failed++ // Windows reports a port nobody listens on here
			continue
		}
		s.mu.Lock()
		ok := s.sent[from.String()] && (key == "" || key == from.String())
		s.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case s.inbox <- packet{b: append([]byte(nil), buf[:n]...), addr: from}:
		default:
		}
	}
}

func (s *simSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		s.mu.Lock()
		deadline, moved := s.deadline, s.moved
		s.mu.Unlock()
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			t := time.NewTimer(time.Until(deadline))
			defer t.Stop()
			timeout = t.C
		}
		select {
		case p := <-s.inbox:
			return copy(b, p.b), p.addr, nil
		case <-s.closed:
			return 0, nil, net.ErrClosed
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		case <-moved:
		}
	}
}

func (s *simSocket) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		close(s.closed)
		for _, pub := range s.public {
			_ = pub.Close()
		}
		s.mu.Unlock()
	})
	return nil
}

func (s *simSocket) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	close(s.moved)
	s.moved = make(chan struct{})
	s.mu.Unlock()
	return nil
}

func (s *simSocket) LocalAddr() net.Addr              { return s.local }
func (s *simSocket) SetDeadline(t time.Time) error    { return s.SetReadDeadline(t) }
func (s *simSocket) SetWriteDeadline(time.Time) error { return nil }
func (s *simSocket) SetReadBuffer(int) error          { return nil }
func (s *simSocket) SetWriteBuffer(int) error         { return nil }

// behind puts a device behind the router, and has it search after a
// moment instead of the twenty seconds ordinary punching gets.
func (r *simRouter) behind(c *Config) {
	c.socket, _ = r.socket()
	c.fresh, c.seekAfter = r.socket, 300*time.Millisecond
}

func mapped(ep *Endpoint, want ...string) func() bool {
	return func() bool { return strings.Contains(" "+strings.Join(want, " ")+" ", " "+ep.NAT().Mapping+" ") }
}
func searched(ep *Endpoint, peer identity.Key) []string { return eventsWith(ep, peer, "port search") }

func eventsWith(ep *Endpoint, peer identity.Key, what string) []string {
	var out []string
	for _, s := range ep.Status().Sessions {
		if s.Peer != peer.String() {
			continue
		}
		for _, ev := range s.Events {
			if strings.Contains(ev.What, what) {
				out = append(out, ev.What)
			}
		}
	}
	return out
}

// Two devices behind routers that each give another public port for every
// destination, in order, and let in only what was written to: a punch at
// the address the hub saw goes nowhere. The search finds the pair of
// ports that point at each other, also while the households behind both
// routers keep opening connections of their own.
func TestTwoRoutersThatCountAreSearched(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "quiet"
		if busy {
			name = "busy"
		}
		t.Run(name, func(t *testing.T) {
			n := newTestNetProbe(t, false, "127.0.0.1:0")
			ra, rb := newSimRouter(false, 21000), newSimRouter(false, 25000)
			a, aKey := n.endpoint(seed(1), true, ra.behind)
			b, bKey := n.endpoint(seed(2), true, rb.behind)
			eventually(t, 15*time.Second, "both routers to be told apart", func() bool {
				return mapped(a, wire.MapSequential)() && mapped(b, wire.MapSequential)()
			})
			if busy {
				stop := make(chan struct{})
				t.Cleanup(func() { close(stop) })
				go func() {
					for {
						select {
						case <-stop:
							return
						case <-time.After(200 * time.Millisecond):
							ra.burn(1)
							rb.burn(1 + rand.IntN(2))
						}
					}
				}()
			}
			a.Connect(bKey)
			eventually(t, 30*time.Second, "a direct connection", func() bool { return a.Connect(bKey).Best() == PathDirect })
			if path := roundTrip(t, a, bKey, "through two routers"); path != PathDirect {
				t.Fatalf("the stream went over %s", path)
			}
			eventually(t, 10*time.Second, "both sides' word", func() bool {
				return len(searched(a, bKey)) > 0 && len(searched(b, aKey)) > 0
			})
			for _, got := range [][]string{searched(a, bKey), searched(b, aKey)} {
				if !strings.Contains(got[len(got)-1], "both routers' next ports: direct") {
					t.Fatalf("the search: %q", got)
				}
			}
		})
	}
}

// One device behind a router that keeps one public port, the other behind
// one that counts: the first writes to the ports the second names, which
// are around where its router's counter stood when it first wrote to the
// first. Whichever of the two dials.
func TestAFixedPortFindsARouterThatCounts(t *testing.T) {
	for _, fixedDials := range []bool{true, false} {
		name := "the fixed side dials"
		if !fixedDials {
			name = "the counting side dials"
		}
		t.Run(name, func(t *testing.T) {
			n := newTestNetProbe(t, false, "127.0.0.1:0")
			ra, rb := newSimRouter(true, 21000), newSimRouter(false, 25000)
			a, aKey := n.endpoint(seed(1), true, ra.behind)
			b, bKey := n.endpoint(seed(2), true, rb.behind)
			// The fixed router's one port is on this machine's own
			// address, which the probe reads as no router at all.
			eventually(t, 15*time.Second, "both routers to be told apart", func() bool {
				return mapped(a, wire.MapIndependent, wire.MapNone)() && mapped(b, wire.MapSequential)()
			})
			rb.burn(40) // the household goes on while nobody searches
			from, toKey := a, bKey
			if !fixedDials {
				from, toKey = b, aKey
			}
			from.Connect(toKey)
			eventually(t, 30*time.Second, "a direct connection", func() bool { return from.Connect(toKey).Best() == PathDirect })
			if path := roundTrip(t, from, toKey, "through two routers"); path != PathDirect {
				t.Fatalf("the stream went over %s", path)
			}
			eventually(t, 10*time.Second, "both sides' word", func() bool {
				return len(searched(a, bKey)) > 0 && len(searched(b, aKey)) > 0
			})
			if got := searched(a, bKey); !strings.Contains(got[0], "the peer's ports: direct") {
				t.Fatalf("the fixed side: %q", got)
			}
			if got := searched(b, aKey); !strings.Contains(got[0], "this device's ports: direct, 0 packets") {
				t.Fatalf("the counting side: %q", got)
			}
		})
	}
}

// The packets of a search between two routers that count, on paper: each
// packet to a port not written to before takes the router's next port.
// However many ports the two routers gave away before the search, up to
// twice what the side that started writes to spare, a run of mappings
// points at each other.
func TestInOrderMeets(t *testing.T) {
	const here, there = 10000, 40000
	for lost := 0; lost <= 2*seekSpare; lost++ {
		for _, split := range []int{0, lost / 2, lost} {
			// The side that started lost split ports since its reading, the other the rest.
			mine, theirs := map[uint16]uint16{}, map[uint16]uint16{} // public port to destination
			port := uint16(here + split)
			for _, to := range inOrder(there, false) {
				port++
				mine[port] = to
			}
			port = uint16(there + lost - split)
			for _, to := range inOrder(here, true) {
				port++
				theirs[port] = to
			}
			met := 0
			for p, to := range mine {
				if theirs[to] == p {
					met++
				}
			}
			if met != seekRun {
				t.Fatalf("%d ports lost, %d of them here: %d pairs meet, want %d", lost, split, met, seekRun)
			}
		}
	}
	if n := len(inOrder(65500, false)); n != 35 {
		t.Fatalf("past the last port: %d packets", n)
	}
}

// What a search may ask of a device is cut twice, by the hub and by the
// device: a handful of spans, a bounded number of ports, none past the
// last one.
func TestSpansAreBounded(t *testing.T) {
	got := wire.Spans([]wire.PortSpan{{First: 0, Count: 9}, {First: 65530, Count: 100}, {First: 100, Count: 0}, {First: 1, Count: 800}, {First: 2000, Count: 800}, {First: 3000, Count: 1}})
	want := []wire.PortSpan{{First: 65530, Count: 6}, {First: 1, Count: 800}, {First: 2000, Count: 218}}
	if len(got) != len(want) {
		t.Fatalf("spans: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spans: %v", got)
		}
	}
	if n := len(spanPorts(make([]wire.PortSpan, 0))); n != 0 {
		t.Fatalf("no spans: %d ports", n)
	}
	var six []wire.PortSpan
	for first := uint16(1); first <= 6; first++ {
		six = append(six, wire.PortSpan{First: first, Count: 1})
	}
	if n := len(spanPorts(six)); n != wire.MaxSeekSpans {
		t.Fatalf("six spans: %d ports", n)
	}
}

// The address in a search message is the one the hub sees its sender at,
// whatever the sender wrote, and the spans are cut on the way: a device
// cannot point a peer's packets at somebody else.
func TestTheHubNamesTheAddressOfASearch(t *testing.T) {
	n := newTestNetProbe(t, false, "127.0.0.1:0")
	a, aKey := n.endpoint(seed(1), true, nil)
	b, bKey := n.endpoint(seed(2), true, nil)
	a.Connect(bKey)
	eventually(t, 15*time.Second, "a connection and both probes", func() bool {
		return a.Connect(bKey).Best() != "" && !a.NAT().Checked.IsZero() && !b.NAT().Checked.IsZero()
	})
	// The two are connected directly here, and a device with a direct
	// connection answers no search: take it away from the side that is asked.
	sb := b.session(aKey, false)
	sb.mu.Lock()
	direct := sb.directConn
	sb.directConn = nil
	sb.mu.Unlock()
	t.Cleanup(func() { sb.mu.Lock(); sb.directConn = direct; sb.mu.Unlock() })

	sa := a.session(bKey, false)
	answer := make(chan wire.Seek, 1)
	sa.mu.Lock()
	hub := sa.hub
	sa.seekRound, sa.seekReply = 7, answer
	sa.mu.Unlock()
	var spans []wire.PortSpan
	for i := 0; i < 9; i++ {
		spans = append(spans, wire.PortSpan{First: 1000, Count: 900})
	}
	forged := wire.Seek{Peer: bKey, Round: 7, Addr: "203.0.113.9", Mapping: wire.MapRandom, Next: 4000, Spans: spans}
	if err := hub.sendMessage(wire.FrameSeek, forged); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answer:
		if got.Peer != bKey || !got.Reply || got.Addr != "127.0.0.1" {
			t.Fatalf("the answer: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer to the search")
	}
	// A router that picks any port cannot be searched for, and the asked
	// side says so instead of writing anywhere.
	eventually(t, 10*time.Second, "the asked side's word", func() bool { return len(eventsWith(b, aKey, "port search: no way")) > 0 })
}
