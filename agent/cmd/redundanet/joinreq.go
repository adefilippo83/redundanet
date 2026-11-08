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

// joinPrefill is what the form can be handed.
type joinPrefill struct {
	MemberKey      string
	ContributionGB *int // nil leaves the field to the member
	DeviceKey      string
	DeviceName     string
}

func joinRequestURL(p joinPrefill) string {
	q := url.Values{}
	q.Set("key", p.MemberKey)
	if p.ContributionGB != nil {
		q.Set("gb", strconv.Itoa(*p.ContributionGB))
	}
	if p.DeviceKey != "" {
		q.Set("device", p.DeviceKey)
	}
	if p.DeviceName != "" {
		q.Set("devicename", p.DeviceName)
	}
	return joinFormURL + "?" + q.Encode()
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
	m, err := membership.Open(membershipConfig(d, *network))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		m.Refresh(ctx)
		cancel()
		if memberListed(m.Manifest(), st.Member) {
			fmt.Printf("member %s is already listed in manifest revision %d; nothing to request\n", st.Member.Short(), m.Status().Revision)
			return nil
		}
	}
	devices, _ := loadDevices(d)
	link := joinRequestURL(prefillFor(st, devices))
	fmt.Println(link)
	fmt.Fprintln(os.Stderr, "the form is filled in from this device; review it, fill the member name and the contribution, then open the issue on GitHub")
	if !*noBrowser {
		openInBrowser(link)
	}
	return nil
}
