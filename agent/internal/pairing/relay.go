package pairing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pairing through the hub. When the two devices cannot reach each other
// (a laptop at home, a node in a data centre), the hub relays the exchange:
// a mailbox per room, with one queue of frames per direction, alive for
// ten minutes. The room is derived from the code's secret, so only the two
// holders of the code find it, and the frames are the very same sealed
// ones as over a direct connection, so the hub learns nothing but that a
// pairing happened and from which addresses. The hub serves it under
// /pair/ on its status page, so it travels over the page's HTTPS.

const (
	roomTTL    = 10 * time.Minute
	maxRooms   = 256
	queueDepth = 16
	maxFrame   = maxMessage + 2 + 64
	pollWait   = 20 * time.Second

	sideNew      = "new"      // the queue of frames addressed to the new device
	sideExisting = "existing" // the queue of frames addressed to the existing device
)

var errRelayUnavailable = errors.New("the hub does not relay pairing")

// roomID derives the mailbox from the code's secret, one way.
func roomID(secret []byte) string {
	h := sha256.Sum256(append([]byte("redundanet/3/pair-room\x00"), secret...))
	return hex.EncodeToString(h[:16])
}

// Relay is the hub's mailbox: an http.Handler for /pair/{room}/{side}.
// POST appends a frame to that side's queue, GET takes the next one,
// waiting up to `wait` seconds (20 at most) and answering 204 when none
// came.
type Relay struct {
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	rooms map[string]*room
}

type room struct {
	expires time.Time
	to      map[string]chan []byte
}

// NewRelay makes an empty mailbox.
func NewRelay(lg *slog.Logger) *Relay {
	if lg == nil {
		lg = slog.Default()
	}
	return &Relay{log: lg.With("component", "pair-relay"), now: time.Now, rooms: map[string]*room{}}
}

// Rooms is how many pairings are in progress.
func (r *Relay) Rooms() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	return len(r.rooms)
}

func (r *Relay) sweepLocked() {
	now := r.now()
	for id, rm := range r.rooms {
		if now.After(rm.expires) {
			delete(r.rooms, id)
		}
	}
}

// room finds or opens a mailbox; nil when the hub is full.
func (r *Relay) room(id string) *room {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	if rm, ok := r.rooms[id]; ok {
		return rm
	}
	if len(r.rooms) >= maxRooms {
		return nil
	}
	rm := &room{expires: r.now().Add(roomTTL), to: map[string]chan []byte{sideNew: make(chan []byte, queueDepth), sideExisting: make(chan []byte, queueDepth)}}
	r.rooms[id] = rm
	return rm
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(req.URL.Path, "/pair/"), "/"), "/")
	if len(parts) != 2 || len(parts[0]) != 32 || !isHex(parts[0]) || (parts[1] != sideNew && parts[1] != sideExisting) {
		http.Error(w, "a pairing room is /pair/{room}/{new|existing}", http.StatusBadRequest)
		return
	}
	rm := r.room(parts[0])
	if rm == nil {
		http.Error(w, "too many pairings in progress; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	q := rm.to[parts[1]]
	switch req.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(req.Body, maxFrame+1))
		if err != nil || len(body) == 0 {
			http.Error(w, "a frame is needed", http.StatusBadRequest)
			return
		}
		if len(body) > maxFrame {
			http.Error(w, "frame too large", http.StatusRequestEntityTooLarge)
			return
		}
		select {
		case q <- body:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "the other side has not read the earlier frames", http.StatusTooManyRequests)
		}
	case http.MethodGet:
		wait := pollWait
		if s := req.URL.Query().Get("wait"); s != "" {
			if n, err := strconv.Atoi(strings.TrimSuffix(s, "s")); err == nil && n >= 0 && time.Duration(n)*time.Second < pollWait {
				wait = time.Duration(n) * time.Second
			}
		}
		deliver := func(b []byte) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(b)
		}
		// A frame already waiting goes out at once, whatever the wait; a
		// select with an expired timer would otherwise pick either at random.
		select {
		case b := <-q:
			deliver(b)
			return
		default:
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case b := <-q:
			deliver(b)
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
		case <-req.Context().Done():
		}
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// relayConn is one side's view of the mailbox as a connection, so the
// exchange runs over it unchanged: writes are posted to the other side's
// queue (coalesced until the next read), reads poll this side's queue.
// Deadlines apply once the conversation has begun; the new device waits
// for an opener as long as its code lives.
type relayConn struct {
	ctx        context.Context
	base, room string
	send, recv string
	client     *http.Client
	started    bool
	deadline   time.Time
	in, out    bytes.Buffer
	closed     bool
}

