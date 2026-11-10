#!/usr/bin/env bash
# Builds the redundanet binary for every release target, the same way
# wherever it runs: the release workflow calls it for a tag, and
# scripts/sign.sh calls the tag's own copy on the maintainer's laptop and
# compares what comes out with the archives on the release, binary for
# binary, before it signs. So nothing here may depend on the machine: no
# cgo, trimmed paths, an empty build id (the default one carries the path
# of the Windows resource file), the Go toolchain go.mod names, and no
# value from the environment or from repository settings. What a binary
# trusts (the release key, the index address, the network) is in
# agent/internal/version, in source.
#
#   scripts/build-agent.sh VERSION OUTDIR
#
# leaves OUTDIR/redundanet-VERSION-OS-ARCH/redundanet[.exe] for each target.
set -euo pipefail

version=${1:-}
out=${2:-}
[ -n "$version" ] && [ -n "$out" ] || { echo "usage: build-agent.sh VERSION OUTDIR" >&2; exit 2; }
mkdir -p "$out"
out=$(cd "$out" && pwd)
cd "$(dirname "$0")/../agent"

export CGO_ENABLED=0 GOFLAGS=-mod=readonly
GOTOOLCHAIN="go$(sed -n 's/^go //p' go.mod)"
export GOTOOLCHAIN

# Product name and version in the Windows executable's properties, which
# Windows shows and SignPath requires. The strings are in
# cmd/redundanet/versioninfo.json; the object is named for windows/amd64 so
# the other targets ignore it, and git ignores it, so the tree stays clean
# for the version control stamp.
go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.7.0 -64 \
  -file-version "$version" -product-version "$version" \
  -o cmd/redundanet/resource_windows_amd64.syso cmd/redundanet/versioninfo.json

flags="-s -w -buildid= -X github.com/adefilippo83/redundanet/agent/internal/version.Version=$version"
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do
  os=${target%/*}; arch=${target#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags="$flags" -o "$out/redundanet-$version-$os-$arch/redundanet$ext" ./cmd/redundanet
done
