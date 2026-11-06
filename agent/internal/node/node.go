// Package node lays out a Linux storage node the way agent/deploy/node's
// installer does, from inside the binary: the service user, the directories,
// the binary in place, the unit and its store drop-in, the socket buffers.
// What runs as root is explicit, injectable and tested on a scratch root.
package node

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Fixed by the layout every runbook and the updater know.
const (
	User       = "redundanet3"
	DataDir    = "/var/lib/redundanet3"
	EnvFile    = "/etc/redundanet3/agent.env"
	UnitName   = "redundanet-agent.service"
	unitDir    = "/etc/systemd/system"
	sysctlFile = "/etc/sysctl.d/90-redundanet-agent.conf"
	storeDir   = "redundanet3" // under the storage path
)

//go:embed redundanet-agent.service
var unitFile string

// Config is what an install needs.
type Config struct {
	Network     string
	Channel     string
	StoragePath string
	// Exe is the binary to put in place, this one.
	Exe string
	// AllowRootDisk lets the storage path be a directory of the system
	// disk; refused otherwise, so shares never land on an SD card or the
	// root filesystem by accident.
	AllowRootDisk bool
	// Root prefixes every path, "/" normally, a scratch directory in tests.
	Root string
	// Run executes a command as root: useradd, chown, sysctl, systemctl.
	Run func(name string, args ...string) (string, error)
	// MountPoint reports whether a path is a mount point; nil uses the
	// filesystem.
	MountPoint func(path string) (bool, error)
}

// Report says what Install did.
type Report struct {
	UserMade bool
	EnvKept  bool // agent.env existed and was left as it was
	Binary   string
	Unit     string
	Store    string
	Steps    []string
}

func (c *Config) path(p string) string {
	if c.Root == "" || c.Root == "/" {
		return p
	}
	return filepath.Join(c.Root, p)
}

// Install lays the node out. It is idempotent: a second run refreshes the
// binary and the unit and keeps the settings and the identity.
func Install(cfg Config) (*Report, error) {
	if cfg.Network == "" || cfg.StoragePath == "" || cfg.Exe == "" {
		return nil, errors.New("node: network, storage path and the binary are required")
	}
	if cfg.Channel == "" {
		cfg.Channel = "stable"
	}
	if cfg.Run == nil {
		return nil, errors.New("node: no way to run commands")
	}
	if cfg.MountPoint == nil {
		cfg.MountPoint = isMountPoint
	}
	rep := &Report{}
	step := func(format string, a ...any) { rep.Steps = append(rep.Steps, fmt.Sprintf(format, a...)) }

	// The storage path: a mount point, unless the operator knows better.
	sp := cfg.path(cfg.StoragePath)
	if fi, err := os.Stat(sp); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("node: the storage path %s is not a directory", cfg.StoragePath)
	}
	if mp, err := cfg.MountPoint(sp); err == nil && !mp && !cfg.AllowRootDisk {
		return nil, fmt.Errorf("node: %s is not a mount point: shares would land on the system disk; mount the storage disk there, or say --allow-root-disk", cfg.StoragePath)
	}

	// The service user.
	if _, err := cfg.Run("id", User); err != nil {
		if _, err := cfg.Run("useradd", "--system", "--home", DataDir, "--shell", "/usr/sbin/nologin", User); err != nil {
			return nil, fmt.Errorf("node: making the user %s: %w", User, err)
		}
		rep.UserMade = true
		step("made the service user %s", User)
	}

	// Directories and the binary, owned by the user so the updater can
	// rename a new version into place.
	data := cfg.path(DataDir)
	bin := filepath.Join(data, "bin")
	for _, d := range []string{data, bin} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.path(filepath.Dir(EnvFile)), 0o755); err != nil {
		return nil, err
	}
	rep.Binary = filepath.Join(bin, "redundanet")
	if err := copyFile(cfg.Exe, rep.Binary, 0o755); err != nil {
		return nil, fmt.Errorf("node: putting the binary in place: %w", err)
	}
	if _, err := cfg.Run("chown", "-R", User+":"+User, data); err != nil {
		return nil, fmt.Errorf("node: chown %s: %w", data, err)
	}
	step("binary at %s, data in %s, owned by %s", rep.Binary, DataDir, User)

	// Settings, kept when present.
	env := cfg.path(EnvFile)
	if _, err := os.Stat(env); err == nil {
		rep.EnvKept = true
		step("kept %s", EnvFile)
	} else {
		if err := os.WriteFile(env, []byte(fmt.Sprintf("NETWORK=%s\nCHANNEL=%s\nSTORAGE_PATH=%s\n", cfg.Network, cfg.Channel, cfg.StoragePath)), 0o644); err != nil {
			return nil, err
		}
		step("wrote %s", EnvFile)
	}

	// The unit and the store drop-in.
	if err := os.MkdirAll(cfg.path(unitDir), 0o755); err != nil {
		return nil, err
	}
	rep.Unit = filepath.Join(cfg.path(unitDir), UnitName)
	if err := os.WriteFile(rep.Unit, []byte(unitFile), 0o644); err != nil {
		return nil, err
	}
	// The store as the node knows it: a Linux path in the node's files,
	// whatever host runs the tests.
	store := path.Join(cfg.StoragePath, storeDir)
	rep.Store = filepath.Join(sp, storeDir)
	if err := os.MkdirAll(rep.Store, 0o750); err != nil {
		return nil, err
	}
	if _, err := cfg.Run("chown", User+":"+User, store); err != nil {
		return nil, fmt.Errorf("node: chown %s: %w", store, err)
	}
	dropDir := rep.Unit + ".d"
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dropDir, "storage.conf"), []byte(fmt.Sprintf("[Service]\nReadWritePaths=%s\n", store)), 0o644); err != nil {
		return nil, err
	}
	step("unit %s with the store %s", UnitName, store)

	// quic-go asks for 7 MiB socket buffers; only the kernel can allow them.
	sc := cfg.path(sysctlFile)
	if _, err := os.Stat(sc); err != nil {
		if err := os.MkdirAll(filepath.Dir(sc), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(sc, []byte("# RedundaNet 3 agent: let quic-go size its UDP socket buffers (7 MiB)\nnet.core.rmem_max = 7500000\nnet.core.wmem_max = 7500000\n"), 0o644); err != nil {
			return nil, err
		}
		_, _ = cfg.Run("sysctl", "-q", "-p", sysctlFile)
		step("socket buffers in %s", sysctlFile)
	}
	_, _ = cfg.Run("systemctl", "daemon-reload")
	return rep, nil
}

