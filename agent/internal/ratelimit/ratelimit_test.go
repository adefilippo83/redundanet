package ratelimit

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// A limiter with a clock and a sleep the test controls.
func fake(kbps int) (*Limiter, *time.Time, *[]time.Duration) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var slept []time.Duration
	l := &Limiter{now: func() time.Time { return now }, sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil }}
	l.SetKBps(kbps)
	return l, &now, &slept
}

func TestBucketGrantsABurstThenPaces(t *testing.T) {
	l, now, slept := fake(100) // 100 kB/s, a burst of 100 kB
	if err := l.Wait(context.Background(), 100_000); err != nil || len(*slept) != 0 {
		t.Fatalf("the burst was not free: %v %v", err, *slept)
	}
	if err := l.Wait(context.Background(), 50_000); err != nil || len(*slept) != 1 || (*slept)[0] != 500*time.Millisecond {
		t.Fatalf("50 kB over the burst at 100 kB/s: %v %v", err, *slept)
	}
	// Idle time refills, up to one burst and no more.
	*now = now.Add(10 * time.Second)
	if err := l.Wait(context.Background(), 150_000); err != nil || len(*slept) != 2 || (*slept)[1] != 500*time.Millisecond {
		t.Fatalf("after idle: %v", *slept)
	}
	l.SetKBps(0)
	if err := l.Wait(context.Background(), 1<<30); err != nil || len(*slept) != 2 {
		t.Fatalf("a cap of 0 waited: %v", *slept)
	}
	if New(250).KBps() != 250 || New(0).KBps() != 0 {
		t.Fatal("KBps")
	}
}

func TestWaitEndsWithTheContext(t *testing.T) {
	l := New(1)                            // 1 kB/s
	_ = l.Wait(context.Background(), 1000) // the burst
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.Wait(ctx, 100_000); err == nil || time.Since(start) > time.Second {
		t.Fatalf("a cancelled wait: %v after %s", err, time.Since(start))
	}
}

func TestStreamPacesBothDirections(t *testing.T) {
	near, far := wire.Loopback()
	up, down := New(200), New(0) // 200 kB/s up: 200 kB free, then paced
	paced := Stream(near, up, down)
	data := bytes.Repeat([]byte{1}, 300_000)
	done := make(chan struct{})
	var got []byte
	go func() { got, _ = io.ReadAll(far); close(done) }()
	start := time.Now()
	if _, err := paced.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = paced.Close()
	<-done
	if took := time.Since(start); took < 400*time.Millisecond || !bytes.Equal(got, data) {
		t.Fatalf("300 kB at 200 kB/s took %s, %d bytes arrived", took, len(got))
	}
	// Reads are paced too: 100 kB/s with 100 kB free, 200 kB arriving.
	near2, far2 := wire.Loopback()
	paced2 := Stream(near2, New(0), New(100))
	go func() { _, _ = far2.Write(bytes.Repeat([]byte{2}, 200_000)); _ = far2.Close() }()
	start = time.Now()
	if b, err := io.ReadAll(paced2); err != nil || len(b) != 200_000 || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("200 kB read at 100 kB/s: %v, %d bytes in %s", err, len(b), time.Since(start))
	}
}
