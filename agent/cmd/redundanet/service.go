package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/release"
	"github.com/adefilippo83/redundanet/agent/internal/service"
	"github.com/adefilippo83/redundanet/agent/internal/update"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// ---- service install | uninstall | status ----

func runService(args []string) error {
	if len(args) == 0 {
		return errors.New("service: a subcommand is needed: install, uninstall, status")
	}
	fs := flag.NewFlagSet("service "+args[0], flag.ContinueOnError)
	dir, network := commonFlags(fs)
	channel := fs.String("channel", release.ChannelStable, "install: release channel for self-update: stable or early")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "install":
		if *network == "" {
			return errors.New("--network is required")
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		d, err := dataDir(*dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		args := []string{"agent", "--network", *network, "--data-dir", d}
		if *channel != release.ChannelStable {
			args = append(args, "--channel", *channel)
		}
		spec := service.Spec{Executable: exe, Args: args, DataDir: d}
		path, err := service.Install(spec)
		if err != nil {
			return err
		}
		fmt.Printf("service installed: %s\nruns: %s %s\n", path, exe, strings.Join(spec.Args, " "))
		return nil
	case "uninstall":
		if err := service.Uninstall(""); err != nil {
			return err
		}
		fmt.Println("service removed")
		return nil
	case "status":
		b, _ := json.MarshalIndent(service.Query(""), "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("service: unknown subcommand %q", args[0])
}

// ---- release keygen | sign: the maintainer's side of self-update ----

func runRelease(args []string) error {
	if len(args) == 0 {
		return errors.New("release: a subcommand is needed: keygen, sign, add")
	}
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("release keygen", flag.ContinueOnError)
		path := fs.String("key", "release.key", "where to write the key; keep it offline, CI never holds it")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		k, err := release.NewKey()
		if err != nil {
			return err
		}
		if err := k.Save(*path); err != nil {
			return err
		}
		fmt.Printf("release key written to %s\n", *path)
		fmt.Printf("public key, to embed in builds (-X .../version.ReleaseKey=): %s\n", k.Public())
		return nil
	case "add":
		// No key here: CI and the maintainer's script both use it to put a
		// release's fragment into the index before anyone signs.
		fs := flag.NewFlagSet("release add", flag.ContinueOnError)
		idxPath := fs.String("index", "releases/index.yaml", "release index file, rewritten in place")
		fragPath := fs.String("fragment", "", "the release's index-fragment.yaml")
		assets := fs.String("assets", "", "a directory with the release's archives as downloaded, checked against the fragment")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *fragPath == "" {
			return errors.New("usage: release add --fragment index-fragment.yaml [--assets DIR] [--index releases/index.yaml]")
		}
		index, err := os.ReadFile(*idxPath)
		if err != nil {
			return err
		}
		fragment, err := os.ReadFile(*fragPath)
		if err != nil {
			return err
		}
		out, rel, err := release.AddFragment(index, fragment)
		if err != nil {
			return err
		}
		if *assets != "" {
			if err := rel.CheckAssets(*assets); err != nil {
				return fmt.Errorf("the archives do not match the fragment: %w", err)
			}
		}
		if err := os.WriteFile(*idxPath, out, 0o644); err != nil {
			return err
		}
		idx, _ := release.Parse(out)
		checked := "not checked against archives"
		if *assets != "" {
			checked = fmt.Sprintf("%d archives checked by sha256 and size", len(rel.Assets))
		}
		fmt.Printf("added %s (%s channel, %s) as revision %d of %s; unsigned until `release sign`\n", rel.Version, rel.Channel, checked, idx.Revision, *idxPath)
		return nil
	case "sign":
		fs := flag.NewFlagSet("release sign", flag.ContinueOnError)
		keyPath := fs.String("key", "release.key", "release key file")
		idxPath := fs.String("index", "releases/index.yaml", "release index file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		k, err := release.LoadKey(*keyPath)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(*idxPath)
		if err != nil {
			return err
		}
		sig, err := release.Sign(k, data, time.Now())
		if err != nil {
			return err
		}
		enc, err := sig.Encode()
		if err != nil {
			return err
		}
		out := strings.TrimSuffix(*idxPath, filepath.Ext(*idxPath)) + ".sig"
		if err := os.WriteFile(out, enc, 0o644); err != nil {
			return err
		}
		fmt.Printf("signed release index revision %d as %s; signature written to %s\n", sig.Revision, k.Public().Short(), out)
		return nil
	}
	return fmt.Errorf("release: unknown subcommand %q", args[0])
}

// ---- update check | apply | status ----

func newUpdater(dir, channel string, probation func(context.Context, string) error, handover func(bool)) (*update.Updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	var key identity.Key
	if version.ReleaseKey != "" {
		if key, err = identity.ParseKey(version.ReleaseKey); err != nil {
			return nil, fmt.Errorf("embedded release key: %w", err)
		}
	}
	return update.New(update.Config{DataDir: dir, Version: version.Version, Embedded: key, IndexURL: version.ReleaseIndexURL,
		Channel: channel, Exe: exe, Probation: probation, Handover: handover}), nil
}

func runUpdate(args []string) error {
	if len(args) == 0 {
		return errors.New("update: a subcommand is needed: check, apply, status, retry VERSION")
	}
	fs := flag.NewFlagSet("update "+args[0], flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	channel := fs.String("channel", release.ChannelStable, "stable or early")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	// From the CLI the probation is the simplest one: the candidate must
	// run and report a version. The agent's own updater runs the real one.
	u, err := newUpdater(d, *channel, probeVersion, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	switch args[0] {
	case "status":
		b, _ := json.MarshalIndent(u.Status(), "", "  ")
		fmt.Println(string(b))
		return nil
	case "check":
		rel, err := u.Check(ctx)
		if err != nil {
			return err
		}
		if rel == nil {
			fmt.Printf("running %s, nothing newer on the %s channel\n", version.Version, *channel)
			return nil
		}
		fmt.Printf("running %s, available %s (%s)\n", version.Version, rel.Version, rel.Channel)
		return nil
	case "channel":
		if fs.NArg() != 1 {
			return errors.New("update channel needs stable or early")
		}
		if err := u.SetChannel(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("channel %s, chosen on this device; the running agent uses it at its next check\n", fs.Arg(0))
		return nil
	case "retry":
		if fs.NArg() != 1 {
			return errors.New("update retry needs the version that failed probation")
		}
		if err := u.Retry(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("%s may be installed again at the next check\n", fs.Arg(0))
		return nil
	case "apply":
		rel, err := u.Check(ctx)
		if err != nil {
			return err
		}
		if rel == nil {
			fmt.Println("nothing to apply")
			return nil
		}
		err = u.Apply(ctx, rel)
		if errors.Is(err, update.ErrRestart) {
			fmt.Printf("installed %s; restart the agent (or the service) to run it\n", rel.Version)
			return nil
		}
		return err
	}
	return fmt.Errorf("update: unknown subcommand %q", args[0])
}

// probeVersion is the CLI's probation: the candidate starts and names itself.
func probeVersion(ctx context.Context, exe string) error {
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(out), "redundanet ") {
		return fmt.Errorf("unexpected output %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// probeAgent is the agent's probation: the candidate runs as the agent with
// --probation and must report, within the deadline, that it reached the
// hubs and the peers the running agent had.
func probeAgent(ctx context.Context, exe string, args []string) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == probationOK {
			return nil
		}
	}
	if ctx.Err() != nil {
		return errors.New("the candidate did not report healthy in time")
	}
	return errors.New("the candidate exited before reporting healthy")
}

const probationOK = "PROBATION OK"
