#!/usr/bin/env bash
# RedundaNet 3 agent installer for a Linux node, run as root.
#
#   install.sh prepare VERSION        user, directories, binary, unit; prints the device key
#   install.sh pair                   shows a pairing code; a device of the member on this network certifies the node, then it joins and starts
#   install.sh finish CERTIFICATE     accepts the member's certificate made on another machine, joins, starts
#   install.sh storage                grants the store under STORAGE_PATH to a node installed earlier
#
# The short way since 3.0.4 is `redundanet node install`, which the one-line
# installer runs: curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage
# This script makes the same layout step by step and keeps the certificate
# flow. prepare downloads the release named VERSION from GitHub and checks
# it against the sha256 in releases/index.yaml on main. pair needs a device
# of the member that can reach this node (`redundanet device pair --storage`
# there, with the phrase); finish needs the file the member made with
# `redundanet identity certify` instead, for a node reached over SSH only.
# The network is built into the binary (3.0.6 and later): finish joins it
# with no descriptor to fetch, and the hints below name --network only for
# another one.
#
# An image build (rpi-image/) runs prepare with ARCHIVE=<file> to install an
# archive it verified itself, since the signed index lists a release only
# once the maintainer has signed it, and NO_DEVICE_KEY=1 to stop before the
# device key, which every booted image must make for itself.
set -euo pipefail

REPO=${REPO:-adefilippo83/redundanet}
NETWORK=${NETWORK:-redundanet}
CHANNEL=${CHANNEL:-stable}
STORAGE_PATH=${STORAGE_PATH:-/mnt/storage}
DATA=/var/lib/redundanet3
BIN=$DATA/bin/redundanet
UNIT=/etc/systemd/system/redundanet-agent.service
ENV=/etc/redundanet3/agent.env
RAW=https://raw.githubusercontent.com/$REPO/main
here=$(cd "$(dirname "$0")" && pwd)
# netflag goes into the commands printed for the member's laptop: nothing
# for the network built into the binary, the flag for any other.
netflag=""; [ "$NETWORK" = redundanet ] || netflag="--network $NETWORK "

die() { echo "install.sh: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root"

arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
}

# storage_grant lets the confined service write its store under the storage
# disk: the redundanet3 directory, owned by the service user, and a drop-in
# that adds it to the unit's writable paths. Idempotent; `install.sh storage`
# applies it to a node installed before the store existed.
storage_grant() {
  local root="$STORAGE_PATH/redundanet3"
  [ -d "$STORAGE_PATH" ] || die "$STORAGE_PATH is not a directory"
  install -d -o redundanet3 -g redundanet3 -m 0750 "$root"
  install -d -m 0755 "$UNIT.d"
  printf '[Service]\nReadWritePaths=%s\n' "$root" > "$UNIT.d/storage.conf"
  echo "store directory $root and unit drop-in in place"
}

