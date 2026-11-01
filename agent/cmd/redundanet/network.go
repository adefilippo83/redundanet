package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// The network commands handle the descriptor: operators print one for an
// invite link, a device joins with one, and status shows what the device
// currently believes about its network.
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
	if fs.NArg() != 1 {
		return errors.New("usage: network join <descriptor>")
	}
	d, err := membership.ParseDescriptor(fs.Arg(0))
	if err != nil {
		return err
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
	dir, _ := commonFlags(fs)
	refresh := fs.Bool("refresh", false, "fetch before printing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dd, err := dataDir(*dir)
	if err != nil {
		return err
	}
	m, err := membership.Open(membership.Config{DataDir: dd, Version: version.Version, Log: quietLog()})
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
func openMembership(ctx context.Context, dd string, onApply func(*manifest.Manifest)) (*membership.Membership, error) {
	m, err := membership.Open(membership.Config{DataDir: dd, Version: version.Version, OnApply: onApply})
	if err != nil {
		return nil, err
	}
	m.Start(ctx)
	if m.Manifest() == nil {
		return nil, fmt.Errorf("no manifest for %s yet and the first fetch failed: %s", m.Descriptor().Network, m.Status().LastError)
	}
	return m, nil
}

// quietLog keeps the one-shot commands' output to what they print.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
