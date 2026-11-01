// Package version holds what the release workflow stamps into a build with
// -ldflags "-X": the version, the release key that signs the release index,
// and where the index is published.
package version

// Version is "dev" in a plain build.
var Version = "dev"

// ReleaseKey is the base32 public half of the key that signs the release
// index. Empty in a plain build, which disables self-update.
var ReleaseKey = ""

// ReleaseIndexURL is where the signed release index is fetched from. A fork
// that builds its own binary sets its own.
var ReleaseIndexURL = "https://raw.githubusercontent.com/adefilippo83/redundanet/main/releases/index.yaml"
