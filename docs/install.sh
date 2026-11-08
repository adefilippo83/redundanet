#!/bin/sh
# The RedundaNet 3 installer, served at https://redundanet.com/install.sh
# (the file docs/install.sh in the repository).
#
#   curl -fsSL https://redundanet.com/install.sh | sh
#       a laptop or desktop: installs the newest stable release as
#       bin/redundanet in your data directory, where the agent can replace
#       it when it updates itself, links /usr/local/bin/redundanet to it,
#       installs the agent as a service that starts at login, and opens its
#       page in your browser, which walks you through the rest
#
#   curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage
#       a Linux storage node, as root: downloads the same way, then runs
#       `redundanet node install`, which lays the node out under
#       /var/lib/redundanet3 and shows a pairing code
#
# Options: --channel early takes the newest release of either channel,
# --version X.Y.Z that release, --bin PATH puts the link there instead of
# /usr/local/bin/redundanet ($REDUNDANET_BIN does the same).
#
# Where things go. The data directory is the agent's own: on macOS
# ~/Library/Application Support/redundanet, on Linux ~/.config/redundanet
# (or $XDG_CONFIG_HOME/redundanet), or $REDUNDANET_DATA_DIR. The binary
# lives in its bin/ because the agent updates itself by renaming a new
# version into place, which a directory owned by root would refuse. The
# link on the PATH needs sudo once and never changes afterwards.
#
# What it trusts. The release index, releases/index.yaml on the main
# branch, names every release's archives with their sha256 and size, newest
# first. The script fetches it over HTTPS, downloads the archive for this
# machine and refuses one whose hash or size differs. The index is signed by
# the release key built into every redundanet binary, and the installed
# binary checks that signature itself as the last step, so a tampered index
# is caught here, as it would be by the agent's own updater. To do the same
# by hand, the download page lists the archives and docs/install.md the
# commands.
#
# The whole script is one function called on the last line, so a partial
# download runs nothing.
set -eu

REPO=${REDUNDANET_REPO:-adefilippo83/redundanet}
INDEX=${REDUNDANET_INDEX:-https://raw.githubusercontent.com/$REPO/main/releases/index.yaml}
BIN=${REDUNDANET_BIN:-/usr/local/bin/redundanet}

usage() {
  cat <<'TXT'
usage: install.sh [--channel stable|early] [--version X.Y.Z] [--bin PATH]
       install.sh --node STORAGE_PATH [--channel stable|early] [--version X.Y.Z]   (Linux, as root)

  curl -fsSL https://redundanet.com/install.sh | sh
  curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage
TXT
}

die() { echo "install.sh: $*" >&2; exit 1; }
note() { echo "$*" >&2; }

# datadir prints the agent's data directory for this user, as the binary
# computes it (Go's os.UserConfigDir plus "redundanet").
datadir() {
  if [ -n "${REDUNDANET_DATA_DIR:-}" ]; then echo "$REDUNDANET_DATA_DIR"; return; fi
  case "$(uname -s)" in
    Darwin) echo "$HOME/Library/Application Support/redundanet" ;;
    *) echo "${XDG_CONFIG_HOME:-$HOME/.config}/redundanet" ;;
  esac
}

need() {
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 || die "$t is needed and not installed"
  done
}

# target prints linux/amd64 and the like, or stops on a platform the
# releases do not cover.
target() {
  os=$(uname -s); arch=$(uname -m)
  case "$os" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT) die "on Windows, in PowerShell:  irm https://redundanet.com/install.ps1 | iex" ;;
    *) die "no release for $os" ;;
  esac
  case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "no release for $os on $arch" ;;
  esac
  echo "$os/$arch"
}

# pick reads the index on stdin and prints "version url sha256 size" for the
# release to install: the one named by want, or the newest one of the
# channel (stable takes stable releases, early takes either) that is not
# withdrawn and has an archive for the target. Nothing printed means none.
pick() {
  awk -v target="$1" -v channel="$2" -v want="$3" '
    function flush() {
      if (done || v == "") return
      if (want != "") { if (v != want) return }
      else { if (wd == "true") return; if (ch != "stable" && channel != "early") return }
      if (url != "" && sha != "" && size != "") { print v, url, sha, size; done = 1 }
      else if (want != "") { done = 1 }
    }
    /^  - version:/ { flush(); v = $3; ch = ""; wd = ""; url = ""; sha = ""; size = ""; next }
    v != "" && /^    channel:/ { ch = $2; next }
    v != "" && /^    withdrawn:/ { wd = $2; next }
    v != "" && $1 == target ":" {
      if (match($0, /url: [^,} ]+/)) url = substr($0, RSTART + 5, RLENGTH - 5)
      if (match($0, /sha256: [0-9a-f]+/)) sha = substr($0, RSTART + 8, RLENGTH - 8)
      if (match($0, /size: [0-9]+/)) size = substr($0, RSTART + 6, RLENGTH - 6)
      next
    }
    END { flush() }'
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | awk '{print $NF}'
  else die "no sha256sum, shasum or openssl to check the download with"
  fi
}

