// Package ratelimit paces a device's mesh traffic: the RFC's bandwidth caps
// (F8), one limit for what the device sends and one for what it receives,
// in kilobytes per second, 0 for none. A token bucket with a second's worth
// of burst, so a backup never takes more than its share of a home uplink.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// chunk bounds what one write hands the bucket at a time, so a cap paces
// a large write instead of letting it through and then stalling.
const chunk = 64 << 10

// Limiter is one direction's cap. The zero rate is no cap.
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

// New makes a limiter at kbps kilobytes (1000 bytes) per second.
func New(kbps int) *Limiter {
	l := &Limiter{now: time.Now, sleep: sleepCtx}
	l.SetKBps(kbps)
	return l
}

// SetKBps changes the cap; 0 removes it. A burst of one second's worth is
// granted after idle time, never more.
func (l *Limiter) SetKBps(kbps int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if kbps <= 0 {
		l.rate, l.burst, l.tokens = 0, 0, 0
		return
	}
	l.rate = float64(kbps) * 1000
	l.burst = l.rate
	l.tokens = l.burst
	l.last = l.now()
}

// KBps is the cap, 0 for none.
func (l *Limiter) KBps() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.rate / 1000)
}

// Wait accounts n bytes and sleeps until they fit the cap: the bytes may
// already have moved, the caller just does not move the next ones before
// their time. A cancelled context ends the wait.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.rate == 0 {
		l.mu.Unlock()
		return nil
	}
	now := l.now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	l.tokens -= float64(n)
	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	return l.sleep(ctx, wait)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Stream paces a mesh stream: writes against up, reads against down. A
// limiter at 0 lets its direction through untouched.
func Stream(st wire.Stream, up, down *Limiter) wire.Stream {
	return &limited{Stream: st, up: up, down: down}
}

type limited struct {
	wire.Stream
	up, down *Limiter
}

func (s *limited) Write(p []byte) (int, error) {
	if s.up.KBps() == 0 {
		return s.Stream.Write(p)
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > chunk {
			n = chunk
		}
		if err := s.up.Wait(context.Background(), n); err != nil {
			return total, err
		}
		w, err := s.Stream.Write(p[:n])
		total += w
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

func (s *limited) Read(p []byte) (int, error) {
	if s.down.KBps() != 0 && len(p) > chunk {
		p = p[:chunk]
	}
	n, err := s.Stream.Read(p)
	if n > 0 {
		if werr := s.down.Wait(context.Background(), n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}
