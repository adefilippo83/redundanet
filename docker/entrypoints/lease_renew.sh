#!/bin/sh
# Periodic lease renewal + repair sweep for the tahoe-client container.
#
# Storage nodes garbage-collect shares whose lease is older than the network's
# lease duration (expire.override_lease_duration, default 90 days). This job
# walks everything reachable from the client's aliases and, per object:
#   - renews its leases, so data under an alias stays alive while the client
#     runs;
#   - repairs it when shares are missing (a storage node lost its disk, a
#     member left): Tahoe re-derives the missing shares from the surviving
#     ones and places them on servers that lack one. Only objects that still
#     have at least k shares can be repaired; below k they are gone.
#
# Repair is on by default; REDUNDANET_REPAIR_ENABLED=false only renews.
# Bare capabilities that are not linked into any alias are neither renewed nor
# repaired here; their owner must do it (redundanet storage renew <cap>).
NODE_DIR=/var/lib/tahoe-client
INTERVAL="${REDUNDANET_LEASE_RENEW_INTERVAL:-604800}"  # 7 days
TIMEOUT="${REDUNDANET_LEASE_RENEW_TIMEOUT:-21600}"     # per-alias ceiling, 6h
REPAIR="${REDUNDANET_REPAIR_ENABLED:-true}"

if [ "$REPAIR" = "true" ]; then
    MODE="renew+repair"
    REPAIR_FLAG="--repair"
else
    MODE="renew only"
    REPAIR_FLAG=""
fi

# A hung deep-check must not stall the loop forever; cap each alias sweep.
if command -v timeout >/dev/null 2>&1; then
    TIMEOUT_CMD="timeout $TIMEOUT"
else
    TIMEOUT_CMD=""
fi

while :; do
    # Give the client time to connect to the grid (first boot / restart).
    sleep 300
    aliases=$(tahoe -d "$NODE_DIR" list-aliases 2>/dev/null | cut -d: -f1)
    for a in $aliases; do
        echo "lease-repair: sweeping $a: ($MODE)"
        # deep-check prints a per-alias summary: objects checked, healthy vs
        # unhealthy before and after, repairs attempted/successful/failed.
        # shellcheck disable=SC2086  # TIMEOUT_CMD is intentionally word-split
        out=$($TIMEOUT_CMD tahoe -d "$NODE_DIR" deep-check --add-lease \
                ${REPAIR_FLAG:+"$REPAIR_FLAG"} "$a:" 2>&1)
        rc=$?
        printf '%s\n' "$out" | sed "s/^/lease-repair: $a: /"
        if [ "$rc" -ne 0 ]; then
            echo "lease-repair: FAILED for $a: (exit $rc; will retry next cycle)"
        fi
    done
    sleep "$INTERVAL"
done
