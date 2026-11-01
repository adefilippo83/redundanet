package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
)

// The operator commands of the RFC: an offline key, `manifest sign` after a
// merge, `manifest verify` for anyone, and `validate` for CI and humans.
func runOperator(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("operator: the subcommand is init")
	}
	fs := flag.NewFlagSet("operator init", flag.ContinueOnError)
	path := fs.String("key", "operator.key", "where to write the key; keep it offline")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	k, err := manifest.NewOperatorKey()
	if err != nil {
		return err
	}
	if err := k.Save(*path); err != nil {
		return err
	}
	fmt.Printf("operator key written to %s\n", *path)
	fmt.Printf("public key, for the manifest's operators list: %s\n", k.Public())
	return nil
}

func runManifest(args []string) error {
	if len(args) == 0 {
		return errors.New("manifest: a subcommand is needed: sign, verify")
	}
	switch args[0] {
	case "sign":
		return manifestSign(args[1:])
	case "verify":
		return manifestVerify(args[1:])
	}
	return fmt.Errorf("manifest: unknown subcommand %q", args[0])
}

func manifestSign(args []string) error {
	fs := flag.NewFlagSet("manifest sign", flag.ContinueOnError)
	keyPath := fs.String("key", "operator.key", "operator key file")
	mPath := fs.String("manifest", "manifests/manifest.yaml", "manifest file")
	out := fs.String("out", "", "signature file (default: manifest.sig beside the manifest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := manifest.LoadOperatorKey(*keyPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*mPath)
	if err != nil {
		return err
	}
	sig, err := manifest.Sign(k, data, time.Now())
	if err != nil {
		return err
	}
	enc, err := sig.Encode()
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(filepath.Dir(*mPath), "manifest.sig")
	}
	if err := os.WriteFile(*out, enc, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s revision %d as operator %s; signature written to %s\n", sig.Network, sig.Revision, k.Public().Short(), *out)
	return nil
}

func manifestVerify(args []string) error {
	fs := flag.NewFlagSet("manifest verify", flag.ContinueOnError)
	mPath := fs.String("manifest", "manifests/manifest.yaml", "manifest file")
	sPath := fs.String("sig", "", "signature file (default: manifest.sig beside the manifest)")
	trust := fs.String("trust", "", "comma-separated operator keys to trust (default: the manifest's own operators list, which only proves self-consistency)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(*mPath)
	if err != nil {
		return err
	}
	if *sPath == "" {
		*sPath = filepath.Join(filepath.Dir(*mPath), "manifest.sig")
	}
	sigData, err := os.ReadFile(*sPath)
	if err != nil {
		return err
	}
	sig, err := manifest.ParseSignature(sigData)
	if err != nil {
		return err
	}
	var trusted []identity.Key
	if *trust != "" {
		for _, s := range strings.Split(*trust, ",") {
			k, err := identity.ParseKey(s)
			if err != nil {
				return err
			}
			trusted = append(trusted, k)
		}
	} else {
		m, err := manifest.Parse(data)
		if err != nil {
			return err
		}
		for _, o := range m.Operators {
			trusted = append(trusted, o.Key)
		}
		fmt.Fprintln(os.Stderr, "note: trusting the operators listed in the manifest itself; pass --trust to pin the keys you know")
	}
	m, err := sig.Verify(data, trusted)
	if err != nil {
		return err
	}
	fr := sig.Freshness(m, 0, time.Now())
	fmt.Printf("ok: %s revision %d, signed %s by operator %s, age %s", m.Network.Name, m.Revision, sig.IssuedAt.Format(time.RFC3339), sig.Operator[:8], fr.Age.Round(time.Hour))
	if fr.Expired {
		fmt.Printf(" (past signature_max_age_days=%d: nodes admit nobody new until it is re-signed)", m.Network.SignatureMaxAgeDays)
	}
	fmt.Println()
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	mPath := fs.String("manifest", "manifests/manifest.yaml", "manifest file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(*mPath)
	if err != nil {
		return err
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return err
	}
	ps := m.Validate()
	for _, p := range ps {
		fmt.Println(p)
	}
	if errs := ps.Errors(); len(errs) > 0 {
		return fmt.Errorf("%d blocking problems", len(errs))
	}
	fmt.Printf("ok: %s revision %d, %d-of-%d, %d operators, %d hubs, %d members, %d storage devices\n",
		m.Network.Name, m.Revision, m.Network.Encoding.K, m.Network.Encoding.N, len(m.Operators), len(m.Hubs), len(m.Members), len(m.Storage))
	for _, mem := range m.Members {
		fmt.Printf("  %-20s contributes %6d GB  allocation %6d GB  %s\n", mem.Name, mem.ContributionGB, m.Allocation(mem.Name), mem.Status)
	}
	return nil
}
