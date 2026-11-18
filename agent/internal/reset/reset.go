// Package reset takes a device back to the state it had before it got an
// identity. An identity is never replaced in place: what belongs to it
// goes, and the device is set up again. The program and what the updater
// knows stay, so nothing has to be downloaded or installed again.
package reset

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// kept is what a reset leaves in the data directory: the binary the
// service runs (and on macOS the launcher that starts it), the releases
// the updater downloaded and its record, which carries the channel, the
// descriptor of the network the device joined, which names no member, and
// the service's log. Everything else is an identity's: the identity
// itself and a pending device key, the pairing code, the manifest copy,
// the backup settings, the local database, the spool, the local API's
// address and token, the list of paired devices.
var kept = map[string]bool{
	"bin": true, "launcher": true, "versions": true, "updates": true,
	"network.yaml": true, "service.log": true,
}

// The share store is not an identity's either: it holds other members'
// shares, which the network counts on until the manifest stops listing the
// device. StoreRecord is the node's copy of the marker on the store's
// disk, and the store refuses to open with one and not the other, so the
// two stay or go together. StoreDir is where the store is when the device
// was given no storage path: in the data directory itself.
const (
	StoreRecord = "storage-id"
	StoreDir    = "redundanet3"
)

// Clear removes from the data directory everything a reset does not keep
// and returns the names it removed, sorted. The store's record, and a
// store in the data directory, go only with store set. The agent must not
// be running. A file a stopping process still holds, which Windows
// refuses to delete, is tried again for a few seconds.
func Clear(dir string, store bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if kept[e.Name()] || (!store && (e.Name() == StoreRecord || e.Name() == StoreDir)) {
			continue
		}
		if err := removeAll(filepath.Join(dir, e.Name())); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	sort.Strings(removed)
	return removed, nil
}

// Empty removes what a directory holds and keeps the directory, with its
// owner and mode: a store's directory is made by root for the service
// user, who could not make it again.
func Empty(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for i, e := range entries {
		if err := removeAll(filepath.Join(dir, e.Name())); err != nil {
			return i, err
		}
	}
	return len(entries), nil
}

func removeAll(path string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}
