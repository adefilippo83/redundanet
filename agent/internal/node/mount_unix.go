//go:build !windows

package node

import (
	"os"
	"path/filepath"
	"syscall"
)

// isMountPoint: a directory whose device differs from its parent's.
func isMountPoint(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	pi, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return false, err
	}
	a, ok1 := fi.Sys().(*syscall.Stat_t)
	b, ok2 := pi.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, nil
	}
	return a.Dev != b.Dev || a.Ino == b.Ino, nil
}
