package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// The identity commands cover the RFC's first-run and headless flows:
//
//	identity init          this device joins as a new member: phrase, keys, certificate
//	identity show          what this device is
//	identity device-key    a headless node makes its device key and prints it
//	identity certify       on a laptop with the phrase: certify a device key, print the certificate
//	identity accept-certificate   the headless node installs the certificate
//
// The phrase never reaches a headless node: device-key runs there, certify
// runs where the member types the phrase, and the certificate travels back
// as a file.
func runIdentity(args []string) error {
	if len(args) == 0 {
		return errors.New("identity: a subcommand is needed: init, show, device-key, certify, accept-certificate")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return identityInit(rest)
	case "show":
		return identityShow(rest)
	case "device-key":
		return identityDeviceKey(rest)
	case "certify":
		return identityCertify(rest)
	case "accept-certificate":
		return identityAcceptCertificate(rest)
	}
	return fmt.Errorf("identity: unknown subcommand %q", sub)
}

// dataDir is the platform's per-user directory for the agent's own files.
func dataDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if d := os.Getenv("REDUNDANET_DATA_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "redundanet"), nil
}

func identityPath(dir string) string { return filepath.Join(dir, "identity") }

func commonFlags(fs *flag.FlagSet) (dir *string, network *string) {
	dir = fs.String("data-dir", "", "data directory (default: the platform's user config directory, or $REDUNDANET_DATA_DIR)")
	network = fs.String("network", "", "network name, as in the manifest")
	return
}

