#!/bin/bash
# NAT survey: build natprobe for the nodes, copy it over SSH, run it on the
# host (not in a container) and keep one JSON report per host in survey/.
#
#   ./survey.sh alessandro@falcon alessandro@falcon-1 ptlug@redundanet ...
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p build survey
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/natprobe-linux-amd64 ./cmd/natprobe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o build/natprobe-linux-arm64 ./cmd/natprobe
for host in "$@"; do
  arch=$(ssh -o ConnectTimeout=10 "$host" uname -m) || { echo "$host: ssh failed"; continue; }
  case "$arch" in
    x86_64) bin=natprobe-linux-amd64 ;;
    aarch64) bin=natprobe-linux-arm64 ;;
    *) echo "$host: unsupported arch $arch"; continue ;;
  esac
  scp -q "build/$bin" "$host:/tmp/natprobe"
  name=${host//[@:\/]/_}
  if ssh "$host" '/tmp/natprobe -json' > "survey/$name.json"; then
    python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print(f"{sys.argv[2]:28s} mapping: {r[\"mapping\"]:30s} filtering: {r[\"filtering\"]:28s} ipv6: {r[\"ipv6\"]}")' "survey/$name.json" "$host" 2>/dev/null || cat "survey/$name.json"
  else
    echo "$host: probe failed"
  fi
done
