package backup

import (
	"fmt"
	"os"
)

// scanKey on Windows uses path, size and mtime: the file id and change
// time need a handle per file, which a later version adds.
func scanKey(path string, fi os.FileInfo) string {
	return fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
}
