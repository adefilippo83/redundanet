package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/node"
	"github.com/adefilippo83/redundanet/agent/internal/release"
)

// `node` is the storage node's install, from the binary itself, as root
// on Linux: what agent/deploy/node/install.sh does, then pairing, so one
// command and a `device pair` on the laptop make a node.

func runNode(args []string) error {
	if len(args) == 0 {
		return errors.New("node: a subcommand is needed: install, uninstall, status")
	}
	if runtime.GOOS != "linux" {
		return errors.New("node: a storage node is a Linux machine; on a laptop use `service install`")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return nodeInstall(rest)
	case "uninstall":
		return nodeUninstall(rest)
	case "status":
		return nodeStatus(rest)
	}
	return fmt.Errorf("node: unknown subcommand %q", sub)
}

func nodeConfig() node.Config {
	return node.Config{Root: "/", Run: func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}}
}

func needRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("node: run it with sudo")
	}
	return nil
}

func nodeInstall(args []string) error {
	fs := flag.NewFlagSet("node install", flag.ContinueOnError)
	network := fs.String("network", "redundanet", "the network to join")
	channel := fs.String("channel", release.ChannelStable, "release channel for self-update: stable or early")
	storage := fs.String("storage-path", "", "the storage disk, mounted; the store goes in its redundanet3 directory")
	allowRoot := fs.Bool("allow-root-disk", false, "accept a storage path that is not a mount point")
	noPair := fs.Bool("no-pair", false, "lay the node out and stop; pair or finish later")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storage == "" {
		return errors.New("usage: sudo redundanet node install --storage-path /mnt/storage [--network NAME] [--channel stable|early] [--allow-root-disk] [--no-pair]")
	}
	if err := needRoot(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfg := nodeConfig()
	cfg.Network, cfg.Channel, cfg.StoragePath, cfg.Exe, cfg.AllowRootDisk = *network, *channel, *storage, exe, *allowRoot
	rep, err := node.Install(cfg)
	if err != nil {
		return err
	}
	for _, s := range rep.Steps {
		fmt.Println(s)
	}
	if out, err := exec.Command(rep.Binary, "version").Output(); err == nil {
		fmt.Printf("installed %s", out)
	}
	if _, err := os.Stat(filepath.Join(node.DataDir, "identity")); err == nil {
		fmt.Println("this node already has an identity; starting the service")
		return startNode()
	}
	if *noPair {
		fmt.Printf("laid out without an identity; next: sudo %s node install again without --no-pair, or install.sh finish with a certificate file\n", exe)
		return nil
	}
	// Pairing, as the service user so the identity is its own: the code
	// shows here, the member's other device answers it.
	fmt.Println()
	fmt.Printf("On a device of yours that has its identity and reaches this node, with the phrase at hand:\n  redundanet device pair --network %s --storage <the code shown below>\n\n", *network)
	if err := asServiceUser(rep.Binary, "identity", "pair", "--network", *network, "--data-dir", node.DataDir); err != nil {
		return fmt.Errorf("pairing did not complete (%w); run the install again for a new code, or install.sh finish with a certificate file", err)
	}
	if err := startNode(); err != nil {
		return err
	}
	st, err := identity.Load(filepath.Join(node.DataDir, "identity"), *network, nil)
	if err != nil {
		return nil
	}
	dev, err := st.Device()
	if err != nil {
		return nil
	}
	host, _ := os.Hostname()
	fmt.Println()
	fmt.Println("to be counted, the node must be listed under storage: in the manifest; these are its lines for the join request or the pull request:")
	fmt.Print(node.StorageEntry(dev.ID().String(), host))
	return nil
}

// asServiceUser runs the binary as the service user with the terminal
// attached, through runuser or sudo, whichever the system has.
func asServiceUser(bin string, args ...string) error {
	var cmd *exec.Cmd
	if p, err := exec.LookPath("runuser"); err == nil {
		cmd = exec.Command(p, append([]string{"-u", node.User, "--", bin}, args...)...)
	} else if p, err := exec.LookPath("sudo"); err == nil {
		cmd = exec.Command(p, append([]string{"-u", node.User, bin}, args...)...)
	} else {
		return errors.New("neither runuser nor sudo is installed to run the pairing as the service user")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func startNode() error {
	if out, err := exec.Command("systemctl", "enable", "--now", node.UnitName).CombinedOutput(); err != nil {
		return fmt.Errorf("starting the service: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("%s enabled and started; the agent as it sees itself: sudo %s/bin/redundanet status\n", node.UnitName, node.DataDir)
	return nil
}

func nodeUninstall(args []string) error {
	fs := flag.NewFlagSet("node uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := needRoot(); err != nil {
		return err
	}
	removed, err := node.Uninstall(nodeConfig())
	if err != nil {
		return err
	}
	fmt.Printf("service stopped and %d files removed; %s (identity, settings, updates) and the store under the storage path stay, yours to delete\n", len(removed), node.DataDir)
	return nil
}

func nodeStatus(args []string) error {
	fs := flag.NewFlagSet("node status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := node.Inspect(nodeConfig())
	fmt.Printf("installed: %v\nservice:   %s\nidentity:  %v\nbinary:    %s\n", st.Installed, st.Active, st.Identity, st.Binary)
	for _, k := range []string{"NETWORK", "CHANNEL", "STORAGE_PATH"} {
		if v, ok := st.Env[k]; ok {
			fmt.Printf("%-10s %s\n", strings.ToLower(k)+":", v)
		}
	}
	return nil
}
