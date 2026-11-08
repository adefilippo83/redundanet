package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// The network commands handle the descriptor: operators print one for an
// invite link, a device joins with one, and status shows what the device
// currently believes about its network. A build carries the descriptor of
// its own network (version.Network): it is the default of every --network
// flag, `network join` takes it when no descriptor is given, and a device
// that never joined by hand joins it the first time it needs the network.
func runNetwork(args []string) error {
	if len(args) == 0 {
		return errors.New("network: a subcommand is needed: descriptor, join, status")
	}
	switch args[0] {
	case "descriptor":
		return networkDescriptor(args[1:])
	case "join":
		return networkJoin(args[1:])
	case "status":
		return networkStatus(args[1:])
	}
	return fmt.Errorf("network: unknown subcommand %q", args[0])
}

func networkDescriptor(args []string) error {
	fs := flag.NewFlagSet("network descriptor", flag.ContinueOnError)
	mPath := fs.String("manifest", "manifests/manifest.yaml", "manifest file to take the name, hubs and operators from")
	repo := fs.String("repository", "", "base URL under which manifests/manifest.yaml and manifest.sig are served, for example https://raw.githubusercontent.com/OWNER/REPO/main")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return errors.New("--repository is required")
	}
	data, err := os.ReadFile(*mPath)
	if err != nil {
		return err
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return err
	}
	d := &membership.Descriptor{Network: m.Network.Name, Repository: *repo}
	for _, h := range m.Hubs {
		d.Hubs = append(d.Hubs, membership.HubHint{Key: h.Key, Relay: h.Relay})
	}
	for _, o := range m.Operators {
		d.Operators = append(d.Operators, o.Key)
	}
	s, err := d.Encode()
	if err != nil {
		return err
	}
	fmt.Println(s)
	return nil
}

func networkJoin(args []string) error {
	fs := flag.NewFlagSet("network join", flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var d *membership.Descriptor
	var err error
	switch fs.NArg() {
	case 0:
		if d, err = builtinDescriptor(); err != nil {
			return fmt.Errorf("usage: network join <descriptor>: %w", err)
		}
	case 1:
		if d, err = membership.ParseDescriptor(fs.Arg(0)); err != nil {
			return err
		}
	default:
		return errors.New("usage: network join [descriptor]")
	}
	dd, err := dataDir(*dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dd, 0o700); err != nil {
		return err
	}
	m, err := membership.Open(membership.Config{DataDir: dd, Descriptor: d, Version: version.Version, Log: quietLog()})
	if err != nil {
		return err
	}
	// A descriptor given by hand is pinned; the built-in one follows releases.
	if fs.NArg() == 0 {
		_ = os.WriteFile(filepath.Join(dd, builtinMarker), []byte(version.Network), 0o644)
	} else {
		_ = os.Remove(filepath.Join(dd, builtinMarker))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m.Refresh(ctx)
	st := m.Status()
	if st.LastError != "" {
		return fmt.Errorf("descriptor saved, but the first fetch failed: %s", st.LastError)
	}
	fmt.Printf("joined %s: manifest revision %d, %d members, %d hubs, signed %s\n", st.Network, st.Revision, st.Members, st.Hubs, st.SignedAt.Format(time.RFC3339))
	fmt.Printf("operators pinned: %d; the identity of this device must now be certified by a member listed there\n", len(d.Operators))
	return nil
}

func networkStatus(args []string) error {
	fs := flag.NewFlagSet("network status", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	refresh := fs.Bool("refresh", false, "fetch before printing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dd, err := dataDir(*dir)
	if err != nil {
		return err
	}
	cfg := membershipConfig(dd, *network)
	cfg.Log = quietLog()
	m, err := membership.Open(cfg)
	if err != nil {
		return err
	}
	if *refresh {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		m.Refresh(ctx)
		cancel()
	}
	b, _ := json.MarshalIndent(m.Status(), "", "  ")
	fmt.Println(string(b))
	return nil
}

// openMembership is what agent and hub use: the descriptor and the last
// good manifest from the data directory, refreshed in the background.
func openMembership(ctx context.Context, dd, network string, onApply func(*manifest.Manifest)) (*membership.Membership, error) {
	cfg := membershipConfig(dd, network)
	cfg.OnApply = onApply
	m, err := membership.Open(cfg)
	if err != nil {
		return nil, err
	}
	m.Start(ctx)
	if m.Manifest() == nil {
		return nil, fmt.Errorf("no manifest for %s yet and the first fetch failed: %s", m.Descriptor().Network, m.Status().LastError)
	}
	return m, nil
}

// builtinMarker, beside the saved descriptor, holds the built-in descriptor
// the device was seeded from. Present, it says the device never joined by
// hand, so a release that carries a changed descriptor (a new hub, a
// rotated operator key) is followed; absent, the descriptor given to
// `network join` or handed over by pairing is pinned until the next one.
const builtinMarker = "network.builtin"

// membershipConfig opens the network a device joined: the descriptor saved
// in the data directory, or, when there is none for the built-in network
// or the saved one came from an earlier build, the built-in descriptor,
// which membership.Open then saves, so the device is joined from here on.
func membershipConfig(dd, network string) membership.Config {
	cfg := membership.Config{DataDir: dd, Version: version.Version}
	d, err := builtinDescriptor()
	if err != nil || d.Network != network {
		return cfg
	}
	seeded, _ := os.ReadFile(filepath.Join(dd, builtinMarker))
	if _, err := os.Stat(filepath.Join(dd, membership.DescriptorFile)); err == nil {
		if len(seeded) == 0 || string(seeded) == version.Network {
			return cfg // joined by hand, or seeded from this very descriptor
		}
	}
	cfg.Descriptor = d
	_ = os.MkdirAll(dd, 0o700)
	_ = os.WriteFile(filepath.Join(dd, builtinMarker), []byte(version.Network), 0o644)
	return cfg
}

// builtinDescriptor is the network this build belongs to, or an error for a
// build without one.
func builtinDescriptor() (*membership.Descriptor, error) {
	if version.Network == "" {
		return nil, errors.New("this build has no network built in")
	}
	d, err := membership.ParseDescriptor(version.Network)
	if err != nil {
		return nil, fmt.Errorf("the network built into this binary: %w", err)
	}
	return d, nil
}

// defaultNetwork is the name of the built-in network, the default of every
// --network flag; empty in a build without one, where the flag is required.
func defaultNetwork() string {
	d, err := builtinDescriptor()
	if err != nil {
		return ""
	}
	return d.Network
}

// networkArg is " --network NAME" for a command printed as a next step, or
// nothing when NAME is the built-in network, which needs no flag.
func networkArg(network string) string {
	if network == defaultNetwork() {
		return ""
	}
	return " --network " + network
}

// quietLog keeps the one-shot commands' output to what they print.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
