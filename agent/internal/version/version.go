// Package version holds the build's version string, set by the release
// workflow through -ldflags "-X .../version.Version=3.0.0".
package version

// Version is "dev" in a plain build.
var Version = "dev"
