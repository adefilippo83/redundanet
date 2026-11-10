package pairing

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// impostor answers at an address as someone who has read the code would:
// it knows the secret and runs the exchange correctly, claims the key
// `claim` and signs with `signer`. It reports whether an offer reached it.
func impostor(t *testing.T, secret []byte, claim identity.Key, signer *identity.DeviceKey) (addr string, gotOffer chan bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	gotOffer = make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			gotOffer <- false
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		ok := func() bool {
			theirs, err := readShare(conn)
			if err != nil {
				return false
			}
			mine, _ := ecdh.X25519().GenerateKey(rand.Reader)
			if writeShare(conn, mine.PublicKey().Bytes()) != nil {
				return false
			}
			s, err := agreedSealer(secret, mine, theirs, theirs, mine.PublicKey().Bytes())
			if err != nil {
				return false
			}
			open, err := s.read(conn, dirToNew)
			if err != nil {
				return false
			}
			proof := signer.Sign(domainExchange, append(append([]byte{}, theirs...), mine.PublicKey().Bytes()...))
			if s.write(conn, dirToExisting, &message{Type: "hello", Nonce: open.Nonce, Network: "testnet", Device: claim, Name: "impostor", Proof: proof}) != nil {
				return false
			}
			m, err := s.read(conn, dirToNew)
			return err == nil && m.Offer != nil
		}()
		gotOffer <- ok
	}()
	return ln.Addr().String(), gotOffer
}

// Whoever else has read the code knows its secret, and could answer in
// the new device's place. The identity, with the member secret in it,
// must not go to them: the code names the new device's key, and only the
// holder of that key can answer.
func TestTheOfferGoesOnlyToTheDeviceOfTheCode(t *testing.T) {
	real, _ := identity.NewDeviceKey()
	other, _ := identity.NewDeviceKey()
	secret := bytes.Repeat([]byte{7}, secretBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for name, c := range map[string]struct {
		claim identity.Key
		want  string
	}{
		"under its own key":      {other.ID(), "is not the one the code was made on"},
		"under the device's key": {real.ID(), "did not prove it is the one the code was made on"},
	} {
		addr, gotOffer := impostor(t, secret, c.claim, other)
		code := (&Code{Version: version, Addrs: []string{addr}, Secret: secret, Device: real.ID()}).String()
		called := false
		_, _, err := Pair(ctx, code, "testnet", func(identity.Key, string) (*Offer, error) {
			called = true
			return &Offer{State: []byte("the member secret")}, nil
		})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("an impostor %s: %v", name, err)
		}
		if called || <-gotOffer {
			t.Errorf("an impostor %s got as far as the offer", name)
		}
	}
}

// Two exchanges with one code do not share a key, so a counter never
// repeats under one: the same message seals differently each time. Under
// a code from before version 3 it sealed the same, which is what let a
// second attempt be played against the first.
func TestEachExchangeHasItsOwnKey(t *testing.T) {
	secret := bytes.Repeat([]byte{3}, secretBytes)
	seal := func(s *sealer) []byte {
		var b bytes.Buffer
		if err := s.write(&b, dirToNew, &message{Type: "open", Nonce: bytes.Repeat([]byte{1}, 16)}); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	exchange := func() (opener, listener *sealer) {
		a, _ := ecdh.X25519().GenerateKey(rand.Reader)
		b, _ := ecdh.X25519().GenerateKey(rand.Reader)
		opener, err := agreedSealer(secret, a, b.PublicKey().Bytes(), a.PublicKey().Bytes(), b.PublicKey().Bytes())
		if err != nil {
			t.Fatal(err)
		}
		listener, err = agreedSealer(secret, b, a.PublicKey().Bytes(), a.PublicKey().Bytes(), b.PublicKey().Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return opener, listener
	}
	o1, l1 := exchange()
	o2, _ := exchange()
	first := seal(o1)
	if bytes.Equal(first, seal(o2)) {
		t.Fatal("two exchanges sealed a message alike")
	}
	if m, err := l1.read(bytes.NewReader(first), dirToNew); err != nil || m.Type != "open" {
		t.Fatalf("the two sides of one exchange do not agree: %v", err)
	}
	// The secret is part of the key: the agreement alone opens nothing.
	a, _ := ecdh.X25519().GenerateKey(rand.Reader)
	b, _ := ecdh.X25519().GenerateKey(rand.Reader)
	right, _ := agreedSealer(secret, a, b.PublicKey().Bytes(), a.PublicKey().Bytes(), b.PublicKey().Bytes())
	wrong, _ := agreedSealer(make([]byte, secretBytes), b, a.PublicKey().Bytes(), a.PublicKey().Bytes(), b.PublicKey().Bytes())
	if _, err := wrong.read(bytes.NewReader(seal(right)), dirToNew); err == nil {
		t.Fatal("a message opened without the code's secret")
	}
	if _, err := agreedSealer(secret, a, make([]byte, keyShareBytes), nil, nil); err == nil {
		t.Fatal("a key share of zeros was accepted")
	}
}

// oldListener serves one exchange as a device before 3.0.15 does: sealed
// under the code's secret alone, no key share, no proof.
func oldListener(t *testing.T, secret []byte, dev identity.Key) (string, chan *Offer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan *Offer, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		s := newSealer(secret)
		open, err := s.read(conn, dirToNew)
		if err != nil {
			got <- nil
			return
		}
		_ = s.write(conn, dirToExisting, &message{Type: "hello", Nonce: open.Nonce, Network: "testnet", Device: dev, Name: "old"})
		m, err := s.read(conn, dirToNew)
		if err != nil || m.Offer == nil {
			got <- nil
			return
		}
		_ = s.write(conn, dirToExisting, &message{Type: "done"})
		got <- m.Offer
	}()
	return ln.Addr().String(), got
}

// A code from a device that runs an older version, such as a Pi image
// flashed before, still pairs, by the older exchange. The other way does
// not: a new device answers only the exchange with the key agreement.
func TestOlderCodesStillPairAndOlderOpenersAreNotAnswered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var dev identity.Key
	dev[0] = 4
	secret := bytes.Repeat([]byte{5}, secretBytes)
	addr, got := oldListener(t, secret, dev)
	old := &Code{Version: 2, Addrs: []string{addr}, Secret: secret}
	if old.Bound() {
		t.Fatal("a version 2 code counts as bound to a device")
	}
	paired, name, err := Pair(ctx, old.String(), "testnet", func(identity.Key, string) (*Offer, error) { return &Offer{State: []byte("x")}, nil })
	if err != nil || paired != dev || name != "old" {
		t.Fatalf("pairing with an older device: %s %q %v", paired.Short(), name, err)
	}
	if o := <-got; o == nil || string(o.State) != "x" {
		t.Fatalf("the older device got %+v", o)
	}

	key, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", key, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now, _ := Parse(l.Code())
	if !now.Bound() || now.Device != key.ID() {
		t.Fatalf("the code of this version: %+v", now)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := l.ln.Accept()
		if err != nil {
			done <- err
			return
		}
		_, err = l.serve(conn)
		done <- err
	}()
	asOld := &Code{Version: 2, Addrs: now.Addrs, Secret: now.Secret}
	called := false
	if _, _, err := Pair(ctx, asOld.String(), "testnet", func(identity.Key, string) (*Offer, error) { called = true; return nil, nil }); err == nil || called {
		t.Fatalf("the older exchange against this version: err %v, certify called %v", err, called)
	}
	if err := <-done; err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "version before") {
		t.Fatalf("the new device answered the older exchange: %v", err)
	}
}
