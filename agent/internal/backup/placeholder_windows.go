package backup

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// cloudPlaceholder reports a file whose bytes are not on this computer:
// a OneDrive or other provider's online-only file, which Windows would
// fetch on open. The backup lists it instead of pulling it down.
func cloudPlaceholder(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	const recallOnOpen = 0x00040000 // FILE_ATTRIBUTE_RECALL_ON_OPEN
	return st.FileAttributes&(windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS|windows.FILE_ATTRIBUTE_OFFLINE|recallOnOpen) != 0
}
