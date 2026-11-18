package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/node"
	"github.com/adefilippo83/redundanet/agent/internal/release"
	"github.com/adefilippo83/redundanet/agent/internal/reset"
	"github.com/adefilippo83/redundanet/agent/internal/service"
)

// `reset` and `node reset` take a device back to before it had an
// identity, in one step: the service stops, what belonged to the identity
// goes (internal/reset says what), the program, the service and the
// settings stay, and the device is ready to be set up again. An identity
// is never replaced in place, so this is how a member starts over after a
// phrase was disclosed or lost, or gives a device another role.
//
// Nothing here reaches the network. The member stays in the manifest until
// the maintainer removes them, and the device stays certified until its
// member revokes it or leaves; both commands say so when they are done.

// The service, as the commands use it; tests put their own in.
var (
	serviceQuery     = service.Query
	serviceUninstall = service.Uninstall
	serviceInstall   = service.Install
)

// resetPlan is what a reset found on the device before it asks.
type resetPlan struct {
	Dir      string
	Identity string // one line on the identity that goes, "" for none
	Device   identity.Key
	Listed   string // the name under storage: in the manifest copy, "" when not listed
	Store    string // the share store, "" when the device has none
}

// planReset reads the device without changing it. An identity sealed with
// a passphrase is not opened: the reset removes it either way.
func planReset(dir, network, storagePath string) resetPlan {
	p := resetPlan{Dir: dir}
	path := identityPath(dir)
	if _, err := os.Stat(path); err == nil {
		if st, err := identity.Load(path, network, nil); err == nil {
			p.Device = st.Certificate.Certificate.Device
			p.Identity = fmt.Sprintf("member %s (%s), device %s, roles %s", st.Member.Short(), identity.DisplayCode(st.Member), p.Device.Short(),
				strings.Join(st.Certificate.Certificate.Roles, ", "))
		} else {
			p.Identity = "an identity this command cannot read without its passphrase"
		}
	}
	if !p.Device.IsZero() {
		if b, err := os.ReadFile(filepath.Join(dir, "manifest.yaml")); err == nil {
			if m, err := manifest.Parse(b); err == nil {
				for _, s := range m.Storage {
					if s.Device == p.Device {
						p.Listed = s.Name
					}
				}
			}
		}
	}
	store := filepath.Join(dir, reset.StoreDir)
	if storagePath != "" {
		store = filepath.Join(storagePath, reset.StoreDir)
	}
	if entries, err := os.ReadDir(store); err == nil && len(entries) > 0 {
		p.Store = store
	}
	return p
}

// refuseStoreWipe is why the store may not be emptied: the manifest still
// lists this device as a storage server, so other members' shares on it
// are counted and nothing repairs them elsewhere until the list changes.
func (p resetPlan) refuseStoreWipe() error {
	if p.Listed == "" {
		return nil
	}
	return fmt.Errorf("--wipe-store refused: the manifest lists this device under storage: as %q, so the shares on it are other members' and still counted. Ask the maintainer to take the node out of the manifest first, or reset without --wipe-store, which keeps the store", p.Listed)
}

func (p resetPlan) print(wipeStore bool) {
	fmt.Println("This removes from", p.Dir+":")
	if p.Identity != "" {
		fmt.Println("  the identity:", p.Identity)
	} else {
		fmt.Println("  no identity is there; whatever state is left")
	}
	fmt.Println("  the backup settings, the local database, the manifest copy, the list of paired devices")
	fmt.Println("It keeps the program, the service and the update channel.")
	switch {
	case p.Store == "":
	case wipeStore:
		fmt.Println("It EMPTIES the share store", p.Store+", with every share on it.")
	default:
		fmt.Println("It keeps the share store", p.Store, "(--wipe-store empties it).")
	}
	fmt.Println("It changes nothing on the network: backups already stored stay there, and so do the member and this device's certificate.")
}

// confirmReset asks for the word unless yes is set.
func confirmReset(yes bool) error {
	if yes {
		return nil
	}
	if ask("Type reset to go on: ") != "reset" {
		return errors.New("nothing was changed")
	}
	return nil
}

// recordedChannel is the release channel the updater last recorded for
// this device (one chosen on the device first), "" when it never ran. The
// record stays through a reset, and the service is put back on it.
func recordedChannel(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "updates", "state.yaml"))
	if err != nil {
		return ""
	}
	var st struct {
		Channel string `yaml:"channel"`
		Chosen  string `yaml:"chosen_channel"`
	}
	if yaml.Unmarshal(b, &st) != nil {
		return ""
	}
	for _, c := range []string{st.Chosen, st.Channel} {
		if c == release.ChannelStable || c == release.ChannelEarly {
			return c
		}
	}
	return ""
}

