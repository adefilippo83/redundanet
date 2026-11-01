package identity

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testState(t *testing.T) *State {
	t.Helper()
	seed := testSeed()
	d, sc := issueTestCert(t, seed, RoleClient)
	return &State{Network: "testnet", Member: seed.MemberKey(), MemberSecret: seed.Secret(), DeviceSeed: d.Seed(), Certificate: *sc}
}

func TestKeystorePlain(t *testing.T) {
	st := testState(t)
	path := filepath.Join(t.TempDir(), "sub", "identity")
	if err := Save(path, st, nil); err != nil {
		t.Fatal(err)
	}
	sealed, err := IsSealed(path)
	if err != nil || sealed {
		t.Fatalf("sealed=%v err=%v", sealed, err)
	}
	back, err := Load(path, "testnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.Member != st.Member || back.MemberSecret != st.MemberSecret || string(back.DeviceSeed) != string(st.DeviceSeed) {
		t.Fatal("state changed on the way through the file")
	}
	if _, err := Load(path, "othernet", nil); err == nil {
		t.Fatal("a file for another network loaded")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", info.Mode().Perm())
		}
	}
}

func TestKeystoreSealed(t *testing.T) {
	st := testState(t)
	path := filepath.Join(t.TempDir(), "identity")
	if err := Save(path, st, []byte("correct horse")); err != nil {
		t.Fatal(err)
	}
	if sealed, _ := IsSealed(path); !sealed {
		t.Fatal("not reported as sealed")
	}
	if _, err := Load(path, "testnet", nil); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("no passphrase: %v", err)
	}
	if _, err := Load(path, "testnet", []byte("wrong")); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Load(path, "othernet", []byte("correct horse")); err == nil {
		t.Fatal("the network is not bound into the seal")
	}
	back, err := Load(path, "testnet", []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if back.MemberSecret != st.MemberSecret {
		t.Fatal("secret changed")
	}
}

func TestKeystoreRefusesMismatchedCertificate(t *testing.T) {
	st := testState(t)
	// A certificate for another device in this device's file.
	other, _ := NewDeviceKey()
	sc, err := Issue(testSeed(), Certificate{Network: "testnet", Device: other.ID(), Roles: []string{RoleClient}, IssuedAt: time.Now(), Serial: 2})
	if err != nil {
		t.Fatal(err)
	}
	st.Certificate = *sc
	path := filepath.Join(t.TempDir(), "identity")
	if err := Save(path, st, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, "testnet", nil); err == nil {
		t.Fatal("a file whose certificate names another device loaded")
	}
}
