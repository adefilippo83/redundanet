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

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// simRouter is a home router for tests: the devices behind it have no
// address of their own on the loopback, and every public port the router
// hands out is a real socket there. It lets a packet in only from an
// address the socket behind it wrote to. One that counts gives a socket
// the next port for every destination; a fixed one gives a socket one
// port for all of them; one with a band gives a socket any port of the
// band for every destination, the same port to several destinations when
// it comes to that. A mapping nobody ever answered is gone when the
// router forgets.
type simRouter struct {
	fixed bool
	base  int
	band  int   // how many ports from base on, for a router that picks
	first []int // what a router that picks gives out first, from base on
	// scramble is how long the first packet to a destination may be on
	// its way to the router: packets written closer together than that
	// get their ports in any order.
	scramble time.Duration

	mu      sync.Mutex
	next    int // the last public port handed out
	inside  int // sockets opened behind it
	sockets []*simSocket
	ports   map[int]*simPort
}

// simPort is one public port: the socket on the loopback, and whose
// packets from where it lets in.
type simPort struct {
	conn *net.UDPConn

	mu     sync.Mutex
	routes map[string]*simSocket // by destination; by "" behind a fixed router
}

func newSimRouter(fixed bool, base int) *simRouter {
	return &simRouter{fixed: fixed, next: base + rand.IntN(1500), ports: map[int]*simPort{}}
}

// The first ports of a router that picks are set, far from each other and
// in no order: what the probe makes of ports that happen to be the same,
// or to follow each other, is not what these tests are about.
func newSimBandRouter(base, band int) *simRouter {
	first := []int{3, 31, 12, 38, 7, 25, 0, 19, 37, 15}
	for i := range first {
		first[i] = first[i] * band / 40
	}
	return &simRouter{base: base, band: band, first: first, ports: map[int]*simPort{}}
}

// open hands a socket a public port for one destination: the next one
// that is free, or any of the band that has no mapping to there yet.
func (r *simRouter) open(s *simSocket, key string) *simPort {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		r.next++
		if r.band > 0 {
			r.next = r.base + rand.IntN(r.band)
			if len(r.first) > 0 {
				r.next, r.first = r.base+r.first[0], r.first[1:]
			}
		}
		if p := r.ports[r.next]; p != nil {
			p.mu.Lock()
			_, taken := p.routes[key]
			if share := r.band > 0 && !taken; share {
				p.routes[key] = s
				p.mu.Unlock()
				return p
			}
			p.mu.Unlock()
			continue
		}
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.next})
		if err != nil {
			continue
		}
		p := &simPort{conn: c, routes: map[string]*simSocket{key: s}}
		r.ports[r.next] = p
		go p.read()
		return p
	}
}

// release ends one mapping, and the port with its last one.
func (r *simRouter) release(p *simPort, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.mu.Lock()
	delete(p.routes, key)
	empty := len(p.routes) == 0
	p.mu.Unlock()
	if empty {
		delete(r.ports, p.conn.LocalAddr().(*net.UDPAddr).Port)
		_ = p.conn.Close()
	}
}

