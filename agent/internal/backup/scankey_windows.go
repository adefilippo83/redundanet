package backup

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// scanKey on Windows is the RFC's key in full: path, size, mtime, the
// change time and the file id (volume serial and file index), which need
// a handle per file. A file that cannot be opened keeps the cheap key of
// path, size and mtime, which still catches every ordinary edit.
func scanKey(path string, fi os.FileInfo) string {
	if id, ctime, ok := fileID(path); ok {
		return fmt.Sprintf("%s|%d|%d|%d|%s", path, fi.Size(), fi.ModTime().UnixNano(), ctime, id)
	}
	return fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
}

// fileBasicInfo is FILE_BASIC_INFO: times in 100 ns since 1601.
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// fileID opens the file without access rights, which needs no read
// permission and does not touch its access time, and reads its id and
// change time.
func fileID(path string) (id string, ctime int64, ok bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, false
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", 0, false
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", 0, false
	}
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return "", 0, false
	}
	// 100 ns intervals since 1601-01-01 to unix nanoseconds.
	const epochDiff = 116444736000000000
	return fmt.Sprintf("%d:%d", info.VolumeSerialNumber, uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow)), (basic.ChangeTime - epochDiff) * 100, true
}
