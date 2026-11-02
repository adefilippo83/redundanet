package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// HubAddr is how an agent reaches one hub.
type HubAddr struct {
	Key identity.Key
	UDP string // host:port of the QUIC relay
	TCP string // host:port of the TLS carrier, "" if none
}

// hubClient keeps one device's channel to one hub, reconnecting as needed.
type hubClient struct {
	ep  *Endpoint
	hub HubAddr

	mu       sync.Mutex
	w        *wire.Writer
	conn     *quic.Conn // the QUIC carrier, nil over TCP
	out      chan []byte
	up       bool
	carrier  string
	observed string
	since    time.Time
	lastErr  string
	reconns  int
	dropped  uint64
	relay    *relayConn
	relayTr  *quic.Transport
	relayLn  *quic.Listener
}

// relayQueue bounds packets waiting for the hub.
const relayQueue = 256

// hubQUICConfig keeps the channel alive through NATs; relayed peer traffic
// rides on this connection as datagrams.
var hubQUICConfig = &quic.Config{
	MaxIdleTimeout:                 30 * time.Second,
	KeepAlivePeriod:                10 * time.Second,
	EnableDatagrams:                true,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

// HubStatus is one hub channel's state.
type HubStatus struct {
	Hub        string    `json:"hub"`
	Up         bool      `json:"up"`
	Carrier    string    `json:"carrier,omitempty"`
	Observed   string    `json:"observed,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Reconnects int       `json:"reconnects"`
	LastError  string    `json:"last_error,omitempty"`
	RelayDrops uint64    `json:"relay_drops"`
}

func (c *hubClient) status() HubStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return HubStatus{Hub: c.hub.Key.Short(), Up: c.up, Carrier: c.carrier, Observed: c.observed, Since: c.since, Reconnects: c.reconns, LastError: c.lastErr, RelayDrops: c.dropped}
}

func (c *hubClient) sendMessage(typ byte, v any) error {
	c.mu.Lock()
	w := c.w
	c.mu.Unlock()
	if w == nil {
		return errors.New("hub channel is down")
	}
	return w.Message(typ, v)
}

// sendRelay queues one packet for the hub and never blocks.
func (c *hubClient) sendRelay(peer identity.Key, pkt []byte) error {
	c.mu.Lock()
	out := c.out
	c.mu.Unlock()
	if out == nil {
		return errors.New("hub channel is down")
	}
	select {
	case out <- wire.RelayPayload(peer, pkt):
		return nil
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
		return nil
	}
}

func (c *hubClient) run(ctx context.Context) {
	// The relay transport lives as long as the client: relayed connections
	// survive a channel reconnect as long as QUIC's idle timeout allows.
	c.relay = newRelayConn(c.ep.udp.LocalAddr(), c.sendRelay)
	c.relayTr = &quic.Transport{Conn: c.relay}
	ln, err := c.relayTr.Listen(c.ep.cfg.Identity.ServerConfig(c.ep.cfg.Allow), relayedQUICConfig)
	if err != nil {
		c.ep.log.Error("relay listener", "hub", c.hub.Key.Short(), "err", err)
		return
	}
	c.relayLn = ln
	go c.ep.acceptLoop(ctx, ln, PathRelay)

	backoff := time.Second
	for {
		err := c.session(ctx)
		if ctx.Err() != nil {
			_ = ln.Close()
			_ = c.relayTr.Close()
			return
		}
		c.mu.Lock()
		c.up = false
		c.reconns++
		if err != nil {
			c.lastErr = err.Error()
		}
		c.mu.Unlock()
		c.ep.log.Warn("hub channel down", "hub", c.hub.Key.Short(), "err", err, "retry", backoff)
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
	conn *quic.Conn
	stop func()
}

func (c *hubClient) session(ctx context.Context) error {
	car, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer car.stop()
	w := wire.NewWriter(car.rw)
	if err := w.Message(wire.FrameHello, wire.Hello{Locals: c.ep.localCandidates()}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	// The Welcome is the first frame from a hub of this version; anything
	// else before it is kept for after the channel is up.
	var welcome wire.Welcome
	var early [][2][]byte
	for {
		typ, payload, err := wire.ReadFrame(car.rw)
		if err != nil {
			return fmt.Errorf("welcome: %w", err)
		}
		if typ == wire.FrameWelcome {
			if err := identity.Unmarshal(payload, &welcome); err != nil {
				return err
			}
			break
		}
		if len(early) > 64 {
			return errors.New("welcome: too many frames before it")
		}
		early = append(early, [2][]byte{{typ}, payload})
	}
	out := make(chan []byte, relayQueue)
	done := make(chan struct{})
	defer close(done)
	c.mu.Lock()
	c.w, c.conn, c.out = w, car.conn, out
	c.up, c.carrier, c.observed, c.since = true, car.name, welcome.Observed, time.Now()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.w, c.conn, c.out = nil, nil, nil
		c.mu.Unlock()
	}()
	go c.writeRelay(car, w, out, done)
	if car.conn != nil {
		go func() {
			for {
				b, err := car.conn.ReceiveDatagram(ctx)
				if err != nil {
					return
				}
				if from, pkt, err := wire.SplitRelay(b); err == nil {
					c.relay.deliver(from, pkt)
				}
			}
		}()
	}
	c.ep.log.Info("hub channel up", "hub", c.hub.Key.Short(), "carrier", car.name, "observed", welcome.Observed)
	c.ep.onHubUp(c)
	for _, f := range early {
		c.handleFrame(w, f[0][0], f[1])
	}

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go c.censusLoop(pingCtx, w)
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
		c.handleFrame(w, typ, payload)
	}
}

func (c *hubClient) handleFrame(w *wire.Writer, typ byte, payload []byte) {
	switch typ {
	case wire.FrameCandidates:
		var cand wire.Candidates
		if err := identity.Unmarshal(payload, &cand); err != nil {
			c.ep.log.Warn("hub sent bad candidates", "err", err)
			return
		}
		c.ep.onCandidates(c, cand)
	case wire.FrameRelay:
		if from, pkt, err := wire.SplitRelay(payload); err == nil {
			c.relay.deliver(from, pkt)
		}
	case wire.FrameError:
		var e wire.Error
		_ = identity.Unmarshal(payload, &e)
		c.ep.log.Warn("hub error", "hub", c.hub.Key.Short(), "msg", e.Msg)
	case wire.FramePing:
		_ = w.Frame(wire.FramePong, nil)
	case wire.FramePong:
	}
}

// writeRelay drains the queue: datagrams on a QUIC carrier, frames otherwise
// or when a packet does not fit a datagram.
func (c *hubClient) writeRelay(car *carrier, w *wire.Writer, out chan []byte, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case b := <-out:
			if car.conn != nil && car.conn.SendDatagram(b) == nil {
				continue
			}
			if err := w.Frame(wire.FrameRelay, b); err != nil {
				return
			}
		}
	}
}

func (c *hubClient) dial(ctx context.Context) (*carrier, error) {
	var firstErr error
	if c.hub.UDP != "" && !c.ep.cfg.TCPOnly {
		car, err := c.dialQUIC(ctx)
		if err == nil {
			return car, nil
		}
		firstErr = err
	}
	if c.hub.TCP != "" {
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

func (c *hubClient) dialQUIC(ctx context.Context) (*carrier, error) {
	addr, err := net.ResolveUDPAddr("udp", c.hub.UDP)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := c.ep.tr.Dial(dctx, addr, c.ep.cfg.Identity.ClientConfig(c.hub.Key, c.ep.cfg.Allow), hubQUICConfig)
	if err != nil {
		return nil, err
	}
	st, err := conn.OpenStreamSync(dctx)
	if err != nil {
		_ = conn.CloseWithError(0, "no stream")
		return nil, err
	}
	var dg *quic.Conn
	if conn.ConnectionState().SupportsDatagrams.Remote {
		dg = conn
	}
	return &carrier{name: "udp", rw: st, conn: dg, stop: func() { _ = conn.CloseWithError(0, "bye") }}, nil
}

func (c *hubClient) dialTCP(ctx context.Context) (*carrier, error) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: c.ep.cfg.Identity.ClientConfig(c.hub.Key, c.ep.cfg.Allow)}
	conn, err := d.DialContext(ctx, "tcp", c.hub.TCP)
	if err != nil {
		return nil, err
	}
	return &carrier{name: "tcp", rw: conn, stop: func() { _ = conn.Close() }}, nil
}

var _ = slog.Logger{}
