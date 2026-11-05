package semver

import (
	"sort"
	"testing"
)

func TestValid(t *testing.T) {
	for _, s := range []string{"0.0.0", "3.0.0", "3.10.0", "3.0.0-alpha.1", "3.0.0-rc.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92", "1.0.0-x-y-z.-"} {
		if !Valid(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "dev", "v3.0.0", "3.0", "3.0.0.1", "03.0.0", "3.0.0-", "3.0.0-01", "3.0.0-alpha..1", "3.0.0+build", "3.0.0-alpha+1", " 3.0.0", "3.0.0 "} {
		if Valid(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
	v, err := Parse("3.0.0-alpha.1")
	if err != nil || v.Major != 3 || v.Minor != 0 || v.Patch != 0 || len(v.Pre) != 2 || v.Pre[0] != "alpha" || v.Pre[1] != "1" || v.String() != "3.0.0-alpha.1" {
		t.Fatalf("parse: %+v %v", v, err)
	}
}

func TestOrder(t *testing.T) {
	// The precedence example of the Semantic Versioning specification, with
	// the numeric-identifier and width cases this project relies on.
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.2", "1.0.0-alpha.10", "1.0.0-alpha.beta",
		"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0",
		"2.0.0", "3.0.0-alpha.1", "3.0.0", "3.9.0", "3.10.0",
	}
	for i := range ordered {
		for j := range ordered {
			if got, want := Less(ordered[i], ordered[j]), i < j; got != want {
				t.Errorf("Less(%q, %q) = %v, want %v", ordered[i], ordered[j], got, want)
			}
		}
	}
	shuffled := append([]string{}, ordered...)
	for i, j := range []int{5, 0, 16, 9, 2, 12, 1, 15, 3, 8, 14, 4, 11, 6, 13, 7, 10} {
		shuffled[i] = ordered[j]
	}
	sort.Slice(shuffled, func(i, j int) bool { return Less(shuffled[i], shuffled[j]) })
	for i := range ordered {
		if shuffled[i] != ordered[i] {
			t.Fatalf("sorted order differs at %d: %v", i, shuffled)
		}
	}
}

func TestLessWithNonVersions(t *testing.T) {
	if !Less("dev", "3.0.0") || Less("3.0.0", "dev") || Less("dev", "x") || Less("3.0.0", "3.0.0") {
		t.Fatal("non-versions must sort below every version and tie with each other")
	}
}
