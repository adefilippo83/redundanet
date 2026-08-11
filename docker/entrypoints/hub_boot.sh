#!/bin/sh
# Boot wrapper for the bootstrap hub image (fly.io / plain Docker).
#
# Platforms like fly.io deliver secrets as environment variables, but the tinc
# entrypoint reads the node's GPG private key from a file (the docker-compose
# stack mounts it at /run/secrets/gpg_private_key). Bridge the two here, then
# hand over to supervisord.
set -eu

mkdir -p /run/secrets
if [ ! -s /run/secrets/gpg_private_key ]; then
    if [ -n "${GPG_PRIVATE_KEY_B64:-}" ]; then
        echo "$GPG_PRIVATE_KEY_B64" | base64 -d > /run/secrets/gpg_private_key
    elif [ -n "${GPG_PRIVATE_KEY:-}" ]; then
        printf '%s' "$GPG_PRIVATE_KEY" > /run/secrets/gpg_private_key
    fi
fi

if [ ! -s /run/secrets/gpg_private_key ]; then
    echo "FATAL: no GPG private key. Set the GPG_PRIVATE_KEY_B64 secret" \
         "(base64 of the armored key) or mount /run/secrets/gpg_private_key." >&2
    exit 1
fi
chmod 600 /run/secrets/gpg_private_key

exec supervisord -c /etc/supervisor/conf.d/bootstrap.conf
