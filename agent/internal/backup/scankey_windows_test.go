package backup

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestScanKeyCarriesTheFileID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(p)
	key := scanKey(p, fi)
	parts := strings.Split(key, "|")
	if len(parts) != 5 || !strings.Contains(parts[4], ":") || parts[4] == "0:0" {
		t.Fatalf("key without a file id: %q", key)
	}
	// The id follows the file through a rename; the path part does not.
	q := filepath.Join(dir, "b.txt")
	if err := os.Rename(p, q); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Lstat(q)
	parts2 := strings.Split(scanKey(q, fi2), "|")
	if parts2[4] != parts[4] || parts2[0] == parts[0] {
		t.Fatalf("after a rename: %v vs %v", parts2, parts)
	}
	// The change time moves with an edit.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(q, []byte("hello again"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi3, _ := os.Lstat(q)
	if parts3 := strings.Split(scanKey(q, fi3), "|"); parts3[3] == parts2[3] {
		t.Fatalf("change time did not move: %v", parts3)
	}
}

type fakeInfo struct {
	os.FileInfo
	attrs uint32
}

func (f fakeInfo) Sys() any { return &syscall.Win32FileAttributeData{FileAttributes: f.attrs} }

func TestCloudPlaceholderByAttribute(t *testing.T) {
	for attrs, want := range map[uint32]bool{
		windows.FILE_ATTRIBUTE_NORMAL:                false,
		windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS: true,
		windows.FILE_ATTRIBUTE_OFFLINE:               true,
		0x00040000:                                   true, // recall on open
		windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_READONLY: false,
	} {
		if got := cloudPlaceholder(fakeInfo{attrs: attrs}); got != want {
			t.Errorf("attributes %#x: %v, want %v", attrs, got, want)
		}
	}
}
