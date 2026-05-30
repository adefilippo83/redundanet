#!/usr/bin/env bash
#
# Smoke test for the Raspberry Pi image provisioning.
#
# The real image build (.github/workflows/build-rpi-image.yml) takes ~2h under
# QEMU because it rewrites a full Raspberry Pi OS .img. This test instead
# exercises the *provisioning logic* the image relies on, on the same CPU
# architecture (arm64) and OS family (Debian Bookworm == Raspberry Pi OS
# Bookworm), in a couple of minutes:
#
#   1. Validate the systemd unit files are structurally sound
#   2. Build the redundanet wheel from the current source
#   3. In an arm64 container: create the /opt/redundanet venv, install the CLI
#      (mirroring install-redundanet.sh), and run it (--version/--help/validate)
#   4. Run rpi-image first-boot.sh and assert it generates the node identity
#      and config the same way the booted Pi would
#
# Runs natively on Apple Silicon; in CI it uses QEMU (docker/setup-qemu-action).
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PLATFORM="linux/arm64"
IMAGE="python:3.11-slim-bookworm"          # matches Raspberry Pi OS Bookworm + venv py3.11
RPI="rpi-image/stage-redundanet"
UNIT_DIR="$RPI/02-configure-services/files"
FIRST_BOOT="$RPI/03-first-boot/files/first-boot.sh"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }

# ----------------------------------------------------------------------------
# 1. Validate systemd unit files (structural)
# ----------------------------------------------------------------------------
log "Step 1/4 — Validating systemd unit files"
for unit in "$UNIT_DIR"/*.service; do
  for section in '[Unit]' '[Service]' '[Install]'; do
    grep -qF "$section" "$unit" || { fail "$(basename "$unit") missing $section"; exit 1; }
  done
  grep -qE '^ExecStart=' "$unit" || { fail "$(basename "$unit") missing ExecStart="; exit 1; }
  ok "$(basename "$unit") is structurally valid"
done

# ----------------------------------------------------------------------------
# 2. Build the wheel from current source
# ----------------------------------------------------------------------------
log "Step 2/4 — Building redundanet wheel from source"
rm -rf dist
poetry build -f wheel >/dev/null
WHEEL_PATH="$(ls dist/*.whl | head -1)"
WHEEL="$(basename "$WHEEL_PATH")"
ok "Built $WHEEL"

# ----------------------------------------------------------------------------
# 3 + 4. arm64 provisioning: install CLI + run first-boot
# ----------------------------------------------------------------------------
log "Step 3/4 — Provisioning + CLI smoke on $PLATFORM ($IMAGE)"

# If we are not on arm64 and arm64 emulation isn't registered, register binfmt.
if [ "$(uname -m)" != "arm64" ] && [ "$(uname -m)" != "aarch64" ]; then
  if ! docker run --rm --platform "$PLATFORM" "$IMAGE" true >/dev/null 2>&1; then
    log "Registering QEMU arm64 emulation (binfmt)"
    docker run --privileged --rm tonistiigi/binfmt --install arm64 >/dev/null 2>&1 || true
  fi
fi

docker run --rm --platform "$PLATFORM" \
  -v "$REPO_ROOT:/work:ro" \
  -e WHEEL="$WHEEL" \
  "$IMAGE" bash -euo pipefail -c '
    echo "--- container arch: $(uname -m) ---"

    # Install the build-relevant subset of rpi-image 00-packages (so pip can
    # build any C-extension deps, exactly as the real Pi image does) plus the
    # tools first-boot.sh expects. The full 00-packages list (docker, tinc, ...)
    # is system-level and not needed to smoke-test provisioning.
    apt-get update -qq >/dev/null
    apt-get install -y -qq --no-install-recommends \
      build-essential python3-dev libffi-dev libssl-dev \
      hostname iputils-ping >/dev/null

    # Mirror install-redundanet.sh: dedicated venv under /opt/redundanet
    INSTALL_DIR=/opt/redundanet
    VENV="$INSTALL_DIR/venv"
    mkdir -p "$INSTALL_DIR"
    python3 -m venv "$VENV"
    "$VENV/bin/pip" install --quiet --upgrade pip wheel setuptools
    "$VENV/bin/pip" install --quiet "/work/dist/$WHEEL"
    ln -sf "$VENV/bin/redundanet" /usr/local/bin/redundanet

    echo "== redundanet --version =="
    redundanet --version
    echo "== redundanet --help =="
    redundanet --help >/dev/null && echo "help OK"
    echo "== redundanet validate (example manifest) =="
    redundanet validate /work/manifests/example.yaml

    echo "== run first-boot.sh =="
    export REDUNDANET_CONFIG_DIR=/tmp/etc/redundanet
    export REDUNDANET_DATA_DIR=/tmp/var/lib/redundanet
    bash /work/'"$FIRST_BOOT"'

    echo "== verify first-boot artifacts =="
    NN="$REDUNDANET_CONFIG_DIR/node-name"
    CFG="$REDUNDANET_CONFIG_DIR/config.yaml"
    test -s "$NN"  || { echo "node-name not created"; exit 1; }
    test -s "$CFG" || { echo "config.yaml not created"; exit 1; }
    test -f "$REDUNDANET_CONFIG_DIR/.needs-setup" || { echo ".needs-setup marker missing"; exit 1; }
    NODE_NAME="$(cat "$NN")"
    grep -q "$NODE_NAME" "$CFG" || { echo "config.yaml does not reference node name"; exit 1; }
    echo "node-name: $NODE_NAME"
    echo "config.yaml references node name; .needs-setup present"
  '

ok "arm64 provisioning + CLI + first-boot smoke passed"

log "Step 4/4 — Done"
log "SUCCESS — Raspberry Pi image smoke test passed 🎉"
