//go:build !windows

package backup

import "os"

// cloudPlaceholder is answered on read on the other platforms: macOS
// refuses an evicted iCloud file with EDEADLK, which skipReason names.
func cloudPlaceholder(os.FileInfo) bool { return false }
