package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	calls  [][]string
	noUser bool
	active string
}

func (r *recorder) run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "id" && r.noUser {
		return "", os.ErrNotExist
	}
	if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
		return r.active + "\n", nil
	}
	return "", nil
}

func (r *recorder) ran(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func scratch(t *testing.T) (Config, *recorder) {
	t.Helper()
	root := t.TempDir()
	exe := filepath.Join(root, "redundanet-download")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho redundanet\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mnt", "storage"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{noUser: true, active: "active"}
	return Config{Network: "redundanet", StoragePath: "/mnt/storage", Exe: exe, Root: root, Run: rec.run,
		MountPoint: func(string) (bool, error) { return true, nil }}, rec
}

func TestInstallLaysTheNodeOut(t *testing.T) {
	cfg, rec := scratch(t)
	rep, err := Install(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.UserMade || !rec.ran("useradd --system --home /var/lib/redundanet3 --shell /usr/sbin/nologin redundanet3") {
		t.Fatalf("user: %+v %v", rep, rec.calls)
	}
	for _, p := range []string{"var/lib/redundanet3/bin/redundanet", "etc/redundanet3/agent.env", "etc/systemd/system/redundanet-agent.service", "etc/systemd/system/redundanet-agent.service.d/storage.conf", "mnt/storage/redundanet3", "etc/sysctl.d/90-redundanet-agent.conf"} {
		if _, err := os.Stat(filepath.Join(cfg.Root, p)); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	env, _ := os.ReadFile(filepath.Join(cfg.Root, "etc/redundanet3/agent.env"))
	if string(env) != "NETWORK=redundanet\nCHANNEL=stable\nSTORAGE_PATH=/mnt/storage\n" {
		t.Fatalf("agent.env: %q", env)
	}
	drop, _ := os.ReadFile(filepath.Join(cfg.Root, "etc/systemd/system/redundanet-agent.service.d/storage.conf"))
	if !strings.Contains(string(drop), "ReadWritePaths=/mnt/storage/redundanet3") {
		t.Fatalf("drop-in: %q", drop)
	}
	unit, _ := os.ReadFile(filepath.Join(cfg.Root, "etc/systemd/system/redundanet-agent.service"))
	if !strings.Contains(string(unit), "ExecStart=/var/lib/redundanet3/bin/redundanet agent") {
		t.Fatalf("unit: %.80q", unit)
	}
	if !rec.ran("chown -R redundanet3:redundanet3") || !rec.ran("sysctl -q -p") || !rec.ran("systemctl daemon-reload") {
		t.Fatalf("commands: %v", rec.calls)
	}
	// Again: the user exists, the settings are kept, the binary is refreshed.
	rec.noUser = false
	_ = os.WriteFile(filepath.Join(cfg.Root, "etc/redundanet3/agent.env"), []byte("NETWORK=redundanet\nCHANNEL=early\nSTORAGE_PATH=/mnt/storage\n"), 0o644)
	rep, err = Install(cfg)
	if err != nil || rep.UserMade || !rep.EnvKept {
		t.Fatalf("second install: %+v %v", rep, err)
	}
	st := Inspect(cfg)
	if !st.Installed || st.Identity || st.Env["CHANNEL"] != "early" || st.Active != "active" || st.Binary == "" {
		t.Fatalf("inspect: %+v", st)
	}
	removed, err := Uninstall(cfg)
	if err != nil || len(removed) != 4 || Inspect(cfg).Installed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "var/lib/redundanet3/bin/redundanet")); err != nil {
		t.Fatal("uninstall removed the data directory")
	}
}

func TestInstallRefusesTheSystemDisk(t *testing.T) {
	cfg, _ := scratch(t)
	cfg.MountPoint = func(string) (bool, error) { return false, nil }
	if _, err := Install(cfg); err == nil || !strings.Contains(err.Error(), "not a mount point") {
		t.Fatalf("a plain directory was accepted: %v", err)
	}
	cfg.AllowRootDisk = true
	if _, err := Install(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.StoragePath = "/mnt/missing"
	if _, err := Install(cfg); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a missing path was accepted: %v", err)
	}
}

// The unit the binary carries is the one the runbook ships.
func TestEmbeddedUnitMatchesTheRunbook(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "deploy", "node", "redundanet-agent.service"))
	if err != nil {
		t.Skip("runbook unit not beside the module")
	}
	if string(shipped) != unitFile {
		t.Fatal("internal/node/redundanet-agent.service differs from deploy/node/redundanet-agent.service; copy it over")
	}
}

func TestStorageEntry(t *testing.T) {
	s := StorageEntry("abc", "falcon-2")
	if !strings.HasPrefix(s, "storage:\n") || !strings.Contains(s, "device: abc") || !strings.Contains(s, "name: falcon-2") {
		t.Fatalf("%q", s)
	}
}