func identityInit(args []string) error {
	fs := flag.NewFlagSet("identity init", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	storage := fs.Bool("storage", false, "this device will also contribute disk")
	yes := fs.Bool("yes", false, "skip typing three words back (scripts only)")
	passphrase := fs.Bool("passphrase", false, "seal the identity file with a passphrase")
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
	path := identityPath(d)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists: this device already has an identity", path)
	}

	seed, err := identity.NewSeed()
	if err != nil {
		return err
	}
	defer seed.Zero()
	words := seed.Phrase()
	fmt.Println("Your recovery phrase. Write it down and keep it where you keep your passport;")
	fmt.Println("it is the only thing that restores your data on another machine, and nobody")
	fmt.Println("can recover it for you.")
	fmt.Println()
	printPhrase(words)
	fmt.Println()
	if !*yes {
		if err := confirmPhrase(words); err != nil {
			return err
		}
	}

	dev, err := identity.NewDeviceKey()
	if err != nil {
		return err
	}
	roles := []string{identity.RoleClient}
	if *storage {
		roles = append(roles, identity.RoleStorage)
	}
	sc, err := identity.Issue(seed, identity.Certificate{Network: *network, Device: dev.ID(), Roles: roles, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		return err
	}
	var pass []byte
	if *passphrase {
		if pass, err = readNewPassphrase(); err != nil {
			return err
		}
	}
	st := &identity.State{Network: *network, Member: seed.MemberKey(), MemberSecret: seed.Secret(), DeviceSeed: dev.Seed(), Certificate: *sc}
	if err := identity.Save(path, st, pass); err != nil {
		return err
	}
	fmt.Printf("Identity written to %s\n", path)
	printState(st)
	return nil
}

func identityShow(args []string) error {
	fs := flag.NewFlagSet("identity show", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	st, err := loadState(identityPath(d), *network)
	if err != nil {
		return err
	}
	printState(st)
	return nil
}

func identityDeviceKey(args []string) error {
	fs := flag.NewFlagSet("identity device-key", flag.ContinueOnError)
	dir, network := commonFlags(fs)
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
	path := filepath.Join(d, "device-key.pending")
	if _, err := os.Stat(identityPath(d)); err == nil {
		return fmt.Errorf("%s exists: this device already has an identity", identityPath(d))
	}
	var dev *identity.DeviceKey
	if b, err := os.ReadFile(path); err == nil {
		if dev, err = identity.DeviceKeyFromSeed(b); err != nil {
			return err
		}
	} else {
		if dev, err = identity.NewDeviceKey(); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, dev.Seed(), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("device key: %s\n", dev.ID())
	fmt.Printf("network:    %s\n", *network)
	fmt.Println()
	fmt.Println("On a machine where you can type your phrase, run:")
	fmt.Printf("  redundanet identity certify --network %s --device %s --storage > certificate\n", *network, dev.ID())
	fmt.Println("then copy the certificate file here and run:")
	fmt.Printf("  redundanet identity accept-certificate --network %s certificate\n", *network)
	return nil
}

func identityCertify(args []string) error {
	fs := flag.NewFlagSet("identity certify", flag.ContinueOnError)
	_, network := commonFlags(fs)
	deviceStr := fs.String("device", "", "the device key to certify, as printed by device-key")
	storage := fs.Bool("storage", false, "grant the storage role")
	serial := fs.Uint64("serial", 0, "certificate serial for this member (default: seconds since 2026-01-01)")
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
	if *serial == 0 {
		*serial = uint64(time.Since(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Seconds())
	}
	roles := []string{identity.RoleClient}
	if *storage {
		roles = append(roles, identity.RoleStorage)
	}
	sc, err := identity.Issue(seed, identity.Certificate{Network: *network, Device: device, Roles: roles, IssuedAt: time.Now(), Serial: *serial})
	if err != nil {
		return err
	}
	// What the device lacks travels as one file: the certificate and the
	// member secret, which every device of the member holds. The device seed
	// stays where it was made. The file is a secret until the device has
	// accepted it, and the message says so.
	st := identity.State{Network: *network, Member: sc.Certificate.Member, MemberSecret: seed.Secret(), Certificate: *sc}
	out, err := identity.Marshal(&st)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "certified device %s for member %s (%s)\n", device.Short(), sc.Certificate.Member.Short(), strings.Join(roles, ", "))
	fmt.Fprintln(os.Stderr, "the output carries your member secret: copy it to the device over SSH, accept it there, then delete both copies")
	_, err = os.Stdout.Write(out)
	return err
}

func identityAcceptCertificate(args []string) error {
	fs := flag.NewFlagSet("identity accept-certificate", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network == "" || fs.NArg() != 1 {
		return errors.New("usage: identity accept-certificate --network NAME <file>")
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	seedBytes, err := os.ReadFile(filepath.Join(d, "device-key.pending"))
	if err != nil {
		return fmt.Errorf("no pending device key here; run identity device-key first (%w)", err)
	}
	dev, err := identity.DeviceKeyFromSeed(seedBytes)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var st identity.State
	if err := identity.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("certificate file: %w", err)
	}
	if err := st.Certificate.Verify(); err != nil {
		return err
	}
	if st.Certificate.Certificate.Device != dev.ID() {
		return fmt.Errorf("the certificate is for device %s, this device is %s", st.Certificate.Certificate.Device.Short(), dev.ID().Short())
	}
	if st.Network != *network || st.Certificate.Certificate.Network != *network {
		return fmt.Errorf("the certificate is for network %q", st.Certificate.Certificate.Network)
	}
	st.DeviceSeed = dev.Seed()
	if err := identity.Save(identityPath(d), &st, nil); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(d, "device-key.pending"))
	fmt.Printf("Identity written to %s\n", identityPath(d))
	printState(&st)
	return nil
}

func loadState(path, network string) (*identity.State, error) {
	if network == "" {
		return nil, errors.New("--network is required")
	}
	sealed, err := identity.IsSealed(path)
	if err != nil {
		return nil, err
	}
	var pass []byte
	if sealed {
		if pass, err = readSecret("Passphrase: "); err != nil {
			return nil, err
		}
	}
	return identity.Load(path, network, pass)
}

func printState(st *identity.State) {
	dev, _ := st.Device()
	fmt.Printf("network:     %s\n", st.Network)
	fmt.Printf("member key:  %s\n", st.Member)
	fmt.Printf("member code: %s\n", identity.DisplayCode(st.Member))
	if dev != nil {
		fmt.Printf("device key:  %s\n", dev.ID())
	}
	fmt.Printf("roles:       %s\n", strings.Join(st.Certificate.Certificate.Roles, ", "))
	fmt.Printf("certified:   %s, serial %d\n", st.Certificate.Certificate.IssuedAt.UTC().Format(time.RFC3339), st.Certificate.Certificate.Serial)
}

func printPhrase(words []string) {
	for i, w := range words {
		fmt.Printf("  %2d. %-12s", i+1, w)
		if i%3 == 2 {
			fmt.Println()
		}
	}
}

// confirmPhrase asks for three words at random positions, the RFC's check
// that the phrase was written down.
func confirmPhrase(words []string) error {
	in := bufio.NewReader(os.Stdin)
	for _, i := range threeIndices(len(words)) {
		for attempt := 0; ; attempt++ {
			fmt.Printf("Word %d: ", i+1)
			line, err := in.ReadString('\n')
			if err != nil && line == "" {
				return errors.New("no input")
			}
			if strings.TrimSpace(strings.ToLower(line)) == words[i] {
				break
			}
			if attempt == 2 {
				return errors.New("the phrase was not confirmed; nothing was written, run init again")
			}
			fmt.Println("That is not the word. Look at what you wrote down.")
		}
	}
	return nil
}

func threeIndices(n int) []int {
	seen := map[int]bool{}
	var out []int
	for len(out) < 3 {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
		i := int(v.Int64())
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

func readPhrase() (identity.Seed, error) {
	line, err := readSecret("Recovery phrase (12 words): ")
	if err != nil {
		return identity.Seed{}, err
	}
	return identity.ParsePhrase(strings.Fields(string(line)))
}

func readNewPassphrase() ([]byte, error) {
	a, err := readSecret("New passphrase: ")
	if err != nil {
		return nil, err
	}
	b, err := readSecret("Again: ")
	if err != nil {
		return nil, err
	}
	if string(a) != string(b) {
		return nil, errors.New("the passphrases differ")
	}
	if len(a) == 0 {
		return nil, errors.New("empty passphrase; omit --passphrase for an unsealed file")
	}
	return a, nil
}

// readSecret reads a line without echo on a terminal, and plainly from a pipe.
func readSecret(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return b, err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}
