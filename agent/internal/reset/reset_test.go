package reset

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A reset removes what belongs to the identity and keeps the program, the
// updater's record and the network's descriptor.
func TestClearKeepsTheProgramAndDropsTheIdentity(t *testing.T) {
	dir := t.TempDir()
	stay := []string{"bin/redundanet", "launcher/redundanet", "versions/3.0.39/redundanet", "updates/state.yaml", "network.yaml", "service.log",
		"storage-id", "redundanet3/shares/ab/share"}
	gone := []string{"identity", "device-key.pending", "pairing.code", "manifest.yaml", "manifest.sig", "backup.yaml", "devices.yaml",
		"local.sqlite", "local.sqlite-wal", "local.sqlite-shm", "api.addr", "api.token", "spool/upload.tmp", "something-new"}
	for _, f := range append(append([]string{}, stay...), gone...) {
		write(t, filepath.Join(dir, f))
	}
	removed, err := Clear(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api.addr", "api.token", "backup.yaml", "device-key.pending", "devices.yaml", "identity", "local.sqlite", "local.sqlite-shm",
		"local.sqlite-wal", "manifest.sig", "manifest.yaml", "pairing.code", "something-new", "spool"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed %v\n   want %v", removed, want)
	}
	for _, f := range stay {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s did not stay: %v", f, err)
		}
	}
	for _, f := range gone {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s stayed", f)
		}
	}
	// A second run, and a directory that is not there, change nothing.
	if removed, err := Clear(dir, false); err != nil || len(removed) != 0 {
		t.Fatalf("second run: %v %v", removed, err)
	}
	if removed, err := Clear(filepath.Join(dir, "nowhere"), false); err != nil || removed != nil {
		t.Fatalf("a missing directory: %v %v", removed, err)
	}
	// The store and its record go only when asked, and together.
	if removed, err := Clear(dir, true); err != nil || !reflect.DeepEqual(removed, []string{"redundanet3", "storage-id"}) {
		t.Fatalf("with the store: %v %v", removed, err)
	}
}

func TestEmptyKeepsTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "redundanet3")
	write(t, filepath.Join(dir, "shares", "ab", "share"))
	write(t, filepath.Join(dir, "index.sqlite"))
	if n, err := Empty(dir); err != nil || n != 2 {
		t.Fatalf("emptied %d: %v", n, err)
	}
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 0 {
		t.Fatalf("the directory after: %v %v", left, err)
	}
	if n, err := Empty(filepath.Join(dir, "nowhere")); err != nil || n != 0 {
		t.Fatalf("a missing directory: %d %v", n, err)
	}
}