main() {
  channel=stable; want=""; node=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --channel) [ $# -ge 2 ] || die "--channel needs stable or early"; channel=$2; shift 2 ;;
      --version) [ $# -ge 2 ] || die "--version needs X.Y.Z"; want=${2#v}; shift 2 ;;
      --bin) [ $# -ge 2 ] || die "--bin needs a path"; BIN=$2; shift 2 ;;
      --node) [ $# -ge 2 ] || die "--node needs the storage path, for example --node /mnt/storage"; node=$2; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) usage >&2; die "unknown argument $1" ;;
    esac
  done
  case "$channel" in stable|early) ;; *) die "--channel takes stable or early, not $channel" ;; esac
  need curl tar uname mktemp awk
  tgt=$(target)
  if [ -n "$node" ]; then
    [ "${tgt%%/*}" = linux ] || die "a storage node is a Linux machine; on this computer run the script without --node"
    [ "$(id -u)" = 0 ] || die "--node runs as root: curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node $node"
  fi

  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT INT TERM
  curl -fsSL -o "$tmp/index.yaml" "$INDEX" || die "could not fetch the release index at $INDEX"
  set -- $(pick "$tgt" "$channel" "$want" < "$tmp/index.yaml")
  if [ $# -ne 4 ]; then
    [ -n "$want" ] && die "the release index does not list $want for $tgt"
    die "the release index lists no $channel release for $tgt"
  fi
  version=$1; url=$2; sha=$3; size=$4
  [ ${#sha} -eq 64 ] || die "the index entry for $version has no usable sha256"
  name=${url##*/}

  real="$(datadir)/bin/redundanet"
  if [ -z "$node" ] && [ -x "$real" ] && [ "$("$real" version 2>/dev/null | awk '{print $2}')" = "$version" ]; then
    note "redundanet $version is already at $real; the agent keeps itself up to date from here"
  else
    note "downloading redundanet $version for $tgt"
    curl -fsSL -o "$tmp/$name" "$url" || die "could not download $url"
    got=$(wc -c < "$tmp/$name" | tr -d ' ')
    [ "$got" = "$size" ] || die "$name is $got bytes, the index says $size: refusing it"
    gotsha=$(sha256 "$tmp/$name")
    [ "$gotsha" = "$sha" ] || die "$name has sha256 $gotsha, the index says $sha: refusing it"
    mkdir "$tmp/x" && tar -xzf "$tmp/$name" -C "$tmp/x"
    bin=$(find "$tmp/x" -type f -name redundanet | head -n 1)
    [ -n "$bin" ] || die "$name holds no redundanet binary"
    chmod 0755 "$bin"
    "$bin" version >/dev/null 2>&1 || die "the downloaded binary does not run on this machine"

    if [ -n "$node" ]; then
      # node install copies the binary under /var/lib/redundanet3 itself,
      # lays the node out and pairs it; it prints what comes next.
      set -- --storage-path "$node"
      [ "$channel" = stable ] || set -- "$@" --channel "$channel"
      "$bin" node install "$@"
      return
    fi

    # The binary goes under the data directory, ours to write, so the
    # agent can replace it when it updates itself; a running one is
    # renamed aside first, as the updater does.
    mkdir -p "$(dirname "$real")"
    cp "$bin" "$real.new" && chmod 0755 "$real.new"
    [ -e "$real" ] && mv -f "$real" "$real.old"
    mv -f "$real.new" "$real"
    note "installed redundanet $version at $real"
  fi

  # The link on the PATH: directly when the directory is ours to write,
  # else with sudo, which asks for the password on the terminal.
  if [ "$(readlink "$BIN" 2>/dev/null)" != "$real" ]; then
    dir=$(dirname "$BIN")
    sudo=""
    if ! { mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ]; }; then
      [ "$(id -u)" = 0 ] && die "cannot write to $dir"
      command -v sudo >/dev/null 2>&1 || die "cannot write to $dir and sudo is not installed; put the link elsewhere with --bin PATH, or use $real as it is"
      note "linking $BIN to it needs administrator rights (sudo)"
      sudo=sudo
      $sudo mkdir -p "$dir"
    fi
    $sudo ln -sfn "$real" "$BIN"
    note "$BIN points to it"
  fi
  installed=$BIN

  # The binary checks the index's signature with the release key built into
  # it, the same check its updater makes every hour.
  if out=$("$installed" update check --channel "$channel" 2>&1 </dev/null); then
    note "the signed release index verifies: $out"
  else
    note "warning: could not verify the signed release index with the installed binary: $out"
  fi
  case ":$PATH:" in
    *":$(dirname "$installed"):"*) cmd=redundanet ;;
    *) cmd=$installed; note "note: $(dirname "$installed") is not on your PATH; the commands below use the full path" ;;
  esac

  # The agent as a service that starts at login: without an identity it
  # serves its page alone, with the setup, and the page walks the member
  # through the rest. A service installed earlier is left as it is.
  if "$installed" service status 2>/dev/null | grep -q '"installed": true'; then
    note "a service is installed already; $cmd service install points it at this binary if it runs another"
  else
    set -- service install
    [ "$channel" = stable ] || set -- "$@" --channel "$channel"
    if "$installed" "$@" >/dev/null 2>&1 </dev/null; then
      note "the agent runs as a service that starts at login"
    else
      note "warning: the service could not be installed; $cmd service install says why"
    fi
  fi
  # The page: the agent needs a moment to come up.
  n=0
  while [ $n -lt 15 ]; do
    if link=$("$installed" open 2>/dev/null </dev/null | head -n 1) && [ -n "$link" ]; then
      break
    fi
    link=""; n=$((n + 1)); sleep 1
  done
  if [ -n "$link" ]; then
    cat <<TXT

the page is open in your browser and walks you through the rest: your identity,
then the join request. If it did not open, this link does, once, for two minutes:
  $link
A new link any time: $cmd open. In a terminal instead: $cmd setup
TXT
  else
    cat <<TXT

next, on this computer:
  $cmd open                 # the page, which walks you through identity and join request
  $cmd setup                # the same, in this terminal
TXT
  fi
}

main "$@"