func (p *simPort) read() {
	buf := make([]byte, 4096)
	for failed := 0; failed < 1000; {
		n, from, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			failed++ // Windows reports a port nobody listens on here
			continue
		}
		key := from.String()
		p.mu.Lock()
		s := p.routes[key]
		if s == nil {
			s, key = p.routes[""], ""
		}
		p.mu.Unlock()
		if s == nil {
			continue
		}
		s.mu.Lock()
		ok := s.sent[from.String()]
		if ok {
			s.answered[key] = true
		}
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

// burn gives n ports to something else in the household.
func (r *simRouter) burn(n int) {
	r.mu.Lock()
	r.next += n
	r.mu.Unlock()
}

// forget drops every mapping that got no answer in the time a router
// keeps one.
func (r *simRouter) forget(kept time.Duration) {
	r.mu.Lock()
	sockets := append([]*simSocket(nil), r.sockets...)
	r.mu.Unlock()
	for _, s := range sockets {
		s.mu.Lock()
		for key, pub := range s.public {
			if key != "" && !s.answered[key] && time.Since(s.born[key]) > kept {
				r.release(pub, key)
				delete(s.public, key)
				delete(s.sent, key)
			}
		}
		s.mu.Unlock()
	}
}

// socket is a socket of a device behind the router.
func (r *simRouter) socket() (net.PacketConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inside++
	s := &simSocket{r: r, local: &net.UDPAddr{IP: net.IPv4(10, 99, 0, 2), Port: 4000 + r.inside},
		inbox: make(chan packet, 1024), closed: make(chan struct{}),
		public: map[string]*simPort{}, sent: map[string]bool{}, answered: map[string]bool{}, born: map[string]time.Time{}, moved: make(chan struct{})}
	r.sockets = append(r.sockets, s)
	return s, nil
}

type simSocket struct {
	r      *simRouter
	local  *net.UDPAddr
	inbox  chan packet
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	public   map[string]*simPort // the public port by destination; by "" behind a fixed router
	sent     map[string]bool     // the destinations written to
	answered map[string]bool     // the mappings something came back on
	born     map[string]time.Time
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
	if pub == nil && s.r.scramble > 0 {
		s.mu.Unlock()
		held := append([]byte(nil), b...)
		go func() {
			time.Sleep(rand.N(s.r.scramble))
			s.mu.Lock()
			select {
			case <-s.closed:
				s.mu.Unlock()
				return
			default:
			}
			pub := s.public[key]
			if pub == nil {
				pub = s.r.open(s, key)
				s.public[key], s.born[key] = pub, time.Now()
			}
			s.sent[to.String()] = true
			s.mu.Unlock()
			if to.IP.IsLoopback() {
				_, _ = pub.conn.WriteToUDP(held, to)
			}
		}()
		return len(b), nil
	}
	if pub == nil {
		pub = s.r.open(s, key)
		s.public[key], s.born[key] = pub, time.Now()
	}
	s.sent[to.String()] = true
	s.mu.Unlock()
	// The world of these tests is the loopback: a packet for anywhere
	// else takes a port on the router and goes nowhere.
	if to.IP.IsLoopback() {
		_, _ = pub.conn.WriteToUDP(b, to)
	}
	return len(b), nil
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
		for key, pub := range s.public {
			s.r.release(pub, key)
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
						case <-time.After(40 * time.Millisecond):
							ra.burn(1 + rand.IntN(2))
							rb.burn(1 + rand.IntN(3))
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
			// The side that dials opens, the other aims, and each side's
			// word has what the other did.
			dials, answers := searched(a, bKey), searched(b, aKey)
			if len(eventsWith(a, bKey, "I am the dialer")) == 0 {
				dials, answers = answers, dials
			}
			if got := dials[len(dials)-1]; !strings.Contains(got, "opening the peer's next ports: direct, 160 packets") || !strings.Contains(got, "the peer wrote") {
				t.Fatalf("the side that opened: %q", dials)
			}
			if got := answers[len(answers)-1]; !strings.Contains(got, "aiming at the ports the peer opened: direct") || !strings.Contains(got, "the peer opened 160 ports in 10 runs") {
				t.Fatalf("the side that aimed: %q", answers)
			}
		})
	}
}

// One device's packets reach its router in any order when they are
// written within a few milliseconds of each other, as over a wireless
// link. Its probe notices, and it then writes the packets of a search
// one by one, whether it opens or aims.
func TestARouterReachedOutOfOrderIsSearchedOneByOne(t *testing.T) {
	for _, scrambledDials := range []bool{true, false} {
		name := "the device out of order dials"
		if !scrambledDials {
			name = "the device out of order answers"
		}
		t.Run(name, func(t *testing.T) {
			n := newTestNetProbe(t, false, "127.0.0.1:0")
			ra, rb := newSimRouter(false, 21000), newSimRouter(false, 25000)
			ra.scramble = 10 * time.Millisecond
			a, aKey := n.endpoint(seed(1), true, ra.behind)
			b, bKey := n.endpoint(seed(2), true, rb.behind)
			eventually(t, 15*time.Second, "both routers to be told apart", func() bool {
				return mapped(a, wire.MapSequential)() && mapped(b, wire.MapSequential)()
			})
			if !a.paced() || b.paced() {
				t.Fatalf("out of order: %v and %v, want true and false", a.paced(), b.paced())
			}
			from, toKey := a, bKey
			if !scrambledDials {
				from, toKey = b, aKey
			}
			from.Connect(toKey)
			eventually(t, 60*time.Second, "a direct connection", func() bool { return from.Connect(toKey).Best() == PathDirect })
			if path := roundTrip(t, from, toKey, "through two routers"); path != PathDirect {
				t.Fatalf("the stream went over %s", path)
			}
			eventually(t, 15*time.Second, "the word of the device out of order", func() bool {
				return len(eventsWith(a, bKey, "one by one")) > 0
			})
			got := eventsWith(a, bKey, "one by one")
			if !strings.Contains(got[len(got)-1], ": direct") {
				t.Fatalf("the search: %q", got)
			}
		})
	}
}

