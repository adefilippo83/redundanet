package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

// `object put|get|has` drive the storage protocol by hand: the grid test of
// phase 2's M2, and a way to look at one object. The backup job of M4 is
// the real client; these take explicit servers and do no placement.

func runObject(args []string) error {
	if len(args) == 0 {
		return errors.New("object: a subcommand is needed: put, get, has")
	}
	fs := flag.NewFlagSet("object "+args[0], flag.ContinueOnError)
	dir, network := commonFlags(fs)
	servers := fs.String("servers", "", "comma-separated device keys of storage nodes")
	out := fs.String("out", "", "get: write the object here instead of stdout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *network == "" || *servers == "" || fs.NArg() != 1 {
		return errors.New("usage: object put|get|has --network NAME --servers KEY,... FILE|CAP")
	}
	var peers []identity.Key
	for _, s := range strings.Split(*servers, ",") {
		k, err := identity.ParseKey(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		peers = append(peers, k)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rc, err := loadRuntime(ctx, *dir, *network, "", "", false, nil)
	if err != nil {
		return err
	}
	man := rc.current()
	var hubs []transport.HubAddr
	for _, h := range man.Hubs {
		hubs = append(hubs, transport.HubAddr{Key: h.Key, UDP: h.Relay, TCP: h.TCPAddr()})
	}
	ep, err := transport.Start(ctx, transport.Config{Identity: rc.tlsID, Device: rc.device, Allow: rc.allow, Hubs: hubs, Listen: ":0",
		OnStream: shareproto.RefuseAll, Log: quietLog()})
	if err != nil {
		return err
	}
	defer ep.Close()
	for _, p := range peers {
		ep.Connect(p)
	}
	c := shareproto.NewClient(ep.OpenStream)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	switch args[0] {
	case "put":
		return objectPut(ctx, rc, man.Network.Encoding.K, man.Network.Encoding.N, c, peers, fs.Arg(0))
	case "get":
		return objectGet(ctx, rc, c, peers, fs.Arg(0), *out)
	case "has":
		return objectHas(ctx, c, peers, fs.Arg(0))
	}
	return fmt.Errorf("object: unknown subcommand %q", args[0])
}

func objectPut(ctx context.Context, rc *runtimeConfig, k, n int, c *shareproto.Client, peers []identity.Key, path string) error {
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
	p := object.DefaultParams(k, n)
	conv := object.ConvergenceSecret(rc.state.MemberSecret)
	e, err := object.Encode(f, fi.Size(), h, p, conv, rc.state.Member)
	if err != nil {
		return err
	}
	dev, err := rc.state.Device()
	if err != nil {
		return err
	}
	sd, err := object.Sign(e.Descriptor, dev, &rc.state.Certificate)
	if err != nil {
		return err
	}
	// Every block in memory: this is a hand tool, not the backup job.
	blocks := make([][][]byte, p.N)
	for i := range blocks {
		blocks[i] = make([][]byte, e.Geometry().Segments())
	}
	if err := e.Shares(ctx, func(share, seg int, b []byte) error { blocks[share][seg] = append([]byte(nil), b...); return nil }); err != nil {
		return err
	}
	rc.log.Info("object encoded", "size", fi.Size(), "segments", e.Geometry().Segments(), "k", k, "n", n)
	for i := 0; i < int(p.N); i++ {
		peer := peers[i%len(peers)]
		start := time.Now()
		if err := c.PutShare(ctx, peer, sd, uint8(i), func(seg int) ([]byte, error) { return blocks[i][seg], nil }); err != nil {
			return fmt.Errorf("share %d to %s: %w", i, peer.Short(), err)
		}
		fmt.Fprintf(os.Stderr, "share %d stored on %s in %s\n", i, peer.Short(), time.Since(start).Round(time.Millisecond))
	}
	rcap, err := e.ReadCap()
	if err != nil {
		return err
	}
	fmt.Println(rcap.String())
	return nil
}

func objectGet(ctx context.Context, rc *runtimeConfig, c *shareproto.Client, peers []identity.Key, capStr, out string) error {
	rcap, err := object.ParseReadCap(capStr)
	if err != nil {
		return err
	}
	si := rcap.Key.StorageIndex()
	var desc *object.Descriptor
	var sources []shareproto.Source
	for _, peer := range peers {
		pres, err := c.Has(ctx, peer, si)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", peer.Short(), err)
			continue
		}
		if pres == nil {
			continue
		}
		for _, n := range pres.Complete {
			if desc == nil {
				sd, _, err := c.GetInfo(ctx, peer, si, n)
				if err != nil || sd == nil {
					continue
				}
				if err := sd.Verify(rc.state.Network); err != nil {
					return err
				}
				if dh, _ := sd.Descriptor.Hash(); dh != rcap.DescriptorHash {
					return errors.New("the holder's descriptor does not match the capability")
				}
				d := sd.Descriptor
				desc = &d
			}
			sources = append(sources, shareproto.Source{Share: int(n), Client: c, Peer: peer, StorageIndex: si, Geometry: desc.Geometry()})
		}
	}
	if desc == nil {
		return errors.New("no server holds a complete share of this object")
	}
	srcs, err := shareproto.Sources(ctx, sources)
	if err != nil {
		return err
	}
	w := os.Stdout
	if out != "" {
		tmp := out + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		conv := object.ConvergenceSecret(rc.state.MemberSecret)
		if err := object.Decode(ctx, f, rcap, desc, &conv, srcs); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, out)
	}
	conv := object.ConvergenceSecret(rc.state.MemberSecret)
	return object.Decode(ctx, w, rcap, desc, &conv, srcs)
}

func objectHas(ctx context.Context, c *shareproto.Client, peers []identity.Key, capStr string) error {
	var si object.StorageIndex
	if rcap, err := object.ParseReadCap(capStr); err == nil {
		si = rcap.Key.StorageIndex()
	} else if vcap, err := object.ParseVerifyCap(capStr); err == nil {
		si = vcap.StorageIndex
	} else {
		return errors.New("object has: give a read or verify capability")
	}
	for _, peer := range peers {
		pres, err := c.Has(ctx, peer, si)
		switch {
		case err != nil:
			fmt.Printf("%s: %v\n", peer.Short(), err)
		case pres == nil:
			fmt.Printf("%s: nothing\n", peer.Short())
		default:
			fmt.Printf("%s: complete %v partial %v tombstoned %v owner %s\n", peer.Short(), pres.Complete, pres.Partial, pres.Tombstoned, pres.Owner.Short())
		}
	}
	return nil
}
