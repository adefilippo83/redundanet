//go:build windows

package node

// A Windows node is not laid out by this package; the check passes.
func isMountPoint(string) (bool, error) { return true, nil }