prepare() {
  local version=${1:-}; [ -n "$version" ] || die "prepare needs a version, for example 3.0.0-alpha.2"
  local target="linux/$(arch)" asset="redundanet-$version-linux-$(arch).tar.gz"
  id redundanet3 >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin redundanet3
  install -d -o redundanet3 -g redundanet3 -m 0750 "$DATA" "$DATA/bin"
  install -d -m 0755 /etc/redundanet3

  local tmp; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  if [ -n "${ARCHIVE:-}" ]; then
    [ -f "$ARCHIVE" ] || die "ARCHIVE $ARCHIVE is not a file"
    cp "$ARCHIVE" "$tmp/$asset"
  else
    curl -fsSL -o "$tmp/index.yaml" "$RAW/releases/index.yaml"
    local sha
    sha=$(awk -v t="$target" -v v="$version" '
      $1=="-" && $2=="version:" {cur=$3}
      cur==v && $1==t":" { if (match($0, /sha256: [0-9a-f]{64}/)) print substr($0, RSTART+8, 64) }' "$tmp/index.yaml")
    [ -n "$sha" ] || die "releases/index.yaml on main does not list $version for $target"
    curl -fsSL -o "$tmp/$asset" "https://github.com/$REPO/releases/download/agent%2Fv$version/$asset"
    echo "$sha  $tmp/$asset" | sha256sum -c >/dev/null || die "checksum mismatch for $asset"
  fi
  tar -xzf "$tmp/$asset" -C "$tmp"
  install -o redundanet3 -g redundanet3 -m 0755 "$(find "$tmp" -type f -name redundanet | head -1)" "$BIN"
  echo "installed $("$BIN" version) at $BIN"

  if [ ! -f "$ENV" ]; then
    printf 'NETWORK=%s\nCHANNEL=%s\nSTORAGE_PATH=%s\n' "$NETWORK" "$CHANNEL" "$STORAGE_PATH" > "$ENV"
    chmod 0644 "$ENV"
  fi
  install -m 0644 "$here/redundanet-agent.service" "$UNIT"
  storage_grant
  systemctl daemon-reload 2>/dev/null || true # no systemd runs inside an image build's chroot
  # quic-go asks for 7 MiB socket buffers; an unprivileged process only gets them if the kernel allows
  if [ ! -f /etc/sysctl.d/90-redundanet-agent.conf ]; then
    printf '# RedundaNet 3 agent: let quic-go size its UDP socket buffers (7 MiB)\nnet.core.rmem_max = 7500000\nnet.core.wmem_max = 7500000\n' > /etc/sysctl.d/90-redundanet-agent.conf
    sysctl -q -p /etc/sysctl.d/90-redundanet-agent.conf || true
  fi

  if [ -f "$DATA/identity" ]; then
    echo "this node already has an identity; nothing more to prepare"
    return
  fi
  if [ "${NO_DEVICE_KEY:-}" = 1 ]; then
    echo "prepared without a device key; the first boot makes one"
    return
  fi
  echo
  echo "== device key of this node (certify it on your laptop):"
  sudo -u redundanet3 "$BIN" identity device-key --network "$NETWORK" --data-dir "$DATA"
  echo
  echo "on the laptop:  redundanet identity certify ${netflag}--device <KEY> --storage > $(hostname).cert"
  echo "then here:      install.sh finish $(hostname).cert"
}

# pair: the node shows a one-time code and waits; the member's other device
# certifies it through the code and hands over the identity and the network,
# so nothing is copied by hand. The code's TCP port must be reachable from
# that device for the minutes of pairing (a host firewall would block it).
pair() {
  [ -x "$BIN" ] || die "run prepare first"
  [ -f "$ENV" ] && . "$ENV"
  [ ! -f "$DATA/identity" ] || die "this node already has an identity"
  echo "On a device of yours that has its identity, with the phrase at hand (on this network or anywhere: the hub relays the exchange):"
  echo "  redundanet device pair ${netflag}--storage <the code shown below>"
  echo
  sudo -u redundanet3 "$BIN" identity pair --network "$NETWORK" --data-dir "$DATA" --wait "${PAIR_WAIT:-10m}" || die "pairing did not complete; run install.sh pair again for a new code, or use finish with a certificate file"
  systemctl enable --now redundanet-agent
  sleep 3
  systemctl --no-pager --lines=5 status redundanet-agent || true
  echo
  echo "the agent as it sees itself: sudo $BIN status"
}

finish() {
  local cert=${1:-}; [ -f "$cert" ] || die "finish needs the certificate file"
  [ -x "$BIN" ] || die "run prepare first"
  local tmp; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  install -o redundanet3 -g redundanet3 -m 0600 "$cert" "$tmp/cert"
  chown redundanet3 "$tmp"
  sudo -u redundanet3 "$BIN" identity accept-certificate --network "$NETWORK" --data-dir "$DATA" "$tmp/cert"
  shred -u "$cert" 2>/dev/null || rm -f "$cert"
  # The descriptor is built into the binary; an older binary needs it spelled out.
  sudo -u redundanet3 "$BIN" network join --data-dir "$DATA" || {
    curl -fsSL -o "$tmp/manifest.yaml" "$RAW/manifests/manifest.yaml"
    local desc
    desc=$(sudo -u redundanet3 "$BIN" network descriptor --manifest "$tmp/manifest.yaml" --repository "$RAW")
    sudo -u redundanet3 "$BIN" network join --data-dir "$DATA" "$desc"
  }
  systemctl enable --now redundanet-agent
  sleep 3
  systemctl --no-pager --lines=5 status redundanet-agent || true
  echo
  echo "the agent as it sees itself: sudo $BIN status"
}

case "${1:-}" in
  prepare) prepare "${2:-}" ;;
  pair) pair ;;
  finish) finish "${2:-}" ;;
  storage) [ -f "$ENV" ] && . "$ENV"; storage_grant; systemctl daemon-reload; systemctl restart redundanet-agent; echo "agent restarted with the store" ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
