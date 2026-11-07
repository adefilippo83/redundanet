package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// The index's two lines AddFragment rewrites; everything else stays byte
// for byte, comments included.
var (
	revisionLine = regexp.MustCompile(`(?m)^revision: ([0-9]+)$`)
	releasesLine = regexp.MustCompile(`(?m)^releases:\n`)
)

// ParseFragment reads the index fragment the release workflow attaches to
// a release: one entry of the releases list, as it goes into the index.
func ParseFragment(fragment []byte) (*Release, error) {
	var list []Release
	if err := yaml.Unmarshal(fragment, &list); err != nil {
		return nil, fmt.Errorf("index fragment: %w", err)
	}
	if len(list) != 1 {
		return nil, fmt.Errorf("index fragment: %d releases, one expected", len(list))
	}
	r := list[0]
	if len(r.Assets) == 0 {
		return nil, fmt.Errorf("index fragment: %s lists no asset", r.Version)
	}
	return &r, nil
}

// AddFragment puts a release's fragment at the top of the releases list
// and raises the revision by one, as text, then proves the result: it
// parses, it is the old index plus that one release, nothing else moved.
func AddFragment(index, fragment []byte) ([]byte, *Release, error) {
	if bytes.Contains(index, []byte("\r\n")) || bytes.Contains(fragment, []byte("\r\n")) {
		return nil, nil, errors.New("Windows line endings in the index or the fragment: the signed file must be the bytes git stores, with plain newlines (see .gitattributes)")
	}
	old, err := Parse(index)
	if err != nil {
		return nil, nil, err
	}
	rel, err := ParseFragment(fragment)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range old.Releases {
		if r.Version == rel.Version {
			return nil, nil, fmt.Errorf("%s is already in the index (revision %d)", rel.Version, old.Revision)
		}
	}
	if len(revisionLine.FindAll(index, -1)) != 1 || len(releasesLine.FindAll(index, -1)) != 1 {
		return nil, nil, errors.New("the index does not have one `revision:` and one `releases:` line at the top level")
	}
	frag := bytes.TrimRight(fragment, "\n")
	out := releasesLine.ReplaceAllLiteral(index, append(append([]byte("releases:\n"), frag...), '\n'))
	out = revisionLine.ReplaceAll(out, []byte("revision: "+strconv.FormatUint(old.Revision+1, 10)))

	got, err := Parse(out)
	if err != nil {
		return nil, nil, fmt.Errorf("the new index does not parse: %w", err)
	}
	switch {
	case got.Revision != old.Revision+1:
		return nil, nil, fmt.Errorf("the new index has revision %d, %d expected", got.Revision, old.Revision+1)
	case len(got.Releases) != len(old.Releases)+1 || !reflect.DeepEqual(got.Releases[0], *rel):
		return nil, nil, errors.New("the new index's first release is not the fragment")
	case !reflect.DeepEqual(got.Releases[1:], old.Releases) || !reflect.DeepEqual(got.Rotations, old.Rotations):
		return nil, nil, errors.New("adding the fragment changed other entries of the index")
	}
	return out, rel, nil
}

// CheckAssets proves every asset of a release against files downloaded
// into dir: each named by the last part of its URL, with the listed size
// and sha256. It is the check that the archives on the release are the
// ones the index will vouch for.
func (r *Release) CheckAssets(dir string) error {
	for target, a := range r.Assets {
		name := path.Base(a.URL)
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("%s (%s): %w", name, target, err)
		}
		if int64(len(data)) != a.Size {
			return fmt.Errorf("%s (%s): %d bytes, the fragment says %d", name, target, len(data), a.Size)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			return fmt.Errorf("%s (%s): sha256 %s, the fragment says %s", name, target, hex.EncodeToString(sum[:]), a.SHA256)
		}
	}
	return nil
}
