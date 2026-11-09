package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/pairing"
)

// `identity pair --code-file` is the Pi image's first boot: the code goes
// to a file, the same code comes back after a restart, and once a device of
// the member has answered it the code and its secret are gone.
func TestIdentityPairCodeFileKeepsTheCode(t *testing.T) {
	d := t.TempDir()
	codeFile := filepath.Join(d, "code.txt")
	args := []string{"--network", "testnet", "--data-dir", d, "--code-file", codeFile}
	err := identityPair(append(args, "--wait", "300ms"))
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("the first run, with nobody answering: %v", err)
	}
	first, err := os.ReadFile(codeFile)
	if err != nil || !strings.HasPrefix(string(first), "rn3:pair:") {
		t.Fatalf("the code file after the first run: %q %v", first, err)
	}
	var kept pairingState
	if b, err := os.ReadFile(filepath.Join(d, pairingStateFile)); err != nil || json.Unmarshal(b, &kept) != nil || kept.Port == 0 {
		t.Fatalf("the code was not kept for the next run: %v", err)
	}

	// The second run writes the same code back (the first boot removes the
	// file before each run, so its return says the listener is up).
	os.Remove(codeFile)
	done := make(chan error, 1)
	go func() { done <- identityPair(append(args, "--wait", "30s")) }()
	var code string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(codeFile); err == nil {
			code = strings.TrimSpace(string(b))
			break
		}
	}
	if code != strings.TrimSpace(string(first)) {
		t.Fatalf("the second run shows %q, the first showed %q", code, first)
	}

	// A device of the member answers it, over loopback here, with an
	// identity made for the node's pending key.
	c, err := pairing.Parse(code)
	if err != nil {
		t.Fatal(err)
	}
	c.Addrs = []string{fmt.Sprintf("127.0.0.1:%d", kept.Port)}
	var seed identity.Seed
	seed[0] = 9
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err = pairing.Pair(ctx, c.String(), "testnet", func(device identity.Key, _ string) (*pairing.Offer, error) {
		sc, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: device, Roles: []string{identity.RoleClient, identity.RoleStorage}, IssuedAt: time.Now(), Serial: 3})
		if err != nil {
			return nil, err
		}
		b, err := identity.Marshal(&identity.State{Network: "testnet", Member: sc.Certificate.Member, MemberSecret: seed.Secret(), Certificate: *sc})
		if err != nil {
			return nil, err
		}
		return &pairing.Offer{State: b}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Load(identityPath(d), "testnet", nil); err != nil {
		t.Fatalf("no identity after pairing: %v", err)
	}
	for _, f := range []string{codeFile, filepath.Join(d, pairingStateFile)} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s survived the pairing", filepath.Base(f))
		}
	}
}
