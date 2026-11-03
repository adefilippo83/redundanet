package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
)

// storage drop removes one complete share from a node's store and the
// census reports the removal, which is what the repair drill rests on.
func TestStorageDropIsSeenByTheCensus(t *testing.T) {
	disk, dataDir := t.TempDir(), t.TempDir()
	st, err := openStore(disk, dataDir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seed identity.Seed
	seed[0] = 1
	dev, _ := identity.NewDeviceKey()
	cert, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: seed.MemberKey(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("drill "), 2000)
	r := bytes.NewReader(data)
	h, _ := object.HashSpool(r, int64(len(data)))
	enc, err := object.Encode(r, int64(len(data)), h, object.Params{K: 2, N: 4, SegmentSize: 4096, Cipher: object.CipherAESGCM}, object.ConvergenceSecret(seed.Secret()), seed.MemberKey())
	if err != nil {
		t.Fatal(err)
	}
	sd, err := object.Sign(enc.Descriptor, dev, cert)
	if err != nil {
		t.Fatal(err)
	}
	blocks := map[int][][]byte{}
	_ = enc.Shares(context.Background(), func(share, seg int, b []byte) error {
		blocks[share] = append(blocks[share], append([]byte(nil), b...))
		return nil
	})
	for _, n := range []uint8{1, 2} {
		p, err := st.BeginPut(sd, n)
		if err != nil {
			t.Fatal(err)
		}
		for seg, b := range blocks[int(n)] {
			if err := p.WriteBlock(seg, b); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Complete(); err != nil {
			t.Fatal(err)
		}
	}
	gen, _ := st.Generation()
	_ = st.Close()
	si := sd.Descriptor.StorageIndex

	// The command opens the same store in its own process, as it would
	// beside the running agent.
	if err := storageDrop(disk, dataDir, 1, si, 2, "drill"); err != nil {
		t.Fatal(err)
	}
	if err := storageDrop(disk, dataDir, 1, si, 2, "drill"); err == nil || !strings.Contains(err.Error(), "not held here") {
		t.Fatalf("dropping twice: %v", err)
	}
	if err := storageHas(disk, dataDir, 1, si); err != nil {
		t.Fatal(err)
	}
	st, err = openStore(disk, dataDir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.Has(si)
	if err != nil || p == nil || len(p.Complete) != 1 || p.Complete[0] != 1 {
		t.Fatalf("after the drop: %+v %v", p, err)
	}
	d, err := st.Census(gen)
	if err != nil || len(d.Removed) != 1 || d.Removed[0].Number != 2 || d.Removed[0].StorageIndex != si || d.Generation <= gen {
		t.Fatalf("census after the drop: %+v %v", d, err)
	}
	if _, err := os.Stat(filepath.Join(disk, "redundanet3", "shares", si.String()[:2], si.String(), "2")); !os.IsNotExist(err) {
		t.Fatalf("the share file is still there: %v", err)
	}
	if _, err := object.ParseStorageIndex("not-base32!"); err == nil {
		t.Fatal("a bad storage index parsed")
	}
	if back, err := object.ParseStorageIndex(si.String()); err != nil || back != si {
		t.Fatalf("storage index round trip: %v", err)
	}
	_ = store.ErrNotFound
}
