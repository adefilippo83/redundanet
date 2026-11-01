#!/bin/bash
# Cross-compile every spike binary into build/<os>-<arch>/.
set -euo pipefail
cd "$(dirname "$0")"
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do
  os=${target%/*}; arch=${target#*/}
  out="build/$os-$arch"; mkdir -p "$out"
  ext=""; [ "$os" = windows ] && ext=.exe
  for cmd in hub agent natprobe cryptobench; do
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags=-s\ -w -o "$out/$cmd$ext" "./cmd/$cmd"
  done
  echo "$out: $(ls "$out" | tr '\n' ' ')"
done
