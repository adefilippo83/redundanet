//go:build !windows

package backup

import (
	"fmt"
	"os"
	"syscall"
)

// scanKey is the change-detection key of RFC 0001: path, size, mtime,
// change time and the file id (device and inode). On a filesystem without
// a stable id the id is zero and the key falls back to path, size and
// the times.
func scanKey(path string, fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%s|%d|%d|%d|%d:%d", path, fi.Size(), fi.ModTime().UnixNano(), ctime(st), st.Dev, st.Ino)
	}
	return fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
}
