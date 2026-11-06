#!/usr/bin/env bash
# RedundaNet 3 Raspberry Pi image: a certificate placed on the boot partition
# joins the node. The outcome goes to redundanet-status.txt beside it, for a
# member who never opens a shell on the Pi.
set -uo pipefail
. /etc/redundanet3/agent.env
BOOT=/boot/firmware
CERT=$BOOT/redundanet-node.cert
STATUS=$BOOT/redundanet-status.txt
INSTALL=/usr/local/lib/redundanet3/install.sh
{
  echo "RedundaNet 3 node, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  if [ -f /var/lib/redundanet3/identity ]; then
    echo "this node already has an identity; the certificate was not used and has been removed"
    rm -f "$CERT"
  else
    # The store directory under the storage path, whether a disk is mounted there yet or not.
    install -d -o redundanet3 -g redundanet3 -m 0750 "$STORAGE_PATH/redundanet3"
    if out=$(bash "$INSTALL" finish "$CERT" 2>&1); then
      echo "joined and started: the agent runs, updates itself from the signed release index, and shows on the status page once the operator has listed this node."
      echo "local page over SSH: ssh -L 4470:127.0.0.1:4470 redundanet@<this pi>, then: sudo /var/lib/redundanet3/bin/redundanet open --print"
      rm -f "$BOOT/redundanet-device-key.txt"
      cat > /etc/motd <<'TXT'

  RedundaNet 3 node. The agent runs as redundanet-agent and updates itself.
    sudo /var/lib/redundanet3/bin/redundanet status
    sudo /var/lib/redundanet3/bin/redundanet open --print

TXT
    else
      echo "the certificate was not accepted. The file has been removed; make a new one and put it here again."
      echo
      echo "$out" | tail -20
      rm -f "$CERT"
    fi
  fi
} > "$STATUS" 2>&1
cat "$STATUS"
