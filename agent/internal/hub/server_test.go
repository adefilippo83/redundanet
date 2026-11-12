package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

func testDevice(t *testing.T, b byte, roles ...string) (identity.Key, *identity.TLSIdentity) {
	t.Helper()
	var seed identity.Seed
	seed[0] = b
	d, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: d.ID(), Roles: roles, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.NewTLSIdentity(d, sc)
	if err != nil {
		t.Fatal(err)
	}
	return d.ID(), id
}

// channel connects a device over the TCP carrier and reads its Welcome.
func channel(t *testing.T, s *Server, hubKey identity.Key, id *identity.TLSIdentity) *tls.Conn {
	t.Helper()
	allow := func(identity.Peer) error { return nil }
	conn, err := tls.Dial("tcp", s.TCPAddr().String(), id.ClientConfig(hubKey, allow))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := wire.NewWriter(conn).Message(wire.FrameHello, wire.Hello{}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, _, err := wire.ReadFrame(conn)
	if err != nil || typ != wire.FrameWelcome {
		t.Fatalf("no welcome: type %d, %v", typ, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

func connected(s *Server, k identity.Key) bool {
	for _, c := range s.Status().Clients {
		if c.Device == k.String() {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A device that keeps its channel open and reads nothing costs nobody but
// itself: what the hub has for it never makes a caller wait, another
// device's rendezvous with it is answered at once, and the hub closes its
// channel.
func TestADeviceThatStopsReadingStallsNobody(t *testing.T) {
	old := writeTimeout
	writeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { writeTimeout = old })

	ctx, cancel := context.WithCancel(context.Background())
	hubKey, hubID := testDevice(t, 100, identity.RoleHub)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s, err := Start(ctx, Config{Identity: hubID, Device: hubKey, Allow: func(identity.Peer) error { return nil }, UDP: "127.0.0.1:0", TCP: "127.0.0.1:0", Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = s.Close() })

	deafKey, deafID := testDevice(t, 1, identity.RoleClient, identity.RoleStorage)
	channel(t, s, hubKey, deafID) // and never read from it again
	askerKey, askerID := testDevice(t, 2, identity.RoleClient)
	asker := channel(t, s, hubKey, askerID)
	waitFor(t, 5*time.Second, "both devices on the hub", func() bool { return connected(s, deafKey) && connected(s, askerKey) })

	// Far more than the socket and the queue hold. Every call returns at
	// once, whatever its answer.
	big := make([]byte, wire.MaxFrame-64)
	refused := false
	for i := 0; i < 400 && !refused; i++ {
		started := time.Now()
		err := s.Send(deafKey, wire.FrameTask, big)
		if took := time.Since(started); took > 200*time.Millisecond {
			t.Fatalf("send %d waited %s for a device that does not read", i, took)
		}
		switch {
		case err == nil:
		case errors.Is(err, ErrNotReading), errors.Is(err, ErrNoChannel):
			refused = true
		default:
			t.Fatalf("send %d: %v", i, err)
		}

		// The other device's rendezvous with the deaf one is answered
		// meanwhile, while the deaf one is still connected.
		if i == 20 && connected(s, deafKey) {
			if err := wire.NewWriter(asker).Message(wire.FrameConnect, wire.Connect{Peer: deafKey}); err != nil {
				t.Fatal(err)
			}
			_ = asker.SetReadDeadline(time.Now().Add(2 * time.Second))
			typ, _, err := wire.ReadFrame(asker)
			if err != nil || typ != wire.FrameCandidates {
				t.Fatalf("no candidates while a device is not reading: type %d, %v", typ, err)
			}
			_ = asker.SetReadDeadline(time.Time{})
		}
	}
	if !refused {
		t.Fatal("the hub kept queueing for a device that does not read")
	}
	waitFor(t, 5*time.Second, "the hub to drop the device that does not read", func() bool { return !connected(s, deafKey) })
	if !connected(s, askerKey) {
		t.Fatal("the device that reads lost its channel too")
	}
}

// The hub passes a revocation on to the other devices only when its
// membership says the record was new.
func TestOnlyANewRevocationIsPassedOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hubKey, hubID := testDevice(t, 100, identity.RoleHub)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var memberSeed identity.Seed
	memberSeed[0] = 1
	newSerial := uint64(2)
	s, err := Start(ctx, Config{Identity: hubID, Device: hubKey, Allow: func(identity.Peer) error { return nil }, UDP: "127.0.0.1:0", TCP: "127.0.0.1:0", Log: log,
		OnRevocation: func(_ identity.Peer, r *identity.SignedRevocation) bool { return r.Revocation.Serial == newSerial }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = s.Close() })

	senderKey, senderID := testDevice(t, 1, identity.RoleClient)
	sender := channel(t, s, hubKey, senderID)
	otherKey, otherID := testDevice(t, 2, identity.RoleClient)
	other := channel(t, s, hubKey, otherID)
	waitFor(t, 5*time.Second, "both devices on the hub", func() bool { return connected(s, senderKey) && connected(s, otherKey) })

	w := wire.NewWriter(sender)
	for _, serial := range []uint64{1, newSerial} {
		rv, err := identity.Revoke(memberSeed, identity.Revocation{Network: "testnet", Device: senderKey, Serial: serial})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Message(wire.FrameRevocation, rv); err != nil {
			t.Fatal(err)
		}
	}
	_ = other.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := wire.ReadFrame(other)
	if err != nil || typ != wire.FrameRevocation {
		t.Fatalf("no revocation passed on: type %d, %v", typ, err)
	}
	rv, err := identity.DecodeRevocation(payload)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Revocation.Serial != newSerial {
		t.Fatalf("the record the membership did not take was passed on (serial %d)", rv.Revocation.Serial)
	}
}
