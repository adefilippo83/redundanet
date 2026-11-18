package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
)

// joinFormURL is the join form on the website. It reads the query
// parameters below and fills its fields with them, so the member reviews
// and clicks instead of copying keys.
const joinFormURL = "https://redundanet.com/join.html"

// joinPrefill is what the form can be handed: a new member's request, or,
// with Device set, a listed member's request to add a storage node, which
// carries the node's certificate as the proof the join workflow verifies.
type joinPrefill struct {
	Device         bool   // the form's "add a storage node" mode
	MemberName     string // known once the manifest lists the member
	MemberKey      string
	ContributionGB *int // nil leaves the field to the member
	DeviceKey      string
	DeviceName     string
	Certificate    string
}

func joinRequestURL(p joinPrefill) string {
	q := url.Values{}
	if p.Device {
		q.Set("mode", "device")
	}
	if p.MemberName != "" {
		q.Set("name", p.MemberName)
	}
	if p.MemberKey != "" {
		q.Set("key", p.MemberKey)
	}
	if p.ContributionGB != nil {
		q.Set("gb", strconv.Itoa(*p.ContributionGB))
	}
	if p.DeviceKey != "" {
		q.Set("device", p.DeviceKey)
	}
	if p.DeviceName != "" {
		q.Set("devicename", p.DeviceName)
	}
	if p.Certificate != "" {
		q.Set("cert", p.Certificate)
	}
	return joinFormURL + "?" + q.Encode()
}

// devicePrefill is a listed member's request to add a storage node: the
// first storage device on record that the manifest does not list, with its
// certificate when the record has one (a node paired before 3.0.8 has none;
// `identity show --certificate` on the node prints it). Nil when every
// recorded storage device is listed.
func devicePrefill(m *manifest.Manifest, member identity.Key, devices []pairedDevice) *joinPrefill {
	listed := map[string]bool{}
	for _, s := range m.Storage {
		listed[s.Device.String()] = true
	}
	sd := storageDeviceNotIn(devices, listed)
	if sd == nil {
		return nil
	}
	p := &joinPrefill{Device: true, MemberKey: member.String(), DeviceKey: sd.Device, DeviceName: sd.Name, Certificate: sd.Certificate}
	for _, mem := range m.Members {
		if mem.Key == member {
			p.MemberName = mem.Name
		}
	}
	return p
}

// prefillFor builds the join request for an identity: its member key, the
// first storage device on record, and a contribution of 0 when there is
// none, since the form requires a number. The devices are the nodes this
// device paired and, when its member said so, this device (withSelf);
// never this device for its storage role alone.
func prefillFor(st *identity.State, devices []pairedDevice) joinPrefill {
	p := joinPrefill{MemberKey: st.Member.String()}
	if sd := storageDevice(devices); sd != nil {
		p.DeviceKey, p.DeviceName = sd.Device, sd.Name
	}
	if p.DeviceKey == "" {
		zero := 0
		p.ContributionGB = &zero
	}
	return p
}

// storageNote says when this device's storage role and the manifest
// disagree, for a listed member: "unlisted" when the device has the role
// and the manifest does not list it, so it stores nothing for the network,
// and its member has not said it is their server (a server waiting for its
// request has the page's request button instead); "norole" when the
// manifest lists it and its certificate has no storage role, so it opens
// no store. "" when they agree.
func storageNote(m *manifest.Manifest, device identity.Key, roles []string, dd string) string {
	listed := false
	for _, s := range m.Storage {
		if s.Device == device {
			listed = true
		}
	}
	has := slices.Contains(roles, identity.RoleStorage)
	switch {
	case listed && !has:
		return "norole"
	case has && !listed:
		if yes, _ := serverChoice(dd); !yes {
			return "unlisted"
		}
	}
	return ""
}

// memberListed reports whether the manifest lists the member: the device is
// admitted once it is, and `setup` and `status` say which.
func memberListed(m *manifest.Manifest, member identity.Key) bool {
	if m == nil {
		return false
	}
	for _, mem := range m.Members {
		if mem.Key == member {
			return true
		}
	}
	return false
}

// openInBrowser starts the default browser on a link; a headless machine
// without one is no error, the link is printed anyway.
func openInBrowser(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	_ = cmd.Start()
}

// runJoinRequest opens the join form filled in from this device's identity
// and the devices it paired. Nothing is sent: the member reviews the form
// and opens the GitHub issue from it.
func runJoinRequest(args []string) error {
	fs := flag.NewFlagSet("join-request", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	noBrowser := fs.Bool("print", false, "only print the link")
	thisDevice := fs.Bool("this-device", false, "this computer is my storage server, a machine that stays on all day: name it in the request (it needs the storage role)")
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
	st, err := loadState(identityPath(d), *network)
	if err != nil {
		return fmt.Errorf("this device has no identity yet (%w); run redundanet setup", err)
	}
	// Whether this computer is the member's storage server is theirs to
	// say, once: the role alone does not put it in a request.
	if hasRole := slices.Contains(st.Certificate.Certificate.Roles, identity.RoleStorage); *thisDevice {
		if !hasRole {
			return errors.New("--this-device: this computer has no storage role, and a role is not added to an identity: `redundanet reset`, then set it up as a storage server")
		}
		if err := recordServerChoice(d, true); err != nil {
			return err
		}
	} else if _, known := serverChoice(d); hasRole && !known {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Println("This computer has the storage role. A storage server is a machine that stays on all day and keeps other members' shares on its disk.")
			yes := strings.EqualFold(ask("Is this computer your storage server? [y/N] "), "y")
			if err := recordServerChoice(d, yes); err != nil {
				return err
			}
		} else {
			fmt.Fprintln(os.Stderr, "this computer has the storage role and the request does not name it; if it is your storage server: redundanet join-request --this-device")
		}
	}
	devices, _ := loadDevices(d)
	devices = withSelf(d, st, devices)
	m, err := membership.Open(membershipConfig(d, *network))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		m.Refresh(ctx)
		cancel()
		if man := m.Manifest(); memberListed(man, st.Member) {
			// A listed member has nothing to request but a storage node the
			// manifest does not list yet.
			p := devicePrefill(man, st.Member, devices)
			if p == nil {
				fmt.Printf("member %s is listed in manifest revision %d and so is every storage node paired from here; nothing to request\n", st.Member.Short(), m.Status().Revision)
				return nil
			}
			link := joinRequestURL(*p)
			fmt.Println(link)
			if p.Certificate == "" {
				fmt.Fprintf(os.Stderr, "the form adds storage node %s to member %s; it needs the node's certificate as proof, which `sudo redundanet3 identity show --certificate` prints on the node\n", p.DeviceName, p.MemberName)
			} else {
				fmt.Fprintf(os.Stderr, "the form adds storage node %s to member %s, with the node's certificate as proof; review it, then open the issue on GitHub\n", p.DeviceName, p.MemberName)
			}
			if !*noBrowser {
				openInBrowser(link)
			}
			return nil
		}
	}
	link := joinRequestURL(prefillFor(st, devices))
	fmt.Println(link)
	fmt.Fprintln(os.Stderr, "the form is filled in from this device; review it, fill the member name and the contribution, then open the issue on GitHub")
	if !*noBrowser {
		openInBrowser(link)
	}
	return nil
}
