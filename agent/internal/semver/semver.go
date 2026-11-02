// Package semver parses and orders RedundaNet 3 version strings: Semantic
// Versioning 2.0.0 without build metadata, so MAJOR.MINOR.PATCH with an
// optional pre-release suffix such as 3.0.0-alpha.1. A pre-release sorts
// before the release it precedes; its dot-separated identifiers compare
// numerically when numeric and by ASCII otherwise, numeric ones first, and a
// longer list wins when every shared identifier ties. Build metadata (+...)
// is refused: a release is one build.
package semver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var re = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$`)

// Version is a parsed version.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string // pre-release identifiers; empty for a release
}

// Parse parses s or reports why it is not a version.
func Parse(s string) (Version, error) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH with an optional pre-release suffix", s)
	}
	var v Version
	var err error
	if v.Major, err = strconv.ParseUint(m[1], 10, 64); err != nil {
		return Version{}, fmt.Errorf("version %q: %w", s, err)
	}
	if v.Minor, err = strconv.ParseUint(m[2], 10, 64); err != nil {
		return Version{}, fmt.Errorf("version %q: %w", s, err)
	}
	if v.Patch, err = strconv.ParseUint(m[3], 10, 64); err != nil {
		return Version{}, fmt.Errorf("version %q: %w", s, err)
	}
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
	}
	return v, nil
}

// Valid reports whether s is a version.
func Valid(s string) bool {
	_, err := Parse(s)
	return err == nil
}

// String formats v as it was parsed.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Compare orders a and b as -1, 0 or 1.
func Compare(a, b Version) int {
	for _, p := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		switch {
		case p[0] < p[1]:
			return -1
		case p[0] > p[1]:
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdentifier(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

func compareIdentifier(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case errA == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// Less reports whether version string a sorts before b. A string that is not
// a version sorts before every version and ties with any other such string,
// so a "dev" build sees every release as newer and never looks newer itself.
func Less(a, b string) bool {
	va, errA := Parse(a)
	vb, errB := Parse(b)
	switch {
	case errA != nil && errB != nil:
		return false
	case errA != nil:
		return true
	case errB != nil:
		return false
	}
	return Compare(va, vb) < 0
}
