package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

// `object put|get|has|check|repair` drive the native engine by hand: the
// grid tests of phase 2 and a way to look at one object. The engine places
// shares on the manifest's storage servers; the backup job of M4 is the
// real client.

func runObject(args []string) error {
	if len(args) == 0 {
		return errors.New("object: a subcommand is needed: put, get, has, check, repair, delete")
	}
	fs := flag.NewFlagSet("object "+args[0], flag.ContinueOnError)
	dir, network := commonFlags(fs)
	out := fs.String("out", "", "get: write the object here instead of stdout")
	timeout := fs.Duration("timeout", 30*time.Second, "per server operation")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *network == "" || fs.NArg() != 1 {
		return errors.New("usage: object put|get|has|check|repair|delete --network NAME FILE|CAP")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rc, err := loadRuntime(ctx, *dir, *network, "", "", false, nil)
	if err != nil {
		return err
	}
	e, closeAll, err := newEngine(ctx, rc, *timeout)
	if err != nil {
		return err
	}
	defer closeAll()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	switch args[0] {
	case "put":
		return objectPut(ctx, e, fs.Arg(0))
	case "get":
		return objectGet(ctx, e, fs.Arg(0), *out)
	case "has":
		return objectHas(ctx, e, fs.Arg(0))
	case "check", "repair":
		return objectCheck(ctx, e, fs.Arg(0), args[0] == "repair")
	case "delete":
		rc, err := object.ParseReadCap(fs.Arg(0))
		if err != nil {
			return err
		}
		res, err := e.DeleteByIndex(ctx, rc.Key.StorageIndex(), rc.DescriptorHash)
		if res != nil {
			fmt.Printf("%d of %d servers answered: %d tombstoned, %d held nothing, %d refused as touched recently, %d refused as stale\n",
				res.Answered, res.Servers, res.Tombstoned, res.NotHeld, res.Recent, res.Stale)
			for dev, msg := range res.Failed {
				fmt.Printf("  %s: %s\n", dev.Short(), msg)
			}
		}
		return err
	}
	return fmt.Errorf("object: unknown subcommand %q", args[0])
}

// newEngine builds a native engine on a standalone endpoint: the tools run
// outside the agent, with the same identity and manifest.
func newEngine(ctx context.Context, rc *runtimeConfig, timeout time.Duration) (*engine.Native, func(), error) {
	man := rc.current()
	if man == nil {
		return nil, nil, errors.New("no manifest")
	}
	var hubs []transport.HubAddr
	for _, h := range man.Hubs {
		hubs = append(hubs, transport.HubAddr{Key: h.Key, UDP: h.Relay, TCP: h.TCPAddr()})
	}
	ep, err := transport.Start(ctx, transport.Config{Identity: rc.tlsID, Device: rc.device, Allow: rc.allow, Hubs: hubs, Listen: ":0",
		OnStream: shareproto.RefuseAll, Log: quietLog()})
	if err != nil {
		return nil, nil, err
	}
	for _, s := range man.Storage {
		ep.Connect(s.Device)
	}
	db, err := localdb.Open(rc.dataDir, nil)
	if err != nil {
		_ = ep.Close()
		return nil, nil, err
	}
	dev, err := rc.state.Device()
	if err != nil {
		_ = ep.Close()
		_ = db.Close()
		return nil, nil, err
	}
	spool := filepath.Join(rc.dataDir, "spool")
	_ = os.MkdirAll(spool, 0o700)
	e, err := engine.New(engine.Config{Network: rc.state.Network, Member: rc.state.Member, Secret: rc.state.MemberSecret, Device: dev, Cert: &rc.state.Certificate,
		Manifest: rc.current, Client: shareproto.NewClient(ep.OpenStream), DB: db, SpoolDir: spool, Timeout: timeout, Log: rc.log})
	if err != nil {
		_ = ep.Close()
		_ = db.Close()
		return nil, nil, err
	}
	return e, func() { _ = db.Close(); _ = ep.Close() }, nil
}

func objectPut(ctx context.Context, e *engine.Native, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	start := time.Now()
	rc, err := e.Put(ctx, f, fi.Size())
	if err != nil {
		return err
	}
	h, _ := e.Check(ctx, rc)
	fmt.Fprintf(os.Stderr, "stored %d bytes in %s: %s\n", fi.Size(), time.Since(start).Round(time.Millisecond), describe(h))
	fmt.Println(rc.String())
	return nil
}

func objectGet(ctx context.Context, e *engine.Native, capStr, out string) error {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return err
	}
	if out == "" {
		return e.Get(ctx, rc, os.Stdout, nil)
	}
	tmp := out + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	start := time.Now()
	if err := e.Get(ctx, rc, f, nil); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "read %d bytes in %s, verified\n", rc.Size, time.Since(start).Round(time.Millisecond))
	return nil
}

func objectHas(ctx context.Context, e *engine.Native, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h, err := object.HashSpool(f, fi.Size())
	if err != nil {
		return err
	}
	rc, ok, err := e.Has(ctx, h, fi.Size())
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("not stored")
		return nil
	}
	fmt.Println(rc.String())
	return nil
}

func objectCheck(ctx context.Context, e *engine.Native, capStr string, repair bool) error {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return err
	}
	var h *engine.Health
	if repair {
		h, err = e.Repair(ctx, rc)
	} else {
		h, err = e.Check(ctx, rc)
	}
	if h != nil {
		fmt.Printf("storage index %s\n", rc.Key.StorageIndex())
		fmt.Println(describe(h))
		for dev, shares := range h.Holders {
			fmt.Printf("  %s holds %v\n", dev.Short(), shares)
		}
	}
	return err
}

func describe(h *engine.Health) string {
	if h == nil {
		return "unknown"
	}
	state := "under-placed"
	switch {
	case h.Placed:
		state = "fully placed"
	case !h.Readable:
		state = "NOT READABLE"
	}
	return fmt.Sprintf("%d of %d shares on %d servers of %d members (%d of %d answered): %s", len(h.Shares), h.N, h.Servers, h.Members, h.Answered, h.Asked, state)
}

var _ = identity.Key{}
var _ = strings.Join
