package main

import (
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
	_, network := commonFlags(fs)
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
	fmt.Fprintf(os.Stderr, "revocation of device %s signed by member %s; add the record below under `revocations:` in manifests/manifest.yaml, raise the revision by one and open a pull request. The operator signs and merges; every agent and server refuses the device from then on.\n", device.Short(), rv.Revocation.Member.Short())
	fmt.Printf("revocations:\n  - member: %s\n    device: %s\n    serial: %d\n    revoked_at: %s\n    signature: %s\n",
		rv.Revocation.Member, rv.Revocation.Device, rv.Revocation.Serial, rv.Revocation.RevokedAt.UTC().Format(time.RFC3339), base64.StdEncoding.EncodeToString(rv.Signature))
	return nil
}
