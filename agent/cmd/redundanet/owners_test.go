package main

import (
	"io"
	"log/slog"
	"testing"
)

// A candidate on probation leaves the store refusing a second owner, so
// the version that started it finds the store as it left it; the first
// run as the installed version turns acceptance on, and it stays on.
func TestSecondOwnersAreAcceptedOnlyAfterProbation(t *testing.T) {
	disk, data := t.TempDir(), t.TempDir()
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() (bool, func(probation bool)) {
		t.Helper()
		st, err := openStore(disk, data, 1, lg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st.SecondOwners(), func(probation bool) { acceptSecondOwners(st, probation, lg) }
	}

	on, run := open()
	if on {
		t.Fatal("a new store accepts a second owner before any run")
	}
	run(true)
	if on, _ = open(); on {
		t.Fatal("a candidate on probation turned acceptance on")
	}
	_, run = open()
	run(false)
	run(false) // a second call changes nothing
	if on, _ = open(); !on {
		t.Fatal("the installed version did not turn acceptance on")
	}
	// A later candidate finds it on and leaves it on.
	_, run = open()
	run(true)
	if on, _ = open(); !on {
		t.Fatal("acceptance did not survive a later candidate")
	}
}
