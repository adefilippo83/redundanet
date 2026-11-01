package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/release"
)

type fakeRepo struct {
	key   *release.Key
	index []byte
	sig   []byte
	asset []byte
	hash  string
}

func newFakeRepo(t *testing.T, revision int, versions ...string) *fakeRepo {
	t.Helper()
	r := &fakeRepo{}
	r.key, _ = release.NewKey()
	r.asset, r.hash = fakeArchive("new binary bytes")
	var b strings.Builder
	fmt.Fprintf(&b, "revision: %d\nreleases:\n", revision)
	for _, v := range versions {
		fmt.Fprintf(&b, "  - version: %s\n    channel: stable\n    assets:\n      %s: {url: https://example.org/%s.tar.gz, sha256: %s, size: %d}\n", v, release.Target(), v, r.hash, len(r.asset))
	}
	r.index = []byte(b.String())
	sig, err := release.Sign(r.key, r.index, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.sig, _ = sig.Encode()
	return r
}

func fakeArchive(content string) ([]byte, string) {
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "redundanet-x/redundanet", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(tgz.Bytes())
	return tgz.Bytes(), hex.EncodeToString(sum[:])
}

func (r *fakeRepo) fetch(_ context.Context, url string) ([]byte, error) {
	switch {
	case strings.HasSuffix(url, "index.yaml"):
		return r.index, nil
	case strings.HasSuffix(url, "index.sig"):
		return r.sig, nil
	}
	return nil, errors.New("404 " + url)
}

func (r *fakeRepo) fetchAsset(_ context.Context, _ string, w io.Writer) error {
	_, err := w.Write(r.asset)
	return err
}

func newUpdater(t *testing.T, r *fakeRepo, dir string, probation func(context.Context, string) error) (*Updater, string) {
	t.Helper()
	exe := filepath.Join(dir, "redundanet")
	if _, err := os.Stat(exe); err != nil {
		if err := os.WriteFile(exe, []byte("old binary bytes"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	u := New(Config{DataDir: dir, Version: "3.0.0", Embedded: r.key.Public(), IndexURL: "https://example.org/releases/index.yaml",
		Exe: exe, Probation: probation, Fetch: r.fetch, FetchAsset: r.fetchAsset, MinVersion: func() string { return "" }})
	return u, exe
}

func TestDisabledWithoutKey(t *testing.T) {
	u := New(Config{DataDir: t.TempDir(), Version: "3.0.0"})
	if u.Enabled() {
		t.Fatal("enabled without a key")
	}
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("check succeeded without a key")
	}
}

func TestCheckApplyAndRollback(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.0", "3.0.1")
	dir := t.TempDir()
	var probed []string
	handovers := []bool{}
	u, exe := newUpdater(t, r, dir, func(_ context.Context, cand string) error {
		probed = append(probed, cand)
		b, _ := os.ReadFile(cand)
		if string(b) != "new binary bytes" {
			return errors.New("candidate is not the downloaded binary")
		}
		return nil
	})
	u.cfg.Handover = func(stop bool) { handovers = append(handovers, stop) }
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil || rel.Version != "3.0.1" {
		t.Fatalf("check: %v %+v", err, rel)
	}
	if st := u.Status(); st.Available != "3.0.1" || st.LastRevision != 1 {
		t.Fatalf("status: %+v", st)
	}
	err = u.Apply(context.Background(), rel)
	if !errors.Is(err, ErrRestart) {
		t.Fatalf("apply: %v", err)
	}
	if len(probed) != 1 || len(handovers) != 1 || !handovers[0] {
		t.Fatalf("probation %v handovers %v", probed, handovers)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary bytes" {
		t.Fatal("the executable was not replaced")
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old binary bytes" {
		t.Fatal("the previous version was not kept")
	}
	// State persisted: a new updater knows the revision and the install.
	u2 := New(Config{DataDir: dir, Version: "3.0.1", Embedded: r.key.Public()})
	if u2.st.LastRevision != 1 || u2.st.Installed["3.0.1"] == "" {
		t.Fatalf("state: %+v", u2.st)
	}
}

func TestProbationFailureKeepsTheRunningVersion(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	handovers := []bool{}
	u, exe := newUpdater(t, r, dir, func(context.Context, string) error { return errors.New("hub never answered") })
	u.cfg.Handover = func(stop bool) { handovers = append(handovers, stop) }
	rel, _ := u.Check(context.Background())
	if err := u.Apply(context.Background(), rel); err == nil || errors.Is(err, ErrRestart) {
		t.Fatalf("apply: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary bytes" {
		t.Fatal("the executable changed after a failed probation")
	}
	if len(handovers) != 2 || handovers[0] != true || handovers[1] != false {
		t.Fatalf("handovers %v", handovers)
	}
	// The failed version is remembered and not retried.
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("second check: %v", err)
	}
	u2, _ := newUpdater(t, r, dir, nil)
	if _, err := u2.Check(context.Background()); err == nil {
		t.Fatal("a restart forgot the failed version")
	}
}

func TestIndexRollbackAndBadSignature(t *testing.T) {
	r := newFakeRepo(t, 5, "3.0.1")
	dir := t.TempDir()
	u, _ := newUpdater(t, r, dir, nil)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	older := newFakeRepo(t, 4, "3.0.2")
	older.key = r.key
	sig, _ := release.Sign(r.key, older.index, time.Now())
	older.sig, _ = sig.Encode()
	u.cfg.Fetch = older.fetch
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("rollback: %v", err)
	}
	rogue := newFakeRepo(t, 6, "3.0.9") // signed by another key
	u.cfg.Fetch = rogue.fetch
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("an index signed by a stranger was accepted")
	}
	if u.Status().LastRevision != 5 {
		t.Fatal("a refused index moved the revision")
	}
}

func TestSigURL(t *testing.T) {
	if sigURL("https://x/releases/index.yaml") != "https://x/releases/index.sig" {
		t.Fatal(sigURL("https://x/releases/index.yaml"))
	}
}
