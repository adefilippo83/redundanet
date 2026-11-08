package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/release"
	"github.com/adefilippo83/redundanet/agent/internal/service"
)

// runSetup walks a laptop through everything after the install, in order,
// and can be run again at any time: each step reports what is already done
// and does what is missing. Binary, identity, network, service, admission,
// join request, page. A storage node is laid out by `node install` instead.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	channel := fs.String("channel", release.ChannelStable, "release channel for self-update: stable or early")
	storage := fs.Bool("storage", false, "this device will also contribute disk")
	noBrowser := fs.Bool("print", false, "print the links instead of opening the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network == "" {
		return errors.New("--network is required")
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return errors.New("setup is for the device you sit at, as yourself; a storage node is laid out with `sudo redundanet node install --storage-path DIR`")
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	step := func(name, format string, a ...any) { fmt.Printf("%-9s %s\n", name, fmt.Sprintf(format, a...)) }

	// 1. The binary: the agent updates itself by renaming a new version
	// into the executable's directory, so that directory must be writable.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	switch {
	case strings.HasPrefix(exe, filepath.Join(d, "bin")+string(filepath.Separator)):
		step("binary", "%s, where the agent can update it", exe)
	case dirWritable(filepath.Dir(exe)):
		step("binary", "%s; the agent can update it there, but the usual place is bin/ in the data directory (the one-line installer puts it there)", exe)
	default:
		step("binary", "warning: %s is in a directory you cannot write, so the agent cannot update itself; run the one-line installer: curl -fsSL https://redundanet.com/install.sh | sh", exe)
	}

	// 2. The identity.
	path := identityPath(d)
	if _, err := os.Stat(path); err != nil {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("this device has no identity and setup needs a terminal to make one: run it in a terminal, or `identity init`, `identity init --from-phrase` or `identity pair` by hand")
		}
		fmt.Println("identity  none yet. Which is it?")
		fmt.Println("            1  I am new here: create my identity (a twelve-word phrase, shown once)")
		fmt.Println("            2  I have my recovery phrase: put my identity on this device")
		fmt.Println("            3  another device of mine is nearby: pair with it (the phrase stays there)")
		choice := ask("          > ")
		common := []string{"--data-dir", d, "--network", *network}
		if *storage {
			common = append(common, "--storage")
		}
		fmt.Println()
		switch choice {
		case "1":
			err = identityInit(common)
		case "2":
			err = identityInit(append(common, "--from-phrase"))
		case "3":
			err = identityPair([]string{"--data-dir", d, "--network", *network})
		default:
			return errors.New("setup stopped: answer 1, 2 or 3")
		}
		if err != nil {
			return err
		}
		fmt.Println()
	}
	st, err := loadState(path, *network)
	if err != nil {
		return err
	}
	step("identity", "member %s (%s), device %s, roles %s", st.Member.Short(), identity.DisplayCode(st.Member), st.Certificate.Certificate.Device.Short(), strings.Join(st.Certificate.Certificate.Roles, ", "))

	// 3. The network: built in, or joined by hand; fetch the manifest once.
	listed := false
	m, err := membership.Open(membershipConfig(d, *network))
	if err != nil {
		step("network", "%v: run `redundanet network join <descriptor>`", err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		m.Refresh(ctx)
		cancel()
		ms := m.Status()
		if ms.LastError != "" && m.Manifest() == nil {
			step("network", "%s: the manifest could not be fetched yet: %s", ms.Network, ms.LastError)
		} else {
			step("network", "%s, manifest revision %d, %d members, %d hubs", ms.Network, ms.Revision, ms.Members, ms.Hubs)
			listed = memberListed(m.Manifest(), st.Member)
			if listed {
				step("member", "listed in the manifest: this device is admitted")
			} else {
				step("member", "not in the manifest yet: the maintainer admits you by signing your join request (below)")
			}
		}
	}

	// 4. The service.
	sv := serviceStatus()
	if !sv.Installed {
		spec, err := agentSpec(exe, d, *network, *channel)
		if err != nil {
			return err
		}
		if _, err := service.Install(spec); err != nil {
			return fmt.Errorf("installing the service: %w", err)
		}
		sv = serviceStatus()
	}
	if sv.Installed {
		if !sv.Running {
			sv = waitRunning(15 * time.Second)
		}
		if sv.Running {
			step("service", "installed and running, starts at login")
		} else {
			step("service", "installed, not running yet: `redundanet service status` and `redundanet logs` say more")
		}
	}

	// 5. The join request, while the member is not listed; for a listed
	// member, a paired storage node the manifest does not list yet.
	devices, _ := loadDevices(d)
	if !listed {
		link := joinRequestURL(prefillFor(st, devices))
		step("request", "the join form, filled in from this device:\n          %s\n          fill the member name and the contribution, then open the issue on GitHub; `redundanet status` says when you are listed", link)
		if !*noBrowser {
			openInBrowser(link)
		}
	} else if m != nil {
		if p := devicePrefill(m.Manifest(), st.Member, devices); p != nil {
			link := joinRequestURL(*p)
			proof := "with the node's certificate as proof"
			if p.Certificate == "" {
				proof = "it needs the node's certificate, which `sudo /var/lib/redundanet3/bin/redundanet identity show --certificate` prints on the node"
			}
			step("request", "storage node %s is paired but not in the manifest yet; the form that adds it, %s:\n          %s", p.DeviceName, proof, link)
			if !*noBrowser {
				openInBrowser(link)
			}
		}
	}

	// 6. The page.
	if c, err := localClient(d); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		link, err := c.LoginLink(ctx)
		cancel()
		if err == nil {
			step("page", "%s (the link works once, for two minutes)", link)
			if !*noBrowser {
				openInBrowser(link)
			}
		}
	}
	if listed {
		fmt.Println("\nnext: redundanet backup add NAME FOLDER   # your first backup set; redundanet open shows the page again")
	} else {
		fmt.Println("\nnext: send the join request; run redundanet setup again any time to see where things stand")
	}
	return nil
}

// ask reads one line from the terminal.
func ask(prompt string) string {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

// dirWritable reports whether this user can create files in dir.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".redundanet-write-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// waitRunning polls the service status until it runs or the time is up.
func waitRunning(timeout time.Duration) service.Status {
	deadline := time.Now().Add(timeout)
	for {
		sv := serviceStatus()
		if sv.Running || time.Now().After(deadline) {
			return sv
		}
		time.Sleep(500 * time.Millisecond)
	}
}
