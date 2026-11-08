#!/usr/bin/env bash
# RedundaNet 3 Raspberry Pi image, first boot: this node's device key, made
# here so that no two cards share one, handed to the member through the boot
# partition and the console. Runs at every boot until the node has an
# identity, so the key stays visible.
set -euo pipefail
. /etc/redundanet3/agent.env
DATA=/var/lib/redundanet3
BIN=$DATA/bin/redundanet
BOOT=/boot/firmware
KEYFILE=$BOOT/redundanet-device-key.txt
[ -f "$DATA/identity" ] && exit 0
key=$(sudo -u redundanet3 "$BIN" identity device-key --network "$NETWORK" --data-dir "$DATA" | awk '/^device key:/ {print $3}')
[ -n "$key" ] || { echo "firstboot: the agent printed no device key" >&2; exit 1; }
# The network built into the binary needs no flag on the member's laptop.
netflag=""; [ "$NETWORK" = redundanet ] || netflag="--network $NETWORK "
cat > "$KEYFILE" <<TXT
RedundaNet 3 node: the device key of this Raspberry Pi

  $key

On a computer where you can type your recovery phrase:

  redundanet identity certify ${netflag}--device $key --storage > redundanet-node.cert

Then put redundanet-node.cert on this SD card's boot partition, next to this
file, and boot the Pi again; or copy it over SSH to /boot/firmware/ while it
runs. The node accepts the certificate, joins the network and starts the
agent; redundanet-status.txt appears next to this file and says how it went.
The certificate file is destroyed once accepted.

Before that, give the node its storage disk, mounted at $STORAGE_PATH:
see rpi-image/README.md in the repository.
TXT
cat > /etc/motd <<TXT

  RedundaNet 3 node, not yet joined. Device key of this Raspberry Pi:

    $key

  Certify it on a computer with your recovery phrase, then put the
  certificate at /boot/firmware/redundanet-node.cert (or run
  sudo redundanet-install finish <file>). Details: redundanet-device-key.txt
  on the boot partition. Change the password first: passwd

TXT
echo "firstboot: device key $key written to $KEYFILE"
