package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
)

// `storage has|drop` work on this node's own share store, beside the
// running agent: the index is SQLite in WAL mode, so a second process may
// read and write it, and a removal bumps the census generation the agent
// polls, so the hub learns of it within seconds. `drop` is the operator's
// way to lose one share on purpose, which is how repair is drilled and
// how a disk is freed by hand. Run it as the service user, so the files
// it touches keep their owner.
func runStorage(args []string) error {
	if len(args) == 0 {
		return errors.New("storage: a subcommand is needed: has, drop")
	}
	fs := flag.NewFlagSet("storage "+args[0], flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	storagePath := fs.String("storage-path", "", "the disk the store lives on, as given to the agent (default: the data directory)")
	floor := fs.Int64("storage-floor-gb", 10, "the agent's floor; a drop needs none")
	reason := fs.String("reason", "dropped by the operator", "drop: the reason recorded in the log")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	d, err := dataDir(*dir)
	if err != nil {
		return err
	}
	disk := *storagePath
	if disk == "" {
		disk = d
	}
	switch args[0] {
	case "has":
		if fs.NArg() != 1 {
			return errors.New("usage: storage has [--storage-path DIR] STORAGE_INDEX")
		}
		si, err := object.ParseStorageIndex(fs.Arg(0))
		if err != nil {
			return err
		}
		return storageHas(disk, d, *floor, si)
	case "drop":
		if fs.NArg() != 2 {
			return errors.New("usage: storage drop [--storage-path DIR] STORAGE_INDEX SHARE_NUMBER")
		}
		si, err := object.ParseStorageIndex(fs.Arg(0))
		if err != nil {
			return err
		}
		n, err := strconv.ParseUint(fs.Arg(1), 10, 8)
		if err != nil {
			return fmt.Errorf("share number: %w", err)
		}
		return storageDrop(disk, d, *floor, si, uint8(n), *reason)
	}
	return fmt.Errorf("storage: unknown subcommand %q", args[0])
}

func storageHas(disk, dataDir string, floorGB int64, si object.StorageIndex) error {
	st, err := openStore(disk, dataDir, floorGB, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := st.Has(si)
	if err != nil {
		return err
	}
	if p == nil {
		fmt.Println("nothing held")
		return nil
	}
	fmt.Printf("owner %s, descriptor %s\n", p.Owner.Short(), p.DescriptorHash.String()[:8])
	fmt.Printf("complete shares %v\n", p.Complete)
	for n, got := range p.Partial {
		fmt.Printf("partial share %d: %d segments\n", n, got)
	}
	if p.Tombstoned {
		fmt.Println("tombstoned")
	}
	return nil
}

func storageDrop(disk, dataDir string, floorGB int64, si object.StorageIndex, n uint8, reason string) error {
	st, err := openStore(disk, dataDir, floorGB, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Remove(si, n, reason); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("share %d of %s is not held here", n, si)
		}
		return err
	}
	fmt.Printf("dropped share %d of %s; the census tells the hub within seconds\n", n, si)
	return nil
}
