package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/service"
)

// resetDir is a laptop's data directory with an identity, state and a
// manifest copy that lists the device under storage: when listed is set.
func resetDir(t *testing.T, listed bool) (string, *identity.State) {
	t.Helper()
	dir := t.TempDir()
	st := testState(t, identity.RoleClient, identity.RoleStorage)
	if err := identity.Save(identityPath(dir), st, nil); err != nil {
		t.Fatal(err)
	}
	op, err := manifest.NewOperatorKey()
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := identity.NewDeviceKey()
	storage := ""
	if listed {
		storage = fmt.Sprintf("storage:\n  - member: alice\n    device: %s\n    name: nas-1\n", st.Certificate.Certificate.Device)
	}
	y := fmt.Sprintf("schema: 3\nrevision: 7\nnetwork:\n  name: testnet\n  encoding: {k: 2, n: 4}\noperators:\n  - name: alice\n    key: %s\nhubs:\n  - name: hub-one\n    relay: hub.example.org:4433\n    key: %s\nmembers:\n  - name: alice\n    key: %s\n    contribution_gb: 100\n    joined: 2026-11-01\n    status: active\n%s",
		op.Public(), hub.ID(), st.Member, storage)
	if _, err := manifest.Parse([]byte(y)); err != nil {
		t.Fatalf("the test's manifest: %v", err)
	}
	for name, body := range map[string]string{
		"manifest.yaml": y, "backup.yaml": "sets: []\n", "local.sqlite": "x", "devices.yaml": "[]\n",
		"bin/redundanet": "program", "updates/state.yaml": "channel: early\n",
		"storage-id": "abc\n", "redundanet3/.marker": "abc\n", "redundanet3/shares/aa": "share",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, st
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// stubService stands in for the system's service manager and records what
// the reset asked of it.
func stubService(t *testing.T, installed bool) *[]string {
	t.Helper()
	var calls []string
	q, u, i := serviceQuery, serviceUninstall, serviceInstall
	serviceQuery = func(string) service.Status { return service.Status{Installed: installed, Running: installed} }
	serviceUninstall = func(string) error { calls = append(calls, "uninstall"); return nil }
	serviceInstall = func(s service.Spec) (string, error) {
		calls = append(calls, "install "+strings.Join(s.Args, " "))
		return "unit", nil
	}
	t.Cleanup(func() { serviceQuery, serviceUninstall, serviceInstall = q, u, i })
	return &calls
}

func answer(t *testing.T, line string) {
	t.Helper()
	old := stdin
	stdin = bufio.NewReader(strings.NewReader(line))
	t.Cleanup(func() { stdin = old })
}

// A reset asks first, removes the identity and its state, keeps the
// program, the store and the updater's record, and puts the service back
// on the channel that record names.
func TestResetStartsADeviceOver(t *testing.T) {
	dir, _ := resetDir(t, true)
	t.Setenv("REDUNDANET_DATA_DIR", dir)
	calls := stubService(t, true)

	answer(t, "no\n")
	if err := runReset([]string{"--network", "testnet"}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("an answer other than the word: %v", err)
	}
	if !exists(dir, "identity") || len(*calls) != 0 {
		t.Fatalf("a refused reset changed something: %v", *calls)
	}

	answer(t, "reset\n")
	if err := runReset([]string{"--network", "testnet"}); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"identity", "manifest.yaml", "backup.yaml", "local.sqlite", "devices.yaml"} {
		if exists(dir, gone) {
			t.Errorf("%s stayed", gone)
		}
	}
	for _, stay := range []string{"bin/redundanet", "updates/state.yaml", "storage-id", "redundanet3/shares/aa"} {
		if !exists(dir, stay) {
			t.Errorf("%s went", stay)
		}
	}
	if len(*calls) != 2 || (*calls)[0] != "uninstall" || !strings.HasPrefix((*calls)[1], "install ") || !strings.Contains((*calls)[1], "--channel early") {
		t.Fatalf("the service: %v", *calls)
	}
	if !strings.Contains((*calls)[1], "--data-dir "+dir) && !strings.Contains((*calls)[1], dir) {
		t.Fatalf("the service was put back on another directory: %v", *calls)
	}
}

// A device with no service installed gets none from a reset, and a reset
// of a directory that is not the default one leaves the user's service,
// which serves the default one, alone.
func TestResetTouchesOnlyItsOwnService(t *testing.T) {
	dir, _ := resetDir(t, false)
	t.Setenv("REDUNDANET_DATA_DIR", dir)
	calls := stubService(t, false)
	if err := runReset([]string{"--network", "testnet", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if exists(dir, "identity") || len(*calls) != 0 {
		t.Fatalf("identity there %v, service calls %v", exists(dir, "identity"), *calls)
	}

	other, _ := resetDir(t, false)
	calls = stubService(t, true)
	if err := runReset([]string{"--data-dir", other, "--network", "testnet", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if exists(other, "identity") || len(*calls) != 0 {
		t.Fatalf("another directory: identity there %v, service calls %v", exists(other, "identity"), *calls)
	}
}

// The store holds other members' shares: it is emptied only when asked,
// and never while the manifest still lists the device as a storage server.
func TestResetWipesTheStoreOnlyOffTheManifest(t *testing.T) {
	dir, _ := resetDir(t, true)
	stubService(t, false)
	err := runReset([]string{"--data-dir", dir, "--network", "testnet", "--yes", "--wipe-store"})
	if err == nil || !strings.Contains(err.Error(), "nas-1") {
		t.Fatalf("a listed storage device: %v", err)
	}
	if !exists(dir, "identity") || !exists(dir, "redundanet3/shares/aa") {
		t.Fatal("a refused wipe changed something")
	}

	dir, _ = resetDir(t, false)
	if err := runReset([]string{"--data-dir", dir, "--network", "testnet", "--yes", "--wipe-store"}); err != nil {
		t.Fatal(err)
	}
	if exists(dir, "redundanet3") || exists(dir, "storage-id") || exists(dir, "identity") {
		t.Fatal("the store, its record or the identity stayed")
	}
	if !exists(dir, "bin/redundanet") {
		t.Fatal("the program went with the store")
	}
}

func TestRecordedChannel(t *testing.T) {
	dir := t.TempDir()
	if got := recordedChannel(dir); got != "" {
		t.Fatalf("no record: %q", got)
	}
	_ = os.MkdirAll(filepath.Join(dir, "updates"), 0o700)
	for body, want := range map[string]string{
		"channel: stable\n":                        "stable",
		"channel: stable\nchosen_channel: early\n": "early",
		"channel: nightly\n":                       "",
		"not yaml: [":                              "",
	} {
		_ = os.WriteFile(filepath.Join(dir, "updates", "state.yaml"), []byte(body), 0o600)
		if got := recordedChannel(dir); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
}
