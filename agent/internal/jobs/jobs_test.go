package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

func TestRunnerRunsVerifiesAndRestores(t *testing.T) {
	var member, device identity.Key
	member[0], device[0] = 1, 2
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	fake := engine.NewFake(member, device, 2, 4)
	dir := t.TempDir()
	db, err := localdb.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	src := filepath.Join(t.TempDir(), "docs")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "a.bin"), make([]byte, 5000), 0o644)
	_ = os.WriteFile(filepath.Join(src, "b.txt"), []byte("hello"), 0o644)
	r, err := New(Config{DataDir: dir, Engine: fake, DB: db, Member: member, Device: device, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddSet(backup.Set{Name: "docs", Folders: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if err := r.AddSet(backup.Set{Name: "bad", Folders: []string{filepath.Join(src, "missing")}}); err == nil {
		t.Fatal("a missing folder was accepted")
	}
	again, _ := Load(dir)
	if len(again.Sets) != 1 || again.Sets[0].Folders[0] != src || again.Every != 24*time.Hour {
		t.Fatalf("saved file: %+v", again)
	}
	rs := r.RunSet(context.Background(), again.Sets[0], snapshot.DefaultRetention)
	if rs == nil || rs.LastErr != "" || rs.Last.Snapshot.Files != 2 || rs.Running {
		t.Fatalf("run: %+v", rs)
	}
	st := r.Status()
	if len(st.Runs) != 1 || st.Runs[0].Last == nil || st.Sets[0].Name != "docs" {
		t.Fatalf("status: %+v", st)
	}
	at, partial := r.LastBackup()
	if !at.Equal(now) || partial {
		t.Fatalf("last backup %v partial %v", at, partial)
	}
	v := r.Verify(context.Background())
	if v == nil || v.Sampled != 2 || v.Verified != 2 || v.Compared != 2 {
		t.Fatalf("verify: %+v", v)
	}
	target := filepath.Join(t.TempDir(), "out")
	if err := r.StartRestore(device, rs.Last.Snapshot.ID, src, target, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if s := r.Status().Restore; s != nil && !s.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := r.Status().Restore
	if s == nil || s.Running || s.Err != "" || s.Report == nil || s.Report.Files != 2 {
		t.Fatalf("restore: %+v", s)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "b.txt")); string(b) != "hello" {
		t.Fatal("restored content differs")
	}
	if err := r.RemoveSet("docs"); err != nil || len(r.Status().Sets) != 0 {
		t.Fatalf("remove: %v", err)
	}
}
