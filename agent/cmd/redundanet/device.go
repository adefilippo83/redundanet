package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/pairing"
)

// `device` is what a member does about their own devices. Revocation
// travels in the signed manifest (RFC 0001, "Losing a device", the
// distribution the hub and the peers add later): the member signs the
// record here, the operator appends it and signs, every agent and server
// applies it with the revision.

func runDevice(args []string) error {
	if len(args) == 0 {
		return errors.New("device: a subcommand is needed: pair, revoke")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "revoke":
		return deviceRevoke(rest)
	case "pair":
		return devicePair(rest)
	}
	return fmt.Errorf("device: unknown subcommand %q", sub)
}

// devicePair is the existing device's side of pairing: with the phrase typed
// here, it reaches the new device through its code, certifies its key and
// hands over the identity and this network's descriptor. The code is asked
// for, like the phrase: on the command line every user of this machine
// could read it while the command runs, and whoever holds a code can
// answer the new device in the member's place.
func devicePair(args []string) error {
	fs := flag.NewFlagSet("device pair", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	storage := fs.Bool("storage", false, "grant the new device the storage role")
	serial := fs.Uint64("serial", 0, "certificate serial for this member (default: seconds since 2026-01-01)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network == "" || fs.NArg() > 1 {
		return errors.New("usage: device pair [--network NAME] [--storage]; it asks for the code the new device shows, then for the phrase")
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	code := fs.Arg(0)
	if code != "" {
		fmt.Fprintln(os.Stderr, "note: a code on the command line can be read by other users of this machine while this runs; leave it out and paste it when asked")
	} else if code, err = readLine("Pairing code shown by the new device: "); err != nil {
		return err
	}
	parsed, err := pairing.Parse(code)
	if err != nil {
		return err
	}
	if !parsed.Bound() {
		fmt.Fprintln(os.Stderr, "note: this code comes from a version before 3.0.15 and does not name the new device's key; make sure nobody else has seen it")
	}
	seed, err := readPhrase()
	if err != nil {
		return err
	}
	defer seed.Zero()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pd, err := pairDevice(ctx, d, *network, code, seed, *storage, *serial)
	if err != nil {
		return err
	}
	fmt.Printf("paired device %s for member %s (%s); it holds its identity and the network now, and can install its service\n", short(pd.Device), seed.MemberKey().Short(), strings.Join(pd.Roles, ", "))
	if *storage {
		fmt.Printf("storage node %s is on record here: redundanet join-request puts it in the join form, or send its storage: lines to the maintainer\n", pd.Name)
	}
	return nil
}

func short(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

// pairDevice is the existing device's side of a pairing, for the command
// and for the page: it reaches the new device through the code, certifies
// it from the seed with the client role, and the storage role when asked,
// hands it the identity and the network, and puts it on record in the data
// directory dd with its certificate, so the join request can name a
// storage node and prove it is the member's without anything copied by
// hand. A serial of 0 is the seconds since 2026-01-01.
func pairDevice(ctx context.Context, dd, network, code string, seed identity.Seed, storage bool, serial uint64) (pairedDevice, error) {
	// The descriptor handed over: the one this device joined with, else the
	// network built into this binary when that is the one.
	descriptor := ""
	desc, err := membership.LoadDescriptor(filepath.Join(dd, membership.DescriptorFile))
	if err != nil || desc.Network != network {
		desc, err = builtinDescriptor()
		if err != nil || desc.Network != network {
			desc = nil
		}
	}
	if desc != nil {
		if descriptor, err = desc.Encode(); err != nil {
			descriptor = ""
		}
	}
	if descriptor == "" {
		fmt.Fprintln(os.Stderr, "this device has no descriptor for that network; the new device will need `network join <descriptor>` by hand")
	}
	if serial == 0 {
		serial = uint64(time.Since(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Seconds())
	}
	roles := []string{identity.RoleClient}
	if storage {
		roles = append(roles, identity.RoleStorage)
	}
	var issued *identity.SignedCertificate
	dev, name, err := pairing.Pair(ctx, code, network, func(device identity.Key, _ string) (*pairing.Offer, error) {
		sc, err := identity.Issue(seed, identity.Certificate{Network: network, Device: device, Roles: roles, IssuedAt: time.Now(), Serial: serial})
		if err != nil {
			return nil, err
		}
		issued = sc
		st := identity.State{Network: network, Member: sc.Certificate.Member, MemberSecret: seed.Secret(), Certificate: *sc}
		b, err := identity.Marshal(&st)
		if err != nil {
			return nil, err
		}
		return &pairing.Offer{State: b, Descriptor: descriptor}, nil
	})
	if err != nil {
		return pairedDevice{}, err
	}
	cert, _ := encodeCertificate(issued)
	pd := pairedDevice{Device: dev.String(), Name: name, Roles: roles, Certificate: cert, PairedAt: time.Now()}
	if err := recordDevice(dd, pd); err != nil {
		fmt.Fprintln(os.Stderr, "could not record the device:", err)
	}
	return pd, nil
}

func deviceRevoke(args []string) error {
	fs := flag.NewFlagSet("device revoke", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	deviceStr := fs.String("device", "", "the device key to revoke, as `status` and the status page show it")
	serial := fs.Uint64("serial", 0, "the certificate serial to revoke; 0 revokes every certificate of the device, the case of a lost one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network == "" || *deviceStr == "" {
		return errors.New("--network and --device are required")
	}
	device, err := identity.ParseKey(*deviceStr)
	if err != nil {
		return err
	}
	seed, err := readPhrase()
	if err != nil {
		return err
	}
	defer seed.Zero()
	rv, err := identity.Revoke(seed, identity.Revocation{Network: *network, Device: device, Serial: *serial})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "revocation of device %s signed by member %s\n", device.Short(), rv.Revocation.Member.Short())
	// The direct path first: the running agent applies the record here and
	// hands it to every storage server and hub, seconds instead of the next
	// signed manifest. Without an agent the record still goes by hand.
	if c, err := localClient(*dir); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if rep, err := c.Revoke(ctx, rv); err != nil {
			fmt.Fprintf(os.Stderr, "the running agent did not send it: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "applied on this device and sent to %d of %d storage servers and %d of %d hubs; servers that were offline learn it from a hub\n", rep.Servers, rep.ServersTotal, rep.Hubs, rep.HubsTotal)
		}
	} else {
		fmt.Fprintln(os.Stderr, "no running agent here, so nothing was sent; the manifest carries it")
	}
	fmt.Fprintln(os.Stderr, "for the record, add this under `revocations:` in manifests/manifest.yaml, raise the revision by one and open a pull request; the operator signs and merges.")
	fmt.Printf("revocations:\n  - member: %s\n    device: %s\n    serial: %d\n    revoked_at: %s\n    signature: %s\n",
		rv.Revocation.Member, rv.Revocation.Device, rv.Revocation.Serial, rv.Revocation.RevokedAt.UTC().Format(time.RFC3339), base64.StdEncoding.EncodeToString(rv.Signature))
	return nil
}
