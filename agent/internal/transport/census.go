package transport

import (
	"bytes"
	"context"
	"sort"
	"sync/atomic"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// A device repeats its heartbeat to a hub every censusEvery, and sooner,
// checked every censusPoll, when what it says has changed: a peer reached
// or lost, free space moved, an update outcome. The first one goes as soon
// as the channel is up. Tests shorten both, which is why they are atomics.
var censusEvery, censusPoll atomic.Int64 // nanoseconds

func init() {
	censusEvery.Store(int64(time.Minute))
	censusPoll.Store(int64(5 * time.Second))
}

// census is the heartbeat for a hub: the caller's device-level fields plus
// the sessions this endpoint holds, in a fixed order.
func (e *Endpoint) census() (wire.Census, bool) {
	if e.cfg.Census == nil {
		return wire.Census{}, false
	}
	c := e.cfg.Census()
	e.mu.Lock()
	sessions := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()
	for _, s := range sessions {
		st := s.status()
		if st.Relay || st.Direct != "" {
			c.Peers = append(c.Peers, wire.CensusPeer{Device: s.peer, Direct: st.Direct != "", Relay: st.Relay})
		}
	}
	sort.Slice(c.Peers, func(i, j int) bool { return c.Peers[i].Device.Less(c.Peers[j].Device) })
	return c, true
}

// fingerprint is the heartbeat without the fields that change on their own.
func fingerprint(c wire.Census) []byte {
	c.Uptime = 0
	if c.Storage != nil {
		s := *c.Storage
		s.FreeBytes -= s.FreeBytes % (64 << 20) // a change of free space under 64 MB is not news
		c.Storage = &s
	}
	b, _ := identity.Marshal(c)
	return b
}

// censusLoop sends the heartbeat over one channel until it ends.
func (c *hubClient) censusLoop(ctx context.Context, w *wire.Writer) {
	var last []byte
	var lastAt time.Time
	send := func() bool {
		cs, ok := c.ep.census()
		if !ok {
			return true
		}
		fp := fingerprint(cs)
		if bytes.Equal(fp, last) && time.Since(lastAt) < time.Duration(censusEvery.Load()) {
			return true
		}
		if err := w.Message(wire.FrameCensus, cs); err != nil {
			return false
		}
		last, lastAt = fp, time.Now()
		return true
	}
	if !send() {
		return
	}
	poll := time.Duration(censusPoll.Load())
	if every := time.Duration(censusEvery.Load()); every < poll {
		poll = every
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}
