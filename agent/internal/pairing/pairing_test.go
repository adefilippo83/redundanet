package pairing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func TestPairHandsOverTheOffer(t *testing.T) {
	dev, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", dev.ID(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	code := l.Code()
	if !strings.HasPrefix(code, "rn3:pair:") {
		t.Fatalf("code %q", code)
	}
	if c, err := Parse(code); err != nil || len(c.Addrs) != 1 || len(c.Secret) != secretBytes {
		t.Fatalf("parse: %+v %v", c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type result struct {
		offer *Offer
		err   error
	}
	got := make(chan result, 1)
	go func() { o, err := l.Wait(ctx); got <- result{o, err} }()
	var certified identity.Key
	paired, pairedName, err := Pair(ctx, code, "testnet", func(d identity.Key, _ string) (*Offer, error) {
		certified = d
		return &Offer{State: []byte("the identity"), Descriptor: "rn3:net:xyz"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil || string(r.offer.State) != "the identity" || r.offer.Descriptor != "rn3:net:xyz" {
		t.Fatalf("listener got %+v %v", r.offer, r.err)
	}
	if paired != dev.ID() || certified != dev.ID() {
		t.Fatalf("paired %s, certified %s, device %s", paired.Short(), certified.Short(), dev.ID().Short())
	}
	if host, _ := os.Hostname(); pairedName != host {
		t.Fatalf("the new device's name came through as %q, its host name is %q", pairedName, host)
	}
}

func TestWrongCodeRevealsNothingAndThreeStrikesEndIt(t *testing.T) {
	dev, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", dev.ID(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	right, _ := Parse(l.Code())
	wrong := &Code{Version: version, Addrs: right.Addrs, Secret: make([]byte, secretBytes)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := l.Wait(ctx); done <- err }()
	called := false
	for i := 0; i < maxAttempts; i++ {
		_, _, err := Pair(ctx, wrong.String(), "testnet", func(identity.Key, string) (*Offer, error) { called = true; return nil, nil })
		if err == nil || !strings.Contains(err.Error(), "did not answer the code") {
			t.Fatalf("attempt %d with the wrong code: %v", i, err)
		}
	}
	if called {
		t.Fatal("certify ran for a wrong code")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "failed the code") {
			t.Fatalf("listener ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener survived three failed attempts")
	}
}

func TestPairRefusesAnotherNetworkAndBadCodes(t *testing.T) {
	dev, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", dev.ID(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { _, _ = l.Wait(ctx) }()
	if _, _, err := Pair(ctx, l.Code(), "othernet", func(identity.Key, string) (*Offer, error) { return &Offer{State: []byte("x")}, nil }); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("another network: %v", err)
	}
	for _, bad := range []string{"", "rn3:net:abc", "rn3:pair:!!!", "rn3:pair:" + strings.Repeat("a", 10)} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	if _, _, err := Pair(ctx, (&Code{Version: version, Addrs: []string{"127.0.0.1:1"}, Secret: make([]byte, secretBytes)}).String(), "testnet", nil); err == nil || !errors.Is(err, errUnreachable) && !strings.Contains(err.Error(), "could not be reached") {
		t.Fatalf("an unreachable address: %v", err)
	}
}

// A device that shows its code in a file, such as the Pi image, listens
// again after a restart with the secret and port it kept, and the code it
// showed before still completes the exchange.
func TestResumeKeepsTheCode(t *testing.T) {
	dev, _ := identity.NewDeviceKey()
	l, err := Listen("testnet", dev.ID(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	code, secret, port := l.Code(), l.Secret(), l.Port()
	l.Close()
	if _, err := Resume("testnet", dev.ID(), "127.0.0.1:0", nil, []byte("short")); err == nil {
		t.Fatal("a secret of the wrong length was accepted")
	}
	again, err := Resume("testnet", dev.ID(), fmt.Sprintf("127.0.0.1:%d", port), nil, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Code() != code {
		t.Fatalf("resumed code %q, the first was %q", again.Code(), code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := again.Wait(ctx); done <- err }()
	if _, _, err := Pair(ctx, code, "testnet", func(identity.Key, string) (*Offer, error) {
		return &Offer{State: []byte("the identity")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
