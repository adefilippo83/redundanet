package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

func fake() *engine.Fake {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	return engine.NewFake(member, device, 2, 3)
}

// dir stores a directory object and returns its capability.
func dir(t *testing.T, e engine.Engine, entries ...snapshot.Entry) string {
	t.Helper()
	b, err := snapshot.NewDirectory(entries).Encode()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := e.Put(context.Background(), bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return rc.String()
}

func file(name, content string) snapshot.Entry {
	return snapshot.Entry{Name: []byte(name), UTF8: true, Kind: snapshot.KindFile, Size: int64(len(content)), Inline: []byte(content), Mode: 0o644}
}

func sub(name, capStr string) snapshot.Entry {
	return snapshot.Entry{Name: []byte(name), UTF8: true, Kind: snapshot.KindDir, Cap: capStr}
}

func link(name, target string) snapshot.Entry {
	return snapshot.Entry{Name: []byte(name), UTF8: true, Kind: snapshot.KindSymlink, Target: target}
}

// place is a restore target two levels down, so there is room above it
// that a restore must not reach, and a folder beside it.
func place(t *testing.T) (base, target, outside string) {
	t.Helper()
	base = t.TempDir()
	target = filepath.Join(base, "a", "b", "target")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{target, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return base, target, outside
}

// below lists every path under root that is not a directory.
func below(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// A snapshot is written by whoever holds the member's secret and one of
// its devices, which after a theft is not the member. Its entry names
// must not take a restore out of the folder the member chose.
func TestNamesStayInsideTheTarget(t *testing.T) {
	e := fake()
	base, target, _ := place(t)
	inner := dir(t, e,
		file("../../escaped.txt", "x"),
		file("../beside.txt", "x"),
		file("deep/er.txt", "x"),
		file("..", "x"),
		file(".", "x"),
		file("", "x"),
		sub("../../escaped-dir", dir(t, e, file("in.txt", "x"))),
		file("kept.txt", "kept"),
	)
	top := dir(t, e, sub("/src", inner), sub("/../../top-escape", dir(t, e, file("in.txt", "x"))), sub("..", dir(t, e, file("in.txt", "x"))))
	rep, err := Restore(context.Background(), e, &snapshot.Snapshot{Top: top}, nil, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := below(t, base)
	if len(got) != 1 || got[0] != "a/b/target/src/kept.txt" {
		t.Fatalf("files after the restore: %v", got)
	}
	if rep.Files != 1 || len(rep.Exceptions) != 9 {
		t.Fatalf("report: %d files, exceptions %q", rep.Files, rep.Exceptions)
	}
	for _, x := range rep.Exceptions {
		if !strings.Contains(x, "not a name a restore writes") {
			t.Fatalf("exception %q", x)
		}
	}
}

// The top directory names each folder by its whole path, so those names
// do contain separators; the folder is recreated under the target.
func TestTopFolderPathsAreKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("separators in a name are replaced on Windows")
	}
	e := fake()
	_, target, _ := place(t)
	top := dir(t, e, sub("/Users/a/Documents", dir(t, e, file("note.txt", "hello"))))
	rep, err := Restore(context.Background(), e, &snapshot.Snapshot{Top: top}, nil, target, Options{})
	if err != nil || rep.Files != 1 || len(rep.Exceptions) != 0 {
		t.Fatalf("restore: %+v %v", rep, err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "Users", "a", "Documents", "note.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("restored file: %q %v", b, err)
	}
}

// A symlink the same restore created must not carry a later write out of
// the target: neither as a folder to write into nor as the temporary name
// of a file.
func TestSymlinksDoNotLeadOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	e := fake()
	base, target, outside := place(t)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	top := dir(t, e,
		sub("/src", dir(t, e, link("door", outside), link("f.txt.rn3-restoring", victim), file("f.txt", "overwritten"))),
		sub("/src/door", dir(t, e, file("planted.txt", "x"))),
	)
	rep, err := Restore(context.Background(), e, &snapshot.Snapshot{Top: top}, nil, target, Options{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "mine" {
		t.Fatalf("a file outside the target was written through a symlink: %q", b)
	}
	if got := below(t, outside); len(got) != 1 {
		t.Fatalf("outside the target after the restore: %v", got)
	}
	if rep.Symlinks != 2 || len(rep.Exceptions) == 0 {
		t.Fatalf("report: %+v (under %s)", rep, base)
	}
}

// A single file restores to the path given, in a folder that exists.
func TestOneFile(t *testing.T) {
	e := fake()
	_, target, _ := place(t)
	top := dir(t, e, sub("/src", dir(t, e, file("one.txt", "one"))))
	dst := filepath.Join(target, "copy.txt")
	for i, want := range []string{"copy.txt", "copy.txt.restored"} {
		rep, err := Restore(context.Background(), e, &snapshot.Snapshot{Top: top}, []string{"/src", "one.txt"}, dst, Options{})
		if err != nil || rep.Files != 1 {
			t.Fatalf("restore %d: %+v %v", i, rep, err)
		}
		if b, err := os.ReadFile(filepath.Join(target, want)); err != nil || string(b) != "one" {
			t.Fatalf("restore %d: %q %v", i, b, err)
		}
	}
}

// The mode a snapshot gives a file is its permission bits and nothing
// more: a forged entry does not produce a set-user-ID file.
func TestModeIsPermissionBitsOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such bits on Windows")
	}
	e := fake()
	_, target, _ := place(t)
	f := file("tool", "#!/bin/sh\n")
	f.Mode = uint32(os.ModeSetuid | os.ModeSetgid | os.ModeSticky | 0o755)
	top := dir(t, e, sub("/src", dir(t, e, f)))
	if _, err := Restore(context.Background(), e, &snapshot.Snapshot{Top: top}, nil, target, Options{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(target, "src", "tool"))
	if err != nil || fi.Mode() != 0o755 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
}
