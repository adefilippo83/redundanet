package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultExcludesCoverThePlatforms(t *testing.T) {
	root := t.TempDir()
	r := &run{excludes: DefaultExcludes}
	for _, c := range []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"$RECYCLE.BIN", true, true},
		{"System Volume Information", true, true},
		{"docs/~$report.docx", false, true},
		{"AppData/Local/Temp/x.tmp", false, true},
		{".Spotlight-V100", true, true},
		{"Library/Caches/com.x/y", false, true},
		{"docs/report.docx", false, false},
		{"AppData/Roaming/app/settings.json", false, false},
		{"node_modules/x.js", false, false},
	} {
		if got := r.excluded(root, filepath.Join(root, filepath.FromSlash(c.rel)), c.isDir); got != c.want {
			t.Errorf("%s excluded=%v, want %v", c.rel, got, c.want)
		}
	}
}

func TestProbeFolderReportsWhatToDo(t *testing.T) {
	dir := t.TempDir()
	if err := ProbeFolder(dir); err != nil {
		t.Fatalf("an empty readable folder: %v", err)
	}
	f := filepath.Join(dir, "file")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := ProbeFolder(f); err == nil {
		t.Fatal("a file probed as a folder")
	}
	if err := ProbeFolder(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing folder probed fine")
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("an unreadable folder needs a non-root unix user")
	}
	guarded := filepath.Join(dir, "guarded")
	_ = os.Mkdir(guarded, 0o000)
	t.Cleanup(func() { _ = os.Chmod(guarded, 0o755) })
	err := ProbeFolder(guarded)
	var ae *AccessError
	if !errors.As(err, &ae) || ae.Folder != guarded {
		t.Fatalf("a guarded folder: %v", err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(ae.Hint, "Full Disk Access") {
		t.Fatalf("macOS hint: %s", ae.Hint)
	}
	if needs := Needs(Set{Folders: []string{dir, guarded}}); len(needs) != 1 || !strings.Contains(needs[0], guarded) {
		t.Fatalf("needs: %v", needs)
	}
}

func TestPlaceholderSkipsWithTheCloudReason(t *testing.T) {
	counted, detail := skipReason(context.Background(), errPlaceholder)
	if counted != "not on this computer" || !strings.Contains(detail, "not on this computer") {
		t.Fatalf("%q %q", counted, detail)
	}
}
