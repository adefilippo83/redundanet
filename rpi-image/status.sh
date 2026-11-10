#!/usr/bin/env bash
# RedundaNet 3 Raspberry Pi image: writes redundanet-status.txt on the boot
# partition and the console's message of the day, for a member who reads
# the card on another computer or logs in. Runs when the node is paired
# (firstboot.sh), when a disk is prepared (install.sh disk) and every few
# minutes from a timer. Before pairing, firstboot.sh owns both files.
set -uo pipefail
. /etc/redundanet3/agent.env
DATA=/var/lib/redundanet3
BIN=$DATA/bin/redundanet
CMD=/usr/local/bin/redundanet3 # the copy root owns and runs; BIN is the service's
BOOT=/boot/firmware
STATUS=$BOOT/redundanet-status.txt
[ -f "$DATA/identity" ] || exit 0

paired="paired: this node has its identity and the network"
if findmnt -rn "$STORAGE_PATH" >/dev/null 2>&1; then
  disk="disk: $(findmnt -rn -o SOURCE,FSTYPE,SIZE "$STORAGE_PATH") mounted at $STORAGE_PATH; the store is $STORAGE_PATH/redundanet3"
else
  disk="disk: nothing mounted at $STORAGE_PATH. The agent waits for a disk labeled REDUNDANET: over SSH, sudo redundanet-install disk /dev/sda formats one (everything on it is destroyed); an ext4 disk already labeled so mounts on its own"
fi
if systemctl is-active -q redundanet-agent 2>/dev/null; then
  agent="agent: running, $("$CMD" version 2>/dev/null)"
else
  agent="agent: $(systemctl is-active redundanet-agent 2>/dev/null); it runs while the disk is mounted"
fi
{
  echo "RedundaNet 3 node, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "$paired"
  echo "$disk"
  echo "$agent"
  echo
  echo "next: list the node. On the device that paired it, redundanet join-request opens the form with"
  echo "this node filled in; the node counts once the maintainer has merged it under storage: in the manifest."
  echo "over SSH (redundanet@redundanet.local): sudo redundanet3 status"
  echo "the page: ssh -L 4470:127.0.0.1:4470 redundanet@redundanet.local, then sudo redundanet3 open --print"
} > "$STATUS.new" 2>&1 && mv -f "$STATUS.new" "$STATUS"
cat > /etc/motd <<TXT

  RedundaNet 3 node. $paired
  $disk
  $agent
    sudo redundanet3 status
    sudo redundanet3 open --print

TXT
