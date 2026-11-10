package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/pairing"
)

// Pairing from the page: the phrase typed must be this member's; with it,
// the new device gets its identity and is on record here, with its
// certificate, for the join request.
func TestPairFromThePage(t *testing.T) {
	var seed, other identity.Seed
	seed[0], other[0] = 9, 7
	dd := t.TempDir()
	rc := &runtimeConfig{dataDir: dd, state: &identity.State{Network: "testnet", Member: seed.MemberKey()}}
	phrase := strings.Join(seed.Phrase(), " ")

	// A new device waits with its code, over loopback here.
	newcomer := func() (*pairing.Listener, string, identity.Key) {
		t.Helper()
		var dev identity.Key
		dev[0] = 5
		l, err := pairing.Listen("testnet", dev, "127.0.0.1:0", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		c, err := pairing.Parse(l.Code())
		if err != nil {
			t.Fatal(err)
		}
		c.Addrs = []string{fmt.Sprintf("127.0.0.1:%d", l.Port())}
		return l, c.String(), dev
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, code, _ := newcomer()
	if _, err := pairFromPage(ctx, rc, code, "eleven words are not a phrase", false); err == nil {
		t.Fatal("words that are no phrase were accepted")
	}
	if _, err := pairFromPage(ctx, rc, code, strings.Join(other.Phrase(), " "), false); err == nil || !strings.Contains(err.Error(), "not the recovery phrase of this device's member") {
		t.Fatalf("another member's phrase: %v", err)
	}
	if devices, _ := loadDevices(dd); len(devices) != 0 {
		t.Fatalf("a refused pairing left a record: %+v", devices)
	}
	if _, err := pairFromPage(ctx, rc, "rn3:pair:nonsense", phrase, false); err == nil {
		t.Fatal("a code that is none was accepted")
	}

	l, code, dev := newcomer()
	got := make(chan *pairing.Offer, 1)
	go func() {
		o, _ := l.Wait(ctx)
		got <- o
	}()
	// The phrase as a person types it: stray spaces, a line break, capitals are theirs to avoid.
	d, err := pairFromPage(ctx, rc, "  "+code+"\n", "  "+strings.Join(seed.Phrase()[:6], " ")+"\n"+strings.Join(seed.Phrase()[6:], "  ")+" ", true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Key != dev.String() || len(d.Roles) != 2 || d.Roles[1] != identity.RoleStorage || d.PairedAt.IsZero() {
		t.Fatalf("the paired device: %+v", d)
	}
	o := <-got
	if o == nil {
		t.Fatal("the new device got no offer")
	}
	var st identity.State
	if err := identity.Unmarshal(o.State, &st); err != nil || st.Member != seed.MemberKey() || st.Certificate.Certificate.Device != dev || st.Network != "testnet" {
		t.Fatalf("the identity handed over: %+v %v", st.Certificate.Certificate, err)
	}
	devices, err := loadDevices(dd)
	if err != nil || len(devices) != 1 || devices[0].Device != dev.String() || devices[0].Certificate == "" {
		t.Fatalf("the record of the paired device: %+v %v", devices, err)
	}
}
