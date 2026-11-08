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
	"time"

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
// first storage device on record (or this device itself when it has the
// storage role), and a contribution of 0 when there is no storage device,
// since the form requires a number.
func prefillFor(st *identity.State, devices []pairedDevice) joinPrefill {
	p := joinPrefill{MemberKey: st.Member.String()}
	if sd := storageDevice(devices); sd != nil {
		p.DeviceKey, p.DeviceName = sd.Device, sd.Name
	} else if slices.Contains(st.Certificate.Certificate.Roles, identity.RoleStorage) {
		if dev, err := st.Device(); err == nil {
			p.DeviceKey = dev.ID().String()
			p.DeviceName, _ = os.Hostname()
		}
	}
	if p.DeviceKey == "" {
		zero := 0
		p.ContributionGB = &zero
	}
	return p
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
	devices, _ := loadDevices(d)
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
				fmt.Fprintf(os.Stderr, "the form adds storage node %s to member %s; it needs the node's certificate as proof, which `sudo /var/lib/redundanet3/bin/redundanet identity show --certificate` prints on the node\n", p.DeviceName, p.MemberName)
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
