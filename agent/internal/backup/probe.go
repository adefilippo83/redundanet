package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
)

// AccessError says a folder of a set cannot be read by the service, with
// what to do about it.
type AccessError struct {
	Folder string
	Hint   string
}

func (e *AccessError) Error() string { return fmt.Sprintf("%s: %s", e.Folder, e.Hint) }

// ProbeFolder is the privacy probe of RFC 0001's macOS notes: it tries to
// list the folder as the service does, so a folder the system guards is
// reported before a run, with the setting to change, instead of as a
// tree of skipped files afterwards.
func ProbeFolder(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	f, err := os.Open(path)
	if err == nil {
		_, err = f.Readdirnames(1)
		f.Close()
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	hint := "the service cannot read this folder; give its user permission"
	if runtime.GOOS == "darwin" {
		hint = "macOS denies the service access to this folder: grant Full Disk Access to redundanet in System Settings, Privacy & Security, then run the backup again"
	}
	return &AccessError{Folder: path, Hint: hint}
}

// Needs lists, for a set, the folders the service cannot read right now,
// with their hints; empty when every folder opens.
func Needs(s Set) []string {
	var out []string
	for _, f := range s.Folders {
		if err := ProbeFolder(f); err != nil {
			out = append(out, err.Error())
		}
	}
	return out
}
