//go:build darwin

package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLaunchdReinstall installs a throwaway user agent twice in a row:
// the second install of a running service is what raced launchd's
// asynchronous bootout. It loads a real launchd agent into the caller's
// session, so it runs only when asked:
//
//	REDUNDANET_LAUNCHD_TEST=1 go test ./internal/service -run Reinstall -v
func TestLaunchdReinstall(t *testing.T) {
	if os.Getenv("REDUNDANET_LAUNCHD_TEST") == "" {
		t.Skip("set REDUNDANET_LAUNCHD_TEST=1 to load a throwaway launchd agent")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "agent.sh")
	// Stops a couple of seconds after SIGTERM, like an agent closing its transports.
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap 'sleep 2; exit 0' TERM\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Spec{Executable: script, DataDir: dir, Label: "reinstall-test"}
	t.Cleanup(func() {
		_ = Uninstall(s.Label)
		// bootout is asynchronous; leave launchd clean for the next run.
		for i := 0; i < 100; i++ {
			if _, err := run("launchctl", "print", launchdDomain()+"/org.redundanet."+s.Label); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	for i := 1; i <= 2; i++ {
		if _, err := Install(s); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
		time.Sleep(time.Second)
		if st := Query(s.Label); !st.Running {
			t.Fatalf("install %d: not running: %+v", i, st)
		}
	}
}
