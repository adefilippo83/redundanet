package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestLoopbackCarriesBothWaysAndCloses(t *testing.T) {
	near, far := Loopback()
	if _, err := near.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	_ = near.Close()
	b, err := io.ReadAll(far)
	if err != nil || string(b) != "request" {
		t.Fatalf("far read %q, %v", b, err)
	}
	if _, err := near.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close: %v", err)
	}
	if _, err := far.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	_ = far.Close()
	if b, err := io.ReadAll(near); err != nil || string(b) != "response" {
		t.Fatalf("near read %q, %v", b, err)
	}
}

func TestLoopbackCancelReadFailsTheWriter(t *testing.T) {
	near, far := Loopback()
	// A writer past the window waits for a reader; the cancel releases it.
	big := make([]byte, 2*loopbackWindow)
	done := make(chan error, 1)
	go func() {
		_, err := near.Write(big)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	far.CancelRead(1)
	select {
	case err := <-done:
		var se *quic.StreamError
		if !errors.As(err, &se) || se.ErrorCode != 1 {
			t.Fatalf("writer got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer stayed blocked after the reader cancelled")
	}
	if _, err := far.Read(make([]byte, 1)); err == nil {
		t.Fatal("a cancelled read half still reads")
	}
}

func TestLoopbackWindowDrains(t *testing.T) {
	near, far := Loopback()
	data := bytes.Repeat([]byte{7}, 3*loopbackWindow+123)
	go func() {
		_, _ = near.Write(data)
		_ = near.Close()
	}()
	got, err := io.ReadAll(far)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
}
