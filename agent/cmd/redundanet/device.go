package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// `device` is what a member does about their own devices. Revocation
// travels in the signed manifest (RFC 0001, "Losing a device", the
// distribution the hub and the peers add later): the member signs the
// record here, the operator appends it and signs, every agent and server
// applies it with the revision.

func runDevice(args []string) error {
	if len(args) == 0 {
		return errors.New("device: a subcommand is needed: revoke")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "revoke":
		return deviceRevoke(rest)
	}
	return fmt.Errorf("device: unknown subcommand %q", sub)
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