func afterReset(p resetPlan) {
	if p.Identity == "" {
		return
	}
	fmt.Println()
	fmt.Println("On the network nothing changed. With a new phrase you are a new member: tell the maintainer, who removes the old entry from the manifest before your new join request arrives. With the same phrase, the old key of this device stays certified until you revoke it from a device of yours:")
	if !p.Device.IsZero() {
		fmt.Printf("  redundanet device revoke --device %s\n", p.Device)
	} else {
		fmt.Println("  redundanet device revoke --device KEY")
	}
}

func runReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	channel := fs.String("channel", "", "the release channel the service is put back on: stable or early (default: the one the updater recorded, or stable)")
	wipeStore := fs.Bool("wipe-store", false, "also empty this device's share store; refused while the manifest lists the device under storage:")
	yes := fs.Bool("yes", false, "do not ask")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network == "" {
		return errors.New("--network is required")
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" && *dir == "" {
		if _, err := os.Stat(identityPath(d)); err != nil && node.Inspect(nodeConfig()).Installed {
			return errors.New("this machine is a storage node: sudo redundanet3 node reset")
		}
	}
	plan := planReset(d, *network, "")
	if *wipeStore {
		if err := plan.refuseStoreWipe(); err != nil {
			return err
		}
	}
	plan.print(*wipeStore)
	if err := confirmReset(*yes); err != nil {
		return err
	}
	ch := *channel
	if ch == "" {
		ch = recordedChannel(d)
	}
	if ch == "" {
		ch = release.ChannelStable
	}
	// The service is this user's one agent, on the default data directory:
	// a reset of another directory leaves it alone.
	var sv service.Status
	if def, err := dataDir(""); err == nil && filepath.Clean(def) == filepath.Clean(d) {
		sv = serviceQuery("")
	} else {
		fmt.Println("the service is left as it is: it serves the default data directory, not", d)
	}
	if sv.Installed {
		if err := serviceUninstall(""); err != nil {
			return fmt.Errorf("stopping the service: %w", err)
		}
	}
	removed, err := reset.Clear(d, *wipeStore)
	if err != nil {
		return fmt.Errorf("after %d entries removed: %w; is an agent still running from a terminal? Stop it and run the reset again", len(removed), err)
	}
	fmt.Printf("\nreset: %d entries removed from %s\n", len(removed), d)
	if sv.Installed {
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		var spec service.Spec
		if err == nil {
			spec, err = agentSpec(exe, d, *network, ch)
		}
		if err == nil {
			_, err = serviceInstall(spec)
		}
		if err != nil {
			fmt.Printf("the service could not be put back (%v): redundanet service install\n", err)
		} else {
			fmt.Println("the service runs again, without an identity: its page has the setup")
		}
	}
	fmt.Println("next: `redundanet open` for the page, or `redundanet setup` in this terminal")
	afterReset(plan)
	return nil
}

func nodeReset(args []string) error {
	fs := flag.NewFlagSet("node reset", flag.ContinueOnError)
	wipeStore := fs.Bool("wipe-store", false, "also empty the share store; refused while the manifest lists this node under storage:")
	noPair := fs.Bool("no-pair", false, "reset and stop; pair later with `node install`, or on the Raspberry Pi image by a reboot")
	yes := fs.Bool("yes", false, "do not ask")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := needRoot(); err != nil {
		return err
	}
	cfg := nodeConfig()
	st := node.Inspect(cfg)
	if !st.Installed {
		return errors.New("node reset: this machine is not laid out as a storage node (sudo redundanet3 node status); on a laptop the command is `redundanet reset`")
	}
	network := st.Env["NETWORK"]
	if network == "" {
		network = defaultNetwork()
	}
	plan := planReset(node.DataDir, network, st.Env["STORAGE_PATH"])
	if *wipeStore {
		if err := plan.refuseStoreWipe(); err != nil {
			return err
		}
	}
	plan.print(*wipeStore)
	if err := confirmReset(*yes); err != nil {
		return err
	}
	if _, err := cfg.Run("systemctl", "stop", node.UnitName); err != nil {
		return fmt.Errorf("stopping the service: %w", err)
	}
	removed, err := reset.Clear(node.DataDir, *wipeStore)
	if err != nil {
		return fmt.Errorf("after %d entries removed: %w", len(removed), err)
	}
	fmt.Printf("\nreset: %d entries removed from %s\n", len(removed), node.DataDir)
	if *wipeStore && st.Env["STORAGE_PATH"] != "" {
		store := filepath.Join(st.Env["STORAGE_PATH"], reset.StoreDir)
		n, err := reset.Empty(store)
		if err != nil {
			return fmt.Errorf("emptying the store %s: %w", store, err)
		}
		fmt.Printf("store emptied: %d entries removed from %s\n", n, store)
	}
	afterReset(plan)
	if *noPair {
		fmt.Println("\nthe node has no identity and its service is stopped; next: sudo redundanet3 node install --storage-path", st.Env["STORAGE_PATH"], "for a pairing code (on the Raspberry Pi image a reboot shows one too)")
		return nil
	}
	return pairNode(network, filepath.Join(node.DataDir, "bin", "redundanet"))
}
