#!/usr/bin/env bash
#
# Bootstrap the RedundaNet hub identity (local half of docs/bootstrap-hub.md).
#
#   1. Generate the hub's GPG identity key (RSA-4096, no passphrase)
#   2. Publish it to the public keyservers and VERIFY it is fetchable
#   3. Export the private key for the fly.io secret
#   4. Patch manifests/manifest.yaml with the key's full fingerprint
#      (and the hub's public IP, if passed)
#
# The fly.io half (apps create, ips allocate, secrets set, deploy, FURL) is
# printed at the end — those steps touch your fly account and the git history,
# so they stay manual.
#
# Usage:
#   scripts/bootstrap-hub.sh --email hub@redundanet.com [--public-ip 1.2.3.4]
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

EMAIL=""
PUBLIC_IP=""
while [ $# -gt 0 ]; do
  case "$1" in
    --email)     EMAIL="$2"; shift 2 ;;
    --public-ip) PUBLIC_IP="$2"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done
[ -n "$EMAIL" ] || { echo "Usage: $0 --email <address> [--public-ip <ip>]" >&2; exit 1; }

BOOTSTRAP_DIR="$REPO_ROOT/.bootstrap"
export GNUPGHOME="$BOOTSTRAP_DIR/gnupg"
KEY_FILE="$BOOTSTRAP_DIR/hub_gpg_private_key.asc"
MANIFEST="manifests/manifest.yaml"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }

command -v gpg >/dev/null    || { echo "gpg is required" >&2; exit 1; }
command -v poetry >/dev/null || { echo "poetry is required (run from the repo)" >&2; exit 1; }

# ----------------------------------------------------------------------------
log "Step 1/4 — Generate the hub GPG identity (RSA-4096)"
mkdir -p "$GNUPGHOME"; chmod 700 "$GNUPGHOME"

FPR="$(gpg --list-keys --with-colons "$EMAIL" 2>/dev/null | awk -F: '/^fpr:/{print $10; exit}')"
if [ -n "$FPR" ]; then
  ok "Reusing existing key for $EMAIL: $FPR"
else
  cat > "$BOOTSTRAP_DIR/keyparams" <<EOF
%no-protection
Key-Type: RSA
Key-Length: 4096
Name-Real: RedundaNet Hub
Name-Email: $EMAIL
Expire-Date: 0
%commit
EOF
  gpg --batch --gen-key "$BOOTSTRAP_DIR/keyparams"
  FPR="$(gpg --list-keys --with-colons "$EMAIL" | awk -F: '/^fpr:/{print $10; exit}')"
  ok "Generated key: $FPR"
fi

# ----------------------------------------------------------------------------
log "Step 2/4 — Publish to keyservers and verify fetchability"
# 'node keys publish' fetches the key back and fails loudly if no keyserver
# actually serves it (GNUPGHOME is honored by the underlying gpg binary).
poetry run redundanet node keys publish --key-id "$FPR"
ok "Key published and verified fetchable"

# ----------------------------------------------------------------------------
log "Step 3/4 — Export the private key for the fly.io secret"
gpg --batch --pinentry-mode loopback --armor --export-secret-keys "$FPR" > "$KEY_FILE"
chmod 600 "$KEY_FILE"
ok "Private key -> $KEY_FILE (gitignored; back it up somewhere safe OFF this machine)"

# ----------------------------------------------------------------------------
log "Step 4/4 — Patch $MANIFEST"
python3 - "$MANIFEST" "$FPR" "$PUBLIC_IP" <<'EOF'
import sys

path, fpr, public_ip = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
text = text.replace("'0000000000000000000000000000000000000000'", f"'{fpr}'")
if public_ip:
    text = text.replace("public_ip: 0.0.0.0", f"public_ip: {public_ip}")
open(path, "w").write(text)
EOF
poetry run redundanet validate "$MANIFEST"
poetry run python .github/scripts/validate_pr.py "$MANIFEST"
ok "Manifest patched and valid (fingerprint $FPR)"
[ -n "$PUBLIC_IP" ] || echo "  NOTE: public_ip is still the 0.0.0.0 placeholder — patch it after 'fly ips allocate-v4'."

# ----------------------------------------------------------------------------
cat <<EOF

Local half done. Now the fly.io half (see docs/bootstrap-hub.md for details):

  fly apps create redundanet-hub
  fly ips allocate-v4                              # note the dedicated IPv4
  # if you didn't pass --public-ip: put that IP into $MANIFEST (public_ip)
  fly volumes create introducer_data --region fra --size 1
  fly secrets set GPG_PRIVATE_KEY_B64="\$(base64 < $KEY_FILE)"

  # COMMIT AND PUSH the manifest before deploying (the hub syncs it at boot):
  git add $MANIFEST && git commit -m "Genesis manifest: bootstrap hub" && git push

  fly deploy
  fly ssh console -C "cat /var/lib/tahoe-introducer/private/introducer.furl"
  # put that FURL into $MANIFEST (introducer_furl), commit and push again

Then back up: the key file above AND a volume snapshot
(fly volumes snapshots create <volume-id>) — together they are the network's
anchor identity.
EOF
