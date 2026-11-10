#!/usr/bin/env bash
# RedundaNet 3 Raspberry Pi image, first boot: this node shows a pairing code
# until a device of the member pairs it. The code goes to the boot partition
# (redundanet-pairing-code.txt), the console's message of the day and the
# journal, and it stays the same across reboots: the card can be read on
# another computer while the Pi is off, then booted again. A device of the
# member anywhere answers it (the hub relays the exchange), hands over the
# identity and the network, and the node joins at once. The agent is then
# enabled; it starts once the storage disk is mounted (agent-disk.conf), and
# status.sh writes how things stand to redundanet-status.txt. Runs at every
# boot until the node has an identity.
set -uo pipefail
. /etc/redundanet3/agent.env
DATA=/var/lib/redundanet3
BIN=$DATA/bin/redundanet
CMD=/usr/local/bin/redundanet3 # the copy root owns and runs; BIN is the service's
BOOT=/boot/firmware
CODE=$BOOT/redundanet-pairing-code.txt
LOG=/run/redundanet3-pair.log
STATUS=/usr/local/lib/redundanet3/status.sh
netflag=""; [ "$NETWORK" = redundanet ] || netflag="--network $NETWORK "

[ -f "$DATA/identity" ] && exit 0
# The help exits non-zero, so no pipeline here (pipefail).
help=$("$CMD" identity pair -h 2>&1 || true)
case "$help" in
  *code-file*) ;;
  *) echo "firstboot: $("$CMD" version) cannot keep a pairing code across boots; this image needs the agent 3.0.12 or later" >&2; exit 1 ;;
esac

# show puts the code where the member finds it.
show() {
  local code=$1
  cat > "$CODE.new" <<TXT
RedundaNet 3 node: the pairing code of this Raspberry Pi

  $code

On a device of yours that has its identity, with the recovery phrase at
hand, on this network or anywhere else (the hub relays the exchange):

  redundanet device pair ${netflag}--storage

and paste the code when it asks.

The code stays valid, and the same after every reboot, until the node is
paired; anyone holding it could claim the node until then, so it is for
your eyes. The node joins the network the moment it is paired, and
redundanet-status.txt appears next to this file to say how things stand.
Then list the node: on the device that paired it, redundanet join-request
opens the form with this node filled in.

The store needs a disk labeled REDUNDANET. Over SSH, with the disk
attached, sudo redundanet-install disk /dev/sda formats one (everything on
it is destroyed) and the agent starts; a disk already labeled so mounts on
its own. The Pi's address: redundanet.local, login redundanet / redundanet.
TXT
  mv -f "$CODE.new" "$CODE"
  cat > /etc/motd <<TXT

  RedundaNet 3 node, not yet paired. The pairing code of this Raspberry Pi:

    $code

  On a device of yours with its identity, with the phrase at hand:
    redundanet device pair ${netflag}--storage     (it asks for the code)
  The same code is in redundanet-pairing-code.txt on the boot partition.
  The store needs a disk labeled REDUNDANET: sudo redundanet-install disk /dev/sda
  Change the password first: passwd

TXT
  echo "firstboot: pairing code $code, also in $CODE"
}

# The listener runs as the service user, so the identity it receives is
# its own; it writes the code to a file of ours, kept the same across runs
# (identity pair --code-file). Ten minutes per run: a code made before the
# network was up names no hub to relay through, and the next run fixes it.
pid=""
stop() { [ -n "$pid" ] && kill "$pid" 2>/dev/null; return 0; }
trap stop EXIT
trap 'stop; exit 143' TERM INT
while [ ! -f "$DATA/identity" ]; do
  rm -f "$DATA/pairing.code"
  sudo -u redundanet3 "$BIN" identity pair --network "$NETWORK" --data-dir "$DATA" --code-file "$DATA/pairing.code" --wait 10m > "$LOG" 2>&1 &
  pid=$!
  for _ in $(seq 1 30); do [ -s "$DATA/pairing.code" ] && break; sleep 1; done
  if [ -s "$DATA/pairing.code" ]; then
    show "$(tr -d '[:space:]' < "$DATA/pairing.code")"
  fi
  wait "$pid"; rc=$?; pid=""
  [ -f "$DATA/identity" ] && break
  echo "firstboot: pairing run ended ($rc): $(tail -n 2 "$LOG" | tr '\n' ' ')"
  sleep 10
done

# Paired: the network came with the offer. The agent is a system service
# from now on, running while the disk labeled REDUNDANET is mounted; the
# code leaves the card.
cat "$LOG"
rm -f "$CODE" "$CODE.new" "$DATA/pairing.code"
systemctl enable redundanet-agent >/dev/null 2>&1 || true
systemctl restart redundanet-agent >/dev/null 2>&1 || echo "firstboot: the agent waits for the storage disk"
[ -x "$STATUS" ] && "$STATUS"
exit 0
