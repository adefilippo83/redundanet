//go:build !windows

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The launcher starts its program again when it exits, and when it is
// told to stop it stops the program rather than leaving it behind.
func TestLaunchLoopRestartsAndStops(t *testing.T) {
	old, oldQuick := launchBackoff, launchQuick
	launchBackoff, launchQuick = 50*time.Millisecond, 0
	t.Cleanup(func() { launchBackoff, launchQuick = old, oldQuick })

	var log bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = launchLoop(ctx, "/bin/sh", []string{"-c", "exit 0"}, &log)
	if n := strings.Count(log.String(), "starting it again"); n < 3 {
		t.Fatalf("restarted %d times in two seconds: %s", n, log.String())
	}

	log.Reset()
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	// The shell ends its sleep too, or the sleep would hold the test's
	// output pipe open for its thirty seconds.
	go func() {
		_ = launchLoop(ctx, "/bin/sh", []string{"-c", `trap 'kill $!; exit 0' TERM; sleep 30 & wait`}, &log)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher did not stop its program within five seconds of being told to")
	}
	if strings.Contains(log.String(), "starting it again") {
		t.Fatalf("a stop was treated as an exit: %s", log.String())
	}
}
