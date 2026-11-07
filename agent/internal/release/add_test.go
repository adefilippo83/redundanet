package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseIndex = `# RedundaNet 3 release index. Signed by the release key (index.sig); see README.md.
revision: 29
releases:
  - version: 3.0.3
    channel: stable
    notes: https://example.org/3.0.3
    assets:
      linux/amd64: {url: https://example.org/redundanet-3.0.3-linux-amd64.tar.gz, sha256: 1111111111111111111111111111111111111111111111111111111111111111, size: 10}
`

func fragment(version, sha string, size int) string {
	return fmt.Sprintf(`  - version: %s
    channel: stable
    notes: https://example.org/%s
    assets:
      linux/amd64: {url: https://example.org/redundanet-%s-linux-amd64.tar.gz, sha256: %s, size: %d}
`, version, version, version, sha, size)
}

func TestAddFragmentInsertsAndRaisesTheRevision(t *testing.T) {
	out, rel, err := AddFragment([]byte(baseIndex), []byte(fragment("3.0.4", strings.Repeat("2", 64), 20)))
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "3.0.4" {
		t.Fatalf("release %s", rel.Version)
	}
	s := string(out)
	if !strings.HasPrefix(s, "# RedundaNet 3 release index.") || !strings.Contains(s, "\nrevision: 30\nreleases:\n  - version: 3.0.4\n") || !strings.HasSuffix(s, baseIndex[strings.Index(baseIndex, "  - version: 3.0.3"):]) {
		t.Fatalf("index:\n%s", s)
	}
	idx, err := Parse(out)
	if err != nil || idx.Revision != 30 || len(idx.Releases) != 2 || idx.Releases[0].Version != "3.0.4" {
		t.Fatalf("parsed: %+v %v", idx, err)
	}
}

func TestAddFragmentRefuses(t *testing.T) {
	cases := map[string]struct{ index, frag string }{
		"already listed":   {baseIndex, fragment("3.0.3", strings.Repeat("2", 64), 20)},
		"two releases":     {baseIndex, fragment("3.0.4", strings.Repeat("2", 64), 20) + fragment("3.0.5", strings.Repeat("3", 64), 20)},
		"no asset":         {baseIndex, "  - version: 3.0.4\n    channel: stable\n"},
		"bad sha":          {baseIndex, fragment("3.0.4", "abc", 20)},
		"bad channel":      {baseIndex, strings.Replace(fragment("3.0.4", strings.Repeat("2", 64), 20), "stable", "nightly", 1)},
		"no revision line": {strings.Replace(baseIndex, "revision: 29\n", "", 1), fragment("3.0.4", strings.Repeat("2", 64), 20)},
		"not a list":       {baseIndex, "version: 3.0.4\n"},
		"crlf index":       {strings.ReplaceAll(baseIndex, "\n", "\r\n"), fragment("3.0.4", strings.Repeat("2", 64), 20)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := AddFragment([]byte(c.index), []byte(c.frag)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCheckAssets(t *testing.T) {
	dir := t.TempDir()
	data := []byte("an archive's bytes")
	sum := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(dir, "redundanet-3.0.4-linux-amd64.tar.gz"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := ParseFragment([]byte(fragment("3.0.4", hex.EncodeToString(sum[:]), len(data))))
	if err != nil {
		t.Fatal(err)
	}
	if err := rel.CheckAssets(dir); err != nil {
		t.Fatal(err)
	}
	// One byte more, as when a second workflow run replaced the archives.
	if err := os.WriteFile(filepath.Join(dir, "redundanet-3.0.4-linux-amd64.tar.gz"), append(data, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rel.CheckAssets(dir); err == nil || !strings.Contains(err.Error(), "the fragment says") {
		t.Fatalf("a replaced archive: %v", err)
	}
	if err := rel.CheckAssets(t.TempDir()); err == nil {
		t.Fatal("a missing archive passed")
	}
}

// The live index takes the next release the way the workflow will add it.
func TestAddFragmentOnTheRealIndex(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("..", "..", "..", "releases", "index.yaml"))
	if err != nil {
		t.Skip("the repository's index is not beside the module")
	}
	idx, err := Parse(index)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := AddFragment(index, []byte(fragment("99.0.0", strings.Repeat("4", 64), 30)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(out)
	if err != nil || got.Revision != idx.Revision+1 || len(got.Releases) != len(idx.Releases)+1 {
		t.Fatalf("real index: %v", err)
	}
}
