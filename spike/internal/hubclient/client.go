// Package hubclient keeps a device's channel to the hub: QUIC over the shared
// UDP socket when the hub answers there, TLS over TCP when UDP is blocked.
// The channel carries rendezvous messages and relayed packets.
package hubclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/spike/internal/identity"
	"github.com/adefilippo83/redundanet/spike/internal/mux"
	"github.com/adefilippo83/redundanet/spike/internal/wire"
)

// Config wires the client to the rest of the agent.
type Config struct {
	Key       *identity.Key
	HubID     identity.ID
	UDPAddr   string // hub QUIC listener, "" to skip
	TCPAddr   string // hub TLS listener, "" to skip
	TCPOnly   bool   // never try UDP: simulates a network that blocks it
	Transport *quic.Transport
	Mux       *mux.Mux
	Locals    func() []string
	// OnCandidates is called for every rendezvous answer the hub sends.
	OnCandidates func(wire.Candidates)
	// OnUp is called each time the channel comes up, so pending rendezvous
	// requests can be repeated.
	OnUp func()
	Log  *log.Logger
}

// Status is what the local page shows about the channel.
type Status struct {
	Up         bool      `json:"up"`
	Carrier    string    `json:"carrier,omitempty"`
	Observed   string    `json:"observed,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Reconnects int       `json:"reconnects"`
	LastError  string    `json:"last_error,omitempty"`
}

// Client is the hub channel with reconnection.
type Client struct {
	cfg Config

	mu      sync.Mutex
	w       *wire.Writer
	conn    *quic.Conn // set when the channel runs over QUIC: relay packets go as datagrams
	out     chan []byte
	status  Status
	dropped uint64 // relay packets dropped because the outbound queue was full
}

// relayQueue bounds the packets waiting for the hub: a congested hub leg
// turns into loss the end-to-end connection can react to, not into seconds of
// buffering it cannot see.
const relayQueue = 256

// New builds a client; Run connects it.
func New(cfg Config) *Client { return &Client{cfg: cfg} }

// Status returns a snapshot.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Connect asks the hub for a rendezvous with peer.
func (c *Client) Connect(peer identity.ID) error {
	c.mu.Lock()
	w := c.w
	c.mu.Unlock()
	if w == nil {
		return errors.New("hub channel is down")
	}
	return w.JSON(wire.FrameConnect, wire.Connect{Peer: peer.String()})
}

// sendRelay queues one end-to-end packet for the hub and never blocks: the
// packet is copied (quic-go reuses its buffers) and dropped when the queue
// is full.
func (c *Client) sendRelay(peer identity.ID, pkt []byte) error {
	c.mu.Lock()
	out := c.out
	c.mu.Unlock()
	if out == nil {
		return errors.New("hub channel is down")
	}
	b := make([]byte, len(peer)+len(pkt))
	copy(b, peer[:])
	copy(b[len(peer):], pkt)
	select {
	case out <- b:
		return nil
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
		return nil
	}
}

// Dropped counts relay packets dropped for a full queue.
func (c *Client) Dropped() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// writeRelay drains the queue: QUIC datagrams when the channel is QUIC,
// relay frames on the stream otherwise or when a packet does not fit.
func (c *Client) writeRelay(conn *quic.Conn, w *wire.Writer, out chan []byte, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case b := <-out:
			if conn != nil {
				err := conn.SendDatagram(b)
				if err == nil {
					continue
				}
				// Too large, or the peer has no datagram support: the
				// stream carries this packet instead.
			}
			var id identity.ID
			copy(id[:], b)
			if err := w.Relay(id, b[len(id):]); err != nil {
				return
			}
		}
	}
}

// Run keeps the channel up until ctx ends.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		c.status.Up = false
		c.status.Reconnects++
		if err != nil {
			c.status.LastError = err.Error()
		}
		c.mu.Unlock()
		c.cfg.Log.Printf("hub: channel down (%v), retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

type carrier struct {
	name string
	rw   io.ReadWriter
	conn *quic.Conn // nil over TCP
	stop func()
}

func (c *Client) session(ctx context.Context) error {
	car, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer car.stop()
	w := wire.NewWriter(car.rw)
	if err := w.JSON(wire.FrameHello, wire.Hello{Locals: c.cfg.Locals()}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	typ, payload, err := wire.ReadFrame(car.rw)
	if err != nil {
		return fmt.Errorf("welcome: %w", err)
	}
	if typ != wire.FrameWelcome {
		return fmt.Errorf("welcome: got frame type %d", typ)
	}
	var welcome wire.Welcome
	if err := decode(payload, &welcome); err != nil {
		return err
	}
	out := make(chan []byte, relayQueue)
	done := make(chan struct{})
	defer close(done)
	c.mu.Lock()
	c.w = w
	c.conn = car.conn
	c.out = out
	c.status = Status{Up: true, Carrier: car.name, Observed: welcome.Observed, Since: time.Now(), Reconnects: c.status.Reconnects}
	c.mu.Unlock()
	go c.writeRelay(car.conn, w, out, done)
	if car.conn != nil {
		go func() {
			for {
				b, err := car.conn.ReceiveDatagram(ctx)
				if err != nil {
					return
				}
				from, pkt, err := wire.SplitRelay(b)
				if err != nil {
					continue
				}
				c.cfg.Mux.DeliverRelayed(from, pkt)
			}
		}()
	}
	c.cfg.Mux.SetRelay(c.sendRelay)
	c.cfg.Log.Printf("hub: channel up over %s, hub sees me as %s", car.name, welcome.Observed)
	if c.cfg.OnUp != nil {
		go c.cfg.OnUp()
	}
	defer func() {
		c.cfg.Mux.SetRelay(nil)
		c.mu.Lock()
		c.w = nil
		c.conn = nil
		c.out = nil
		c.mu.Unlock()
	}()

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				if err := w.Frame(wire.FramePing, nil); err != nil {
					return
				}
			}
		}
	}()

	for {
		typ, payload, err := wire.ReadFrame(car.rw)
		if err != nil {
			return err
		}
		switch typ {
		case wire.FrameCandidates:
			var cand wire.Candidates
			if err := decode(payload, &cand); err != nil {
				return err
			}
			c.cfg.OnCandidates(cand)
		case wire.FrameRelay:
			from, pkt, err := wire.SplitRelay(payload)
			if err != nil {
				return err
			}
			c.cfg.Mux.DeliverRelayed(from, pkt)
		case wire.FrameError:
			var e wire.Error
			_ = decode(payload, &e)
			c.cfg.Log.Printf("hub: error: %s", e.Msg)
		case wire.FramePing:
			_ = w.Frame(wire.FramePong, nil)
		case wire.FramePong:
		default:
			c.cfg.Log.Printf("hub: unknown frame type %d", typ)
		}
	}
}

func (c *Client) dial(ctx context.Context) (*carrier, error) {
	var firstErr error
	if c.cfg.UDPAddr != "" && !c.cfg.TCPOnly {
		car, err := c.dialQUIC(ctx)
		if err == nil {
			return car, nil
		}
		firstErr = err
		c.cfg.Log.Printf("hub: no QUIC answer from %s (%v)", c.cfg.UDPAddr, err)
	}
	if c.cfg.TCPAddr != "" {
		car, err := c.dialTCP(ctx)
		if err == nil {
			return car, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = errors.New("no hub address configured")
	}
	return nil, firstErr
}

// hubQUICConfig keeps the channel alive through NATs and notices a dead hub
// quickly; relayed peer traffic rides on this connection.
var hubQUICConfig = &quic.Config{
	MaxIdleTimeout:                 30 * time.Second,
	KeepAlivePeriod:                10 * time.Second,
	EnableDatagrams:                true,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

func (c *Client) dialQUIC(ctx context.Context) (*carrier, error) {
	addr, err := net.ResolveUDPAddr("udp", c.cfg.UDPAddr)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	conn, err := c.cfg.Transport.Dial(dctx, addr, c.cfg.Key.ClientTLS(c.cfg.HubID), hubQUICConfig)
	if err != nil {
		return nil, err
	}
	st, err := conn.OpenStreamSync(dctx)
	if err != nil {
		_ = conn.CloseWithError(0, "no stream")
		return nil, err
	}
	var dg *quic.Conn // datagrams only when the hub supports them
	if conn.ConnectionState().SupportsDatagrams.Remote {
		dg = conn
	}
	return &carrier{name: "udp", rw: st, conn: dg, stop: func() { _ = conn.CloseWithError(0, "bye") }}, nil
}

func (c *Client) dialTCP(ctx context.Context) (*carrier, error) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: c.cfg.Key.ClientTLS(c.cfg.HubID)}
	conn, err := d.DialContext(ctx, "tcp", c.cfg.TCPAddr)
	if err != nil {
		return nil, err
	}
	return &carrier{name: "tcp", rw: conn, stop: func() { _ = conn.Close() }}, nil
}
