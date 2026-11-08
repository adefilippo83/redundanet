package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// The wizard's state machine: a phrase is drawn and confirmed by three
// words before anything is written; a wrong word keeps it; a restore from a
// typed phrase yields the same member; nothing overwrites an identity.
func TestSetupStateNewMember(t *testing.T) {
	d := t.TempDir()
	st := newSetupState(d, "testnet", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if st.status().Phase != "choose" {
		t.Fatalf("phase %q", st.status().Phase)
	}
	words, check, err := st.begin(true)
	if err != nil || len(words) != 12 || len(check) != 3 {
		t.Fatalf("begin: %v %v %v", words, check, err)
	}
	if s := st.status(); s.Phase != "confirm" || len(s.Check) != 3 || !s.Storage {
		t.Fatalf("status after begin: %+v", s)
	}
	if _, err := os.Stat(identityPath(d)); err == nil {
		t.Fatal("identity written before confirmation")
	}
	wrong := []string{"x", "y", "z"}
	if err := st.confirm(wrong); err == nil || !strings.Contains(err.Error(), "word") {
		t.Fatalf("wrong words: %v", err)
	}
	if st.status().Phase != "confirm" {
		t.Fatal("a wrong word dropped the phrase")
	}
	right := make([]string, 3)
	for i, n := range check {
		right[i] = strings.ToUpper(words[n-1]) + " " // case and spacing do not matter
	}
	if err := st.confirm(right); err != nil {
		t.Fatal(err)
	}
	if st.status().Phase != "done" || st.seed != nil {
		t.Fatalf("after confirm: phase %q, seed kept %v", st.status().Phase, st.seed != nil)
	}
	saved, err := identity.Load(identityPath(d), "testnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := identity.ParsePhrase(words)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Member != seed.MemberKey() || !saved.Certificate.HasRole(identity.RoleStorage) {
		t.Fatalf("identity %+v does not match the phrase or lacks the storage role", saved.Certificate.Certificate)
	}
	if _, _, err := st.begin(false); err == nil {
		t.Fatal("begin after done succeeded")
	}
	if err := st.restore(strings.Join(words, " "), false); err == nil {
		t.Fatal("restore over an identity succeeded")
	}
}

func TestSetupStateRestoreAndCancel(t *testing.T) {
	d := t.TempDir()
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := newSetupState(d, "testnet", lg)
	words, _, err := st.begin(false)
	if err != nil {
		t.Fatal(err)
	}
	st.cancel()
	if s := st.status(); s.Phase != "choose" || st.seed != nil {
		t.Fatalf("cancel left %+v", s)
	}
	if err := st.restore("not twelve words", false); err == nil {
		t.Fatal("a bad phrase restored")
	}
	if err := st.restore(strings.Join(words, " "), false); err != nil {
		t.Fatal(err)
	}
	saved, err := identity.Load(identityPath(d), "testnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := identity.ParsePhrase(words)
	if saved.Member != seed.MemberKey() || saved.Certificate.Certificate.Serial < 2 {
		t.Fatalf("restored %+v", saved.Certificate.Certificate)
	}
	// A pairing code can be shown and cancelled; the pending device key stays
	// for the next attempt.
	other := t.TempDir()
	st2 := newSetupState(other, "testnet", lg)
	code, err := st2.pair()
	if err != nil || !strings.HasPrefix(code, "rn3:pair:") {
		t.Fatalf("pair: %q %v", code, err)
	}
	if again, _ := st2.pair(); again != code {
		t.Fatal("a second pair call made a new code")
	}
	if st2.status().Phase != "pairing" || st2.status().Code != code {
		t.Fatalf("status %+v", st2.status())
	}
	st2.cancel()
	if st2.status().Phase != "choose" {
		t.Fatal("cancel did not end the pairing")
	}
	if _, err := os.Stat(filepath.Join(other, "device-key.pending")); err != nil {
		t.Fatal("the pending device key was lost")
	}
}
