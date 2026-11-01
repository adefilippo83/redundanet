package main

import (
	"runtime"

	"github.com/adefilippo83/redundanet/agent/internal/version"
)

func versionString() string {
	return "redundanet " + version.Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ", " + runtime.Version() + ")"
}
