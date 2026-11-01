package release

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Download fetches an asset into dir, checks its size and hash against the
// index, and extracts the `redundanet` executable from the archive into
// dir. It returns the executable's path.
func Download(ctx context.Context, a Asset, dir string, fetch func(ctx context.Context, url string, w io.Writer) error) (string, error) {
	if fetch == nil {
		fetch = httpFetch
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	archive := filepath.Join(dir, "asset")
	f, err := os.Create(archive)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	err = fetch(ctx, a.URL, io.MultiWriter(f, h))
	_ = f.Close()
	if err != nil {
		return "", err
	}
	info, err := os.Stat(archive)
	if err != nil {
		return "", err
	}
	if info.Size() != a.Size {
		return "", fmt.Errorf("asset is %d bytes, the index says %d", info.Size(), a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return "", errors.New("asset hash does not match the index")
	}
	exe := filepath.Join(dir, exeName())
	if err := Extract(archive, exe); err != nil {
		return "", err
	}
	_ = os.Remove(archive)
	return exe, nil
}

func exeName() string {
	if strings.HasSuffix(Target(), "windows/amd64") {
		return "redundanet.exe"
	}
	return "redundanet"
}

// Extract pulls the executable out of a release archive (tar.gz or zip, as
// the release workflow builds them) into dest, mode 0755. Paths inside the
// archive are not trusted: only a member whose base name is the executable
// is read, and nothing is written anywhere but dest.
func Extract(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	want := "redundanet"
	if head[0] == 'P' && head[1] == 'K' {
		return extractZip(archive, want+".exe", dest)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("archive is neither zip nor tar.gz: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errors.New("archive holds no redundanet executable")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != want {
			continue
		}
		return writeExe(dest, io.LimitReader(tr, 256<<20))
	}
}

func extractZip(archive, want, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || filepath.Base(zf.Name) != want {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeExe(dest, io.LimitReader(rc, 256<<20))
	}
	return errors.New("archive holds no redundanet executable")
}

func writeExe(dest string, r io.Reader) error {
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func httpFetch(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "redundanet/3")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// FetchBytes is the small-file fetch for the index and its signature.
func FetchBytes(ctx context.Context, url string) ([]byte, error) {
	var b strings.Builder
	if err := httpFetch(ctx, url, &b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
