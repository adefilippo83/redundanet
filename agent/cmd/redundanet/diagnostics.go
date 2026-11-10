package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/diag"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// `diagnostics` writes the bundle a member sends by hand when something is
// wrong. It asks the running agent when it answers and reads the data
// directory and the service log when it does not, so it works either way.
// Nothing is uploaded.
func runDiagnostics(args []string) error {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	out := fs.String("out", "", "where to write the bundle (default: redundanet-diagnostics-DATE.zip in the current directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	in := diag.Input{Now: time.Now(), Version: version.Version, DataDir: d, Network: *network}
	// The descriptor names the network when the flag does not.
	if desc, err := membership.LoadDescriptor(filepath.Join(d, "network.yaml")); err == nil {
		if in.Network == "" {
			in.Network = desc.Network
		}
	}
	// The identity, when it is not sealed behind a passphrase.
	if sealed, err := identity.IsSealed(identityPath(d)); err == nil && !sealed && in.Network != "" {
		if st, err := identity.Load(identityPath(d), in.Network, nil); err == nil {
			if dev, err := st.Device(); err == nil {
				in.Device = dev.ID().String()
			}
			in.Member = st.Member.Short()
			in.Roles = st.Certificate.Certificate.Roles
		}
	}
	// The manifest on disk.
	if b, err := os.ReadFile(filepath.Join(d, "manifest.yaml")); err == nil {
		if m, err := manifest.Parse(b); err == nil {
			in.Manifest = &diag.ManifestInfo{Revision: m.Revision, Encoding: fmt.Sprintf("%d-of-%d", m.Network.Encoding.K, m.Network.Encoding.N), Hubs: len(m.Hubs), Members: len(m.Members), Storage: len(m.Storage)}
			if sb, err := os.ReadFile(filepath.Join(d, "manifest.sig")); err == nil {
				if sig, err := manifest.ParseSignature(sb); err == nil {
					in.Manifest.SignedAt = sig.IssuedAt
				}
			}
		}
	}
	sv := serviceStatus()
	in.Service = &sv
	// The running agent, when it answers.
	if c, err := localClient(*dir); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if snap, err := c.Status(ctx); err == nil {
			in.AgentUp = true
			in.Uptime = snap.Uptime
			if in.Network == "" {
				in.Network = snap.Network
			}
			if in.Device == "" {
				in.Device, in.Member, in.Roles = snap.Device, snap.Member, snap.Roles
			}
			in.Transport, in.Membership, in.Update, in.Jobs, in.Repair = snap.Transport, snap.Membership, snap.Update, snap.Jobs, snap.Repair
			if lines, err := c.LogLines(ctx, 2000); err == nil {
				in.Lines = lines
			}
		}
	}
	// The service's log: the file launchd writes on macOS, the journal on
	// Linux; on Windows the agent's own lines are what there is. The
	// journal is read as the messages alone: its own prefix names the host,
	// and every line of the agent carries its time already.
	if text, err := diag.TailFile(filepath.Join(d, "service.log"), 1<<20); err == nil {
		in.LogText = text
	} else if runtime.GOOS == "linux" {
		for _, args := range [][]string{{"--user", "-u", "redundanet.service", "-n", "2000", "--no-pager", "-o", "cat"}, {"-u", "redundanet-agent", "-n", "2000", "--no-pager", "-o", "cat"}} {
			if b, err := exec.Command("journalctl", args...).Output(); err == nil && len(b) > 0 {
				in.LogText = string(b)
				break
			}
		}
	}
	path := *out
	if path == "" {
		path = fmt.Sprintf("redundanet-diagnostics-%s.zip", in.Now.UTC().Format("20060102-150405"))
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s exists already; pass --out for another name", path)
		}
		return err
	}
	if err := diag.Write(f, in); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	abs, _ := filepath.Abs(path)
	source := "the data directory and the service log"
	if in.AgentUp {
		source = "the running agent, the data directory and the service log"
	}
	fmt.Printf("wrote %s from %s\n", abs, source)
	fmt.Println("it holds versions, the manifest revision, the service state, hubs and peers as counts, the backup sets with folders as label and hash,")
	fmt.Println("error kinds and counts, a disk check and the last log lines with paths, URLs and addresses replaced; nothing was uploaded.")
	fmt.Println("the replacement works by pattern: look through log.txt in the zip before you send it by hand")
	_ = strings.TrimSpace
	return nil
}
