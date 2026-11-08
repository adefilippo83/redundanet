package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// On macOS the service runs the agent through a launcher copied once into
// the data directory and never replaced; elsewhere it runs the binary.
func TestAgentSpecUsesALauncherOnMacOS(t *testing.T) {
	d := t.TempDir()
	exe := filepath.Join(d, "bin", "redundanet")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("version one"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := agentSpec(exe, d, "testnet", "early")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agent", "--network", "testnet", "--data-dir", d, "--channel", "early"}
	if runtime.GOOS != "darwin" {
		if spec.Executable != exe || strings.Join(spec.Args, " ") != strings.Join(want, " ") {
			t.Fatalf("spec %+v", spec)
		}
		return
	}
	launcher := filepath.Join(d, "launcher", "redundanet")
	if spec.Executable != launcher || strings.Join(spec.Args, " ") != strings.Join(append([]string{"launch", "--", exe}, want...), " ") {
		t.Fatalf("spec %+v", spec)
	}
	b, err := os.ReadFile(launcher)
	if err != nil || string(b) != "version one" {
		t.Fatalf("launcher copy: %q %v", b, err)
	}
	if info, _ := os.Stat(launcher); info.Mode().Perm()&0o100 == 0 {
		t.Fatal("the launcher is not executable")
	}
	// A new version of the binary leaves the launcher as it was.
	if err := os.WriteFile(exe, []byte("version two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := agentSpec(exe, d, "testnet", "stable"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(launcher); string(b) != "version one" {
		t.Fatal("the launcher was replaced by the new version")
	}
}
