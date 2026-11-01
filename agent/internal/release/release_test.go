package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func indexYAML(revision int, releases ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "revision: %d\nreleases:\n", revision)
	for _, r := range releases {
		parts := strings.Split(r, " ") // "version channel [withdrawn]"
		fmt.Fprintf(&b, "  - version: %s\n    channel: %s\n", parts[0], parts[1])
		if len(parts) > 2 {
			fmt.Fprintf(&b, "    withdrawn: true\n")
		}
		fmt.Fprintf(&b, "    assets:\n      %s: {url: https://example.org/%s.tar.gz, sha256: %s, size: 10}\n", Target(), parts[0], strings.Repeat("ab", 32))
	}
	return b.String()
}

func TestSignVerifyAndRotation(t *testing.T) {
	k1, _ := NewKey()
	k2, _ := NewKey()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	data := []byte(indexYAML(1, "3.0.0 stable"))
	sig, err := Sign(k1, data, now)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := sig.Encode()
	back, err := ParseSignature(enc)
	if err != nil {
		t.Fatal(err)
	}
	idx, signer, err := back.Verify(data, k1.Public(), now)
	if err != nil || signer != k1.Public() || idx.Revision != 1 {
		t.Fatalf("verify: %v", err)
	}
	if _, _, err := back.Verify(data, k2.Public(), now); err == nil {
		t.Fatal("verified against the wrong embedded key")
	}
	if _, _, err := back.Verify([]byte(indexYAML(1, "3.0.1 stable")), k1.Public(), now); err == nil {
		t.Fatal("a changed index verified")
	}

	// Rotation: k1 hands over to k2; an index signed by k2 verifies against
	// a binary that embeds k1, and k1 can no longer sign.
	rot, err := k1.SignRotation(k2.Public(), now)
	if err != nil {
		t.Fatal(err)
	}
	rotated := indexYAML(2, "3.0.0 stable", "3.0.1 stable") + fmt.Sprintf("rotations:\n  - new_key: %s\n    valid_from: %s\n    signature: %s\n", rot.NewKey, rot.ValidFrom.Format(time.RFC3339), rot.Signature)
	sig2, err := Sign(k2, []byte(rotated), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, signer, err := sig2.Verify([]byte(rotated), k1.Public(), now.Add(time.Hour)); err != nil || signer != k2.Public() {
		t.Fatalf("rotated index: %v", err)
	}
	sigOld, _ := Sign(k1, []byte(rotated), now.Add(time.Hour))
	if _, _, err := sigOld.Verify([]byte(rotated), k1.Public(), now.Add(time.Hour)); err == nil {
		t.Fatal("the retired key still signs")
	}
	// A rotation not signed by the retiring key is refused.
	k3, _ := NewKey()
	fake, _ := k3.SignRotation(k3.Public(), now)
	forged := indexYAML(3, "3.0.0 stable") + fmt.Sprintf("rotations:\n  - new_key: %s\n    valid_from: %s\n    signature: %s\n", fake.NewKey, fake.ValidFrom.Format(time.RFC3339), fake.Signature)
	sig3, _ := Sign(k3, []byte(forged), now)
	if _, _, err := sig3.Verify([]byte(forged), k1.Public(), now); err == nil {
		t.Fatal("a forged rotation was accepted")
	}
	// A rotation in the future does not apply yet.
	future := indexYAML(4, "3.0.0 stable") + fmt.Sprintf("rotations:\n  - new_key: %s\n    valid_from: %s\n    signature: %s\n", rot.NewKey, now.Add(48*time.Hour).Format(time.RFC3339), rot.Signature)
	sig4, _ := Sign(k1, []byte(future), now)
	if _, signer, err := sig4.Verify([]byte(future), k1.Public(), now); err != nil || signer != k1.Public() {
		t.Fatalf("future rotation applied early: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for name, y := range map[string]string{
		"no revision":  "releases: []\n",
		"bad version":  "revision: 1\nreleases:\n  - version: v3\n    channel: stable\n    assets: {}\n",
		"bad channel":  "revision: 1\nreleases:\n  - version: 3.0.0\n    channel: nightly\n    assets: {}\n",
		"asset no url": "revision: 1\nreleases:\n  - version: 3.0.0\n    channel: stable\n    assets:\n      linux/amd64: {sha256: " + strings.Repeat("ab", 32) + ", size: 1}\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestPick(t *testing.T) {
	idx, err := Parse([]byte(indexYAML(1, "3.0.0 stable", "3.0.1 stable", "3.1.0 early", "3.0.2 stable withdrawn", "3.2.0 early")))
	if err != nil {
		t.Fatal(err)
	}
	tgt := Target()
	if r := idx.Pick("3.0.0", ChannelStable, "", tgt); r == nil || r.Version != "3.0.1" {
		t.Fatalf("stable pick: %+v", r)
	}
	if r := idx.Pick("3.0.0", ChannelEarly, "", tgt); r == nil || r.Version != "3.2.0" {
		t.Fatalf("early pick: %+v", r)
	}
	if r := idx.Pick("3.0.1", ChannelStable, "", tgt); r != nil {
		t.Fatalf("nothing newer on stable, got %+v", r)
	}
	// min_version pulls a stable member onto an early release.
	if r := idx.Pick("3.0.1", ChannelStable, "3.1.0", tgt); r == nil || r.Version != "3.2.0" {
		t.Fatalf("required pick: %+v", r)
	}
	if r := idx.Pick("3.0.0", ChannelStable, "", "plan9/mips"); r != nil {
		t.Fatal("picked a release without an asset for the target")
	}
	if v := idx.Versions(); v[0] != "3.2.0" || v[len(v)-1] != "3.0.0" {
		t.Fatalf("versions: %v", v)
	}
	if !Less("3.9.0", "3.10.0") || Less("3.10.0", "3.9.0") {
		t.Fatal("version compare is lexical")
	}
}

func TestDownloadAndExtract(t *testing.T) {
	payload := []byte("#!/bin/sh\necho fake redundanet\n")
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "redundanet-3.0.1-linux-amd64/README", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.WriteHeader(&tar.Header{Name: "redundanet-3.0.1-linux-amd64/redundanet", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(tgz.Bytes())
	a := Asset{URL: "https://example.org/x.tar.gz", SHA256: hex.EncodeToString(sum[:]), Size: int64(tgz.Len())}
	serve := func(body []byte) func(context.Context, string, io.Writer) error {
		return func(_ context.Context, _ string, w io.Writer) error { _, err := w.Write(body); return err }
	}
	dir := filepath.Join(t.TempDir(), "3.0.1")
	exe, err := Download(context.Background(), a, dir, serve(tgz.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, payload) {
		t.Fatal("extracted the wrong bytes")
	}
	if info, _ := os.Stat(exe); runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatal("extracted executable is not executable") // Windows has no execute bit
	}
	if _, err := os.Stat(filepath.Join(dir, "asset")); err == nil {
		t.Fatal("archive left behind")
	}
	// Wrong hash and wrong size are refused.
	bad := a
	bad.SHA256 = strings.Repeat("00", 32)
	if _, err := Download(context.Background(), bad, filepath.Join(t.TempDir(), "x"), serve(tgz.Bytes())); err == nil {
		t.Fatal("a wrong hash was accepted")
	}
	bad = a
	bad.Size = a.Size + 1
	if _, err := Download(context.Background(), bad, filepath.Join(t.TempDir(), "y"), serve(tgz.Bytes())); err == nil {
		t.Fatal("a wrong size was accepted")
	}
	// Zip, as the Windows asset is built.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("redundanet-3.0.1-windows-amd64/redundanet.exe")
	_, _ = f.Write(payload)
	_ = zw.Close()
	zdest := filepath.Join(t.TempDir(), "redundanet.exe")
	_ = os.WriteFile(zdest+".zip", zb.Bytes(), 0o644)
	if err := Extract(zdest+".zip", zdest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(zdest); !bytes.Equal(got, payload) {
		t.Fatal("zip extraction")
	}
}