// A device of the version that guessed, and one of this version: the two
// search the way both know.
func TestAnOlderPeerIsSearchedTheOldWay(t *testing.T) {
	n := newTestNetProbe(t, false, "127.0.0.1:0")
	ra, rb := newSimRouter(false, 21000), newSimRouter(false, 25000)
	a, aKey := n.endpoint(seed(1), true, func(c *Config) { ra.behind(c); c.seekOld = true })
	b, bKey := n.endpoint(seed(2), true, rb.behind)
	eventually(t, 15*time.Second, "both routers to be told apart", func() bool {
		return mapped(a, wire.MapSequential)() && mapped(b, wire.MapSequential)()
	})
	a.Connect(bKey)
	eventually(t, 30*time.Second, "a direct connection", func() bool { return a.Connect(bKey).Best() == PathDirect })
	eventually(t, 10*time.Second, "both sides' word", func() bool {
		return len(searched(a, bKey)) > 0 && len(searched(b, aKey)) > 0
	})
	for _, got := range [][]string{searched(a, bKey), searched(b, aKey)} {
		if !strings.Contains(got[len(got)-1], "both routers' next ports: direct") {
			t.Fatalf("the search: %q", got)
		}
	}
}

// One router counts, the other gives each destination any port out of a
// narrow band. The first device opens a port of its own toward every port
// of the band; the second writes once to each of those, and the way is
// open when its router happens to give one packet the port its target was
// opened toward. That is about two searches in three, so the routers
// forget what nobody answered and the devices search again until it is.
func TestARouterWithABandIsSearched(t *testing.T) {
	old := seekBackoff
	for i := range seekBackoff {
		seekBackoff[i] = 1500 * time.Millisecond
	}
	t.Cleanup(func() { seekBackoff = old })
	for _, bandDials := range []bool{false, true} {
		name := "the counting side dials"
		if bandDials {
			name = "the side with the band dials"
		}
		t.Run(name, func(t *testing.T) {
			// The searches here follow each other within seconds and not
			// minutes, so the hub answers as many asks as they need.
			n := newTestNetProbe(t, false, "127.0.0.1:0", func(c *hub.Config) { c.ProbeAnswers = 1 << 16 })
			ra, rb := newSimRouter(false, 21000), newSimBandRouter(27000, 40)
			a, aKey := n.endpoint(seed(1), true, ra.behind)
			b, bKey := n.endpoint(seed(2), true, rb.behind)
			eventually(t, 15*time.Second, "both routers to be told apart", func() bool {
				return mapped(a, wire.MapSequential)() && mapped(b, wire.MapConfined)()
			})
			stop := make(chan struct{})
			t.Cleanup(func() { close(stop) })
			go func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(500 * time.Millisecond):
						ra.forget(3 * time.Second)
						rb.forget(3 * time.Second)
					}
				}
			}()
			from, toKey := a, bKey
			if bandDials {
				from, toKey = b, aKey
			}
			from.Connect(toKey)
			eventually(t, 90*time.Second, "a direct connection", func() bool { return from.Connect(toKey).Best() == PathDirect })
			if path := roundTrip(t, from, toKey, "through two routers"); path != PathDirect {
				t.Fatalf("the stream went over %s", path)
			}
			eventually(t, 10*time.Second, "both sides' word", func() bool {
				return len(eventsWith(a, bKey, "band of ports: direct")) > 0 && len(eventsWith(b, aKey, "every port the peer opened: direct")) > 0
			})
		})
	}
}

// What a device says it opened is cut like the spans: a bounded number of
// runs and of ports, none past the last port on either side, and a run
// without a port of its own is not exact.
func TestOpeningsAreBounded(t *testing.T) {
	got := wire.Openings([]wire.Opening{{To: 0, Count: 5, From: 9}, {To: 65530, Count: 100, From: 100}, {To: 100, Count: 50, From: 65530, Exact: true},
		{To: 200, Count: 16, Exact: true}, {To: 1000, Count: 2000, From: 2000}, {To: 5000, Count: 1, From: 1}})
	want := []wire.Opening{{To: 65530, Count: 6, From: 100}, {To: 100, Count: 6, From: 65530, Exact: true}, {To: 200, Count: 16}, {To: 1000, Count: 996, From: 2000}}
	if len(got) != len(want) {
		t.Fatalf("openings: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("openings: %+v", got)
		}
	}
	var many []wire.Opening
	for i := 0; i < 2*wire.MaxSeekOpenings; i++ {
		many = append(many, wire.Opening{To: uint16(1000 + i), Count: 1, From: 7})
	}
	if n := len(wire.Openings(many)); n != wire.MaxSeekOpenings {
		t.Fatalf("%d runs", n)
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
	// On one machine the two connect directly, and a device with a direct
	// connection answers no search: wait until the side that is asked has
	// it, so that it cannot arrive later, and take it away.
	var sb *session
	eventually(t, 20*time.Second, "the direct connection and both probes", func() bool {
		if sb = b.session(aKey, false); sb == nil {
			return false
		}
		sb.mu.Lock()
		direct := sb.directConn != nil
		sb.mu.Unlock()
		return direct && a.Connect(bKey).Best() == PathDirect && !a.NAT().Checked.IsZero() && !b.NAT().Checked.IsZero()
	})
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
