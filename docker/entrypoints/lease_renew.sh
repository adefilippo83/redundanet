#!/bin/sh
# Periodic lease renewal for the tahoe-client container.
#
# Storage nodes garbage-collect shares whose lease is older than the network's
# lease duration (expire.override_lease_duration, default 90 days). This job
# renews the leases of everything reachable from the client's aliases, so data
# organized under aliases stays alive as long as the client runs.
#
# Bare capabilities that are not linked into any alias must be renewed by
# their owner (redundanet storage renew <cap>).
NODE_DIR=/var/lib/tahoe-client
INTERVAL="${REDUNDANET_LEASE_RENEW_INTERVAL:-604800}"  # 7 days

while :; do
    # Give the client time to connect to the grid (first boot / restart).
    sleep 300
    aliases=$(tahoe -d "$NODE_DIR" list-aliases 2>/dev/null | awk -F: '{print $1}')
    for a in $aliases; do
        echo "lease-renew: renewing $a:"
        tahoe -d "$NODE_DIR" deep-check --add-lease "$a:" \
            || echo "lease-renew: FAILED for $a: (will retry next cycle)"
    done
    sleep "$INTERVAL"
done