// newRelayConn opens one side of the room at a hub. side is which device
// this is: the new one waits for the opener, the existing one opens.
func newRelayConn(ctx context.Context, base, room, side string) *relayConn {
	c := &relayConn{ctx: ctx, base: strings.TrimRight(base, "/"), room: room, client: &http.Client{Timeout: pollWait + 10*time.Second}}
	if side == sideNew {
		c.send, c.recv = sideExisting, sideNew
	} else {
		c.send, c.recv, c.started = sideNew, sideExisting, true
	}
	return c
}

func (c *relayConn) url(side string) string { return c.base + "/pair/" + c.room + "/" + side }

func (c *relayConn) Write(p []byte) (int, error) {
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.out.Write(p)
}

// flush posts what was written so far as one frame.
func (c *relayConn) flush() error {
	if c.out.Len() == 0 {
		return nil
	}
	ctx, cancel := c.requestContext(10 * time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(c.send), bytes.NewReader(c.out.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnreachable, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	c.out.Reset()
	return c.status(resp.StatusCode)
}

func (c *relayConn) status(code int) error {
	switch {
	case code == http.StatusNoContent || code == http.StatusOK:
		return nil
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		// A hub before 3.0.10 has a status page but no mailbox: its page
		// answers 404 to a GET under /pair/ and 405 to a POST.
		return fmt.Errorf("%w: %s answers %d to /pair/, it needs 3.0.10 or later", errRelayUnavailable, c.base, code)
	case code == http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %s has too many pairings in progress", errRelayUnavailable, c.base)
	default:
		return fmt.Errorf("the hub at %s answered %d", c.base, code)
	}
}

// requestContext bounds one request by the deadline, once it applies.
func (c *relayConn) requestContext(d time.Duration) (context.Context, context.CancelFunc) {
	if c.started && !c.deadline.IsZero() {
		if until := time.Until(c.deadline); until < d {
			d = until
		}
	}
	if d <= 0 {
		d = time.Millisecond
	}
	return context.WithTimeout(c.ctx, d)
}

func (c *relayConn) Read(p []byte) (int, error) {
	if err := c.flush(); err != nil {
		return 0, err
	}
	for c.in.Len() == 0 {
		if c.closed {
			return 0, io.EOF
		}
		if c.ctx.Err() != nil {
			return 0, c.ctx.Err()
		}
		wait := pollWait - time.Second
		if c.started && !c.deadline.IsZero() {
			until := time.Until(c.deadline)
			if until <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			if until < wait {
				wait = until
			}
		}
		ctx, cancel := context.WithTimeout(c.ctx, wait+5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(c.recv)+"?wait="+strconv.Itoa(int(wait.Seconds())), nil)
		if err != nil {
			cancel()
			return 0, err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			cancel()
			if c.ctx.Err() != nil {
				return 0, c.ctx.Err()
			}
			return 0, fmt.Errorf("%w: %v", errUnreachable, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxFrame+1))
		_ = resp.Body.Close()
		cancel()
		if err != nil {
			return 0, err
		}
		if resp.StatusCode == http.StatusOK {
			c.in.Write(body)
			c.started = true
			continue
		}
		if err := c.status(resp.StatusCode); err != nil {
			return 0, err
		}
	}
	return c.in.Read(p)
}

func (c *relayConn) Close() error {
	if c.closed {
		return nil
	}
	err := c.flush()
	c.closed = true
	return err
}

type relayAddr string

func (a relayAddr) Network() string { return "https" }
func (a relayAddr) String() string  { return string(a) }

func (c *relayConn) LocalAddr() net.Addr                { return relayAddr("this device") }
func (c *relayConn) RemoteAddr() net.Addr               { return relayAddr(c.base) }
func (c *relayConn) SetDeadline(t time.Time) error      { c.deadline = t; return nil }
func (c *relayConn) SetReadDeadline(t time.Time) error  { c.deadline = t; return nil }
func (c *relayConn) SetWriteDeadline(t time.Time) error { c.deadline = t; return nil }