// Uninstall stops the service and removes the unit, its drop-in and the
// sysctl file. The data directory, the identity and the store stay, and
// the report says so; they are the operator's to delete.
func Uninstall(cfg Config) ([]string, error) {
	if cfg.Run == nil {
		return nil, errors.New("node: no way to run commands")
	}
	_, _ = cfg.Run("systemctl", "disable", "--now", UnitName)
	var removed []string
	unit := filepath.Join(cfg.path(unitDir), UnitName)
	for _, p := range []string{filepath.Join(unit+".d", "storage.conf"), unit + ".d", unit, cfg.path(sysctlFile)} {
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		}
	}
	_, _ = cfg.Run("systemctl", "daemon-reload")
	return removed, nil
}

// Status is what a node looks like from the outside.
type Status struct {
	Installed bool   // the unit exists
	Active    string // systemctl's word
	Identity  bool
	Binary    string
	Env       map[string]string
}

// Inspect reads the node without changing it.
func Inspect(cfg Config) Status {
	st := Status{Env: map[string]string{}}
	if _, err := os.Stat(filepath.Join(cfg.path(unitDir), UnitName)); err == nil {
		st.Installed = true
	}
	if _, err := os.Stat(filepath.Join(cfg.path(DataDir), "identity")); err == nil {
		st.Identity = true
	}
	if _, err := os.Stat(filepath.Join(cfg.path(DataDir), "bin", "redundanet")); err == nil {
		st.Binary = filepath.Join(DataDir, "bin", "redundanet")
	}
	if b, err := os.ReadFile(cfg.path(EnvFile)); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				st.Env[k] = v
			}
		}
	}
	if cfg.Run != nil {
		if out, err := cfg.Run("systemctl", "is-active", UnitName); err == nil {
			st.Active = strings.TrimSpace(out)
		} else {
			st.Active = "inactive"
		}
	}
	return st
}

// StorageEntry is the manifest lines that list this node under storage:,
// for the join request or the pull request.
func StorageEntry(device, name string) string {
	return fmt.Sprintf("storage:\n  - member: <your member name>\n    device: %s\n    name: %s\n", device, name)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
