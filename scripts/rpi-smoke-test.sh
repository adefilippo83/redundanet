#!/usr/bin/env bash
#
# Smoke test for the RedundaNet 3 Raspberry Pi image provisioning
# (rpi-image/, agent/deploy/node/install.sh). The real image build takes an
# hour or more under QEMU; this exercises what the image relies on, on the
# same CPU architecture and OS family (arm64, Debian Trixie), in minutes:
#
#   1. the unit files and drop-ins are structurally sound, the scripts parse,
#      and the image build carries the fstab line for the disk labeled REDUNDANET
#   2. the agent cross-builds for linux/arm64 and packs like a release archive
#   3. in an arm64 container: install.sh prepare with that archive and no
#      device key lays out the node; install.sh pair shows a code and gives
#      up cleanly when nobody comes; the first boot shows a pairing code on
#      the boot partition, the same one again after a restart, and no
#      identity appears; status.sh reports a paired node without its disk;
#      install.sh disk refuses what is not a block device
#
# Runs natively on Apple silicon; in CI under QEMU (docker/setup-qemu-action).
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
PLATFORM="linux/arm64"
IMAGE="debian:trixie-slim" # Raspberry Pi OS Trixie's family
VERSION=smoke

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ok\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  FAIL %s\033[0m\n' "$*" >&2; exit 1; }

log "1/3 units, drop-ins and scripts"
for unit in rpi-image/redundanet3-firstboot.service rpi-image/redundanet3-status.service agent/deploy/node/redundanet-agent.service; do
  grep -qF '[Unit]' "$unit" && grep -qF '[Service]' "$unit" && grep -qE '^ExecStart=' "$unit" || fail "$unit is not a service unit"
done
grep -qF '[Timer]' rpi-image/redundanet3-status.timer && grep -qE '^OnBootSec=' rpi-image/redundanet3-status.timer || fail "the status timer is not a timer unit"
grep -qE '^ConditionPathExists=!/var/lib/redundanet3/identity$' rpi-image/redundanet3-firstboot.service || fail "the first boot would run on a paired node"
grep -qE '^RequiresMountsFor=/mnt/storage$' rpi-image/agent-disk.conf && grep -qE '^ConditionPathIsMountPoint=/mnt/storage$' rpi-image/agent-disk.conf || fail "the agent drop-in does not bind the agent to the disk"
grep -qE '^ExecStartPre=\+' rpi-image/agent-disk.conf || fail "the agent drop-in does not make the store on the disk"
grep -qE '^Wants=redundanet-agent.service$' rpi-image/mount-agent.conf || fail "the mount drop-in does not start the agent"
grep -qF 'LABEL=REDUNDANET /mnt/storage' .github/workflows/build-rpi-image.yml || fail "the image build has no fstab line for the labeled disk"
for s in rpi-image/firstboot.sh rpi-image/status.sh agent/deploy/node/install.sh; do bash -n "$s" || fail "$s does not parse"; done
ok "three service units, one timer, two drop-ins, the fstab line, three scripts"

log "2/3 the agent for linux/arm64, packed like a release archive"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/redundanet-$VERSION-linux-arm64"
(cd agent && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X github.com/adefilippo83/redundanet/agent/internal/version.Version=$VERSION" -o "$WORK/redundanet-$VERSION-linux-arm64/redundanet" ./cmd/redundanet)
tar -C "$WORK" -czf "$WORK/redundanet-$VERSION-linux-arm64.tar.gz" "redundanet-$VERSION-linux-arm64"
cp agent/deploy/node/install.sh agent/deploy/node/redundanet-agent.service rpi-image/firstboot.sh rpi-image/status.sh "$WORK/"
ok "$(du -h "$WORK/redundanet-$VERSION-linux-arm64.tar.gz" | cut -f1) archive"

log "3/3 provisioning on $PLATFORM ($IMAGE)"
if [ "$(uname -m)" != "arm64" ] && [ "$(uname -m)" != "aarch64" ]; then
  docker run --rm --platform "$PLATFORM" "$IMAGE" true >/dev/null 2>&1 \
    || docker run --privileged --rm tonistiigi/binfmt --install arm64 >/dev/null 2>&1 || true
fi
docker run --rm --platform "$PLATFORM" -v "$WORK:/work:ro" -e VERSION="$VERSION" "$IMAGE" bash -euo pipefail -c '
  echo "container: $(uname -m) $(. /etc/os-release && echo "$PRETTY_NAME")"
  apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends sudo procps >/dev/null
  install -d -m 0755 /usr/local/lib/redundanet3 /mnt/storage /boot/firmware
  install -m 0755 /work/install.sh /work/firstboot.sh /work/status.sh /usr/local/lib/redundanet3/
  install -m 0644 /work/redundanet-agent.service /usr/local/lib/redundanet3/
  # the image build: the archive it verified, no device key
  ARCHIVE=/work/redundanet-$VERSION-linux-arm64.tar.gz NO_DEVICE_KEY=1 STORAGE_PATH=/mnt/storage CHANNEL=stable \
    bash /usr/local/lib/redundanet3/install.sh prepare "$VERSION" | tail -2
  test -x /var/lib/redundanet3/bin/redundanet || { echo "FAIL: no binary"; exit 1; }
  /var/lib/redundanet3/bin/redundanet version | grep -q "redundanet $VERSION (linux/arm64" || { echo "FAIL: version"; exit 1; }
  grep -q "^STORAGE_PATH=/mnt/storage$" /etc/redundanet3/agent.env || { echo "FAIL: agent.env"; exit 1; }
  test -d /mnt/storage/redundanet3 && grep -q ReadWritePaths=/mnt/storage/redundanet3 /etc/systemd/system/redundanet-agent.service.d/storage.conf || { echo "FAIL: store grant"; exit 1; }
  test ! -e /var/lib/redundanet3/identity && test ! -e /var/lib/redundanet3/device-key.pending || { echo "FAIL: the image build made a key"; exit 1; }
  echo "prepared without a key: ok"
  # pair: shows a code, waits, and gives up cleanly when nobody comes
  if PAIR_WAIT=2s bash /usr/local/lib/redundanet3/install.sh pair > /tmp/pair.out 2>&1; then echo "FAIL: pair succeeded with nobody pairing"; exit 1; fi
  grep -q "rn3:pair:" /tmp/pair.out && grep -q "pairing did not complete" /tmp/pair.out || { echo "FAIL: pair output"; cat /tmp/pair.out; exit 1; }
  test ! -e /var/lib/redundanet3/identity || { echo "FAIL: an identity appeared from a failed pairing"; exit 1; }
  echo "pair: code shown, gave up after the wait: ok"
  # first boot, twice: the pairing code on the boot partition, the same one
  # after a restart, since the secret and the port are kept
  timeout 40 bash /usr/local/lib/redundanet3/firstboot.sh > /tmp/fb1.out 2>&1 || true
  code=$(grep -oE "rn3:pair:[A-Za-z0-9_-]+" /boot/firmware/redundanet-pairing-code.txt | head -n 1)
  test -n "$code" || { echo "FAIL: no code on the boot partition"; cat /tmp/fb1.out; exit 1; }
  test -e /var/lib/redundanet3/pairing.state || { echo "FAIL: the code was not kept"; exit 1; }
  test -e /var/lib/redundanet3/device-key.pending || { echo "FAIL: no pending key"; exit 1; }
  grep -q "$code" /etc/motd || { echo "FAIL: motd"; exit 1; }
  grep -q "device pair --storage $code" /boot/firmware/redundanet-pairing-code.txt || { echo "FAIL: the code file does not say what to run"; exit 1; }
  sleep 1
  timeout 40 bash /usr/local/lib/redundanet3/firstboot.sh > /tmp/fb2.out 2>&1 || true
  again=$(grep -oE "rn3:pair:[A-Za-z0-9_-]+" /boot/firmware/redundanet-pairing-code.txt | head -n 1)
  [ "$again" = "$code" ] || { echo "FAIL: the code changed between boots: $code then $again"; cat /tmp/fb2.out; exit 1; }
  test ! -e /var/lib/redundanet3/identity || { echo "FAIL: an identity appeared"; exit 1; }
  echo "first boot: code ${code:0:24}..., the same across boots, no identity: ok"
  # status.sh: silent before pairing; with an identity, the card says how things stand
  bash /usr/local/lib/redundanet3/status.sh
  test ! -e /boot/firmware/redundanet-status.txt || { echo "FAIL: a status file before pairing"; exit 1; }
  touch /var/lib/redundanet3/identity
  bash /usr/local/lib/redundanet3/status.sh
  grep -q "^paired:" /boot/firmware/redundanet-status.txt && grep -q "^disk: nothing mounted" /boot/firmware/redundanet-status.txt && grep -q "^agent:" /boot/firmware/redundanet-status.txt || { echo "FAIL: status file"; cat /boot/firmware/redundanet-status.txt; exit 1; }
  rm -f /var/lib/redundanet3/identity
  echo "status: paired node without its disk reported: ok"
  # disk: refuses what is not a device
  bash /usr/local/lib/redundanet3/install.sh disk 2>&1 | grep -q "needs the device" || { echo "FAIL: disk without a device"; exit 1; }
  bash /usr/local/lib/redundanet3/install.sh disk /dev/null 2>&1 | grep -q "not a block device" || { echo "FAIL: disk on a non-device"; exit 1; }
  echo "disk: refuses a missing or wrong device: ok"
'
ok "provisioning, first boot, status and disk checks"
