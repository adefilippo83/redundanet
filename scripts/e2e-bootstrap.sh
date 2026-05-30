#!/usr/bin/env bash
#
# End-to-end bootstrap test for RedundaNet.
#
# Exercises the full path a real user follows:
#   1. Generate a GPG node identity keypair
#   2. Produce node configuration (env + exported private key secret)
#   3. Start the components: tinc VPN -> tahoe introducer -> storage -> client
#   4. Operate the grid: upload a file and download it back, byte-identical
#
# Runs the same way locally (laptop) and in CI (GitHub Actions).
#
# Usage:
#   scripts/e2e-bootstrap.sh
#
# Env knobs:
#   BUILD=0           Skip image build (use already-built images)
#   KEEP=1            Keep containers/volumes running after the test (debug)
#   KEY_LENGTH=2048   GPG key length (2048 is fast; 4096 matches production)
#   VPN_IP=...        Override the node VPN IP (default 10.100.0.10)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# ----------------------------------------------------------------------------
# Configuration
# ----------------------------------------------------------------------------
PROJECT="redundanet-e2e"
COMPOSE=(docker compose -p "$PROJECT" -f docker/docker-compose.yml -f docker/docker-compose.e2e.yml)
export COMPOSE_PROFILES="introducer,storage,client"

export VPN_IP="${VPN_IP:-10.100.0.10}"
export PUBLIC_IP=""                 # single node, not publicly reachable
export LOG_LEVEL="${LOG_LEVEL:-INFO}"
# Single-host grid: one storage node must be enough to satisfy uploads.
export SHARES_NEEDED=1
export SHARES_HAPPY=1
export SHARES_TOTAL=1
export RESERVED_SPACE="1G"
# Bind-mount paths are resolved relative to the compose file dir (docker/).
export GPG_KEY_FILE="./secrets/gpg_private_key.asc"
export MOUNT_POINT="./mount"

BUILD="${BUILD:-1}"
KEEP="${KEEP:-0}"
KEY_LENGTH="${KEY_LENGTH:-2048}"

SECRETS_DIR="$REPO_ROOT/docker/secrets"
MOUNT_DIR="$REPO_ROOT/docker/mount"
WORKDIR="$(mktemp -d)"
export GNUPGHOME="$WORKDIR/gnupg"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "Test failed (exit $rc). Dumping recent container logs:"
    "${COMPOSE[@]}" ps || true
    "${COMPOSE[@]}" logs --tail=60 || true
  fi
  if [ "$KEEP" = "1" ]; then
    log "KEEP=1 set — leaving stack running. Tear down with:"
    echo "    docker compose -p $PROJECT -f docker/docker-compose.yml down -v"
  else
    log "Tearing down"
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" "$SECRETS_DIR" "$MOUNT_DIR" 2>/dev/null || true
  return $rc
}
trap cleanup EXIT

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------
wait_healthy() {
  local svc="$1" timeout="${2:-180}" cid status waited=0
  cid="$("${COMPOSE[@]}" ps -q "$svc")"
  [ -n "$cid" ] || { fail "$svc has no container"; return 1; }
  while :; do
    status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid" 2>/dev/null || echo gone)"
    case "$status" in
      healthy) ok "$svc is healthy"; return 0 ;;
      unhealthy) fail "$svc became unhealthy"; return 1 ;;
    esac
    if ! docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null | grep -q true; then
      fail "$svc container exited"; return 1
    fi
    [ "$waited" -ge "$timeout" ] && { fail "$svc not healthy after ${timeout}s (status=$status)"; return 1; }
    sleep 3; waited=$((waited + 3))
  done
}

wait_for_furl() {
  local timeout="${1:-180}" waited=0
  while :; do
    if "${COMPOSE[@]}" exec -T tahoe-introducer test -s /var/lib/redundanet/manifest/introducer.furl 2>/dev/null; then
      ok "Introducer FURL published to shared volume"; return 0
    fi
    [ "$waited" -ge "$timeout" ] && { fail "Introducer FURL not published after ${timeout}s"; return 1; }
    sleep 3; waited=$((waited + 3))
  done
}

# ----------------------------------------------------------------------------
# 1. Generate GPG node identity keypair
# ----------------------------------------------------------------------------
log "Step 1/4 — Generating GPG node identity keypair (RSA-$KEY_LENGTH)"
mkdir -p "$GNUPGHOME"; chmod 700 "$GNUPGHOME"
EMAIL="e2e@redundanet.local"
cat > "$WORKDIR/keyparams" <<EOF
%no-protection
Key-Type: RSA
Key-Length: $KEY_LENGTH
Name-Real: RedundaNet E2E Node
Name-Email: $EMAIL
Expire-Date: 0
%commit
EOF
gpg --batch --gen-key "$WORKDIR/keyparams" 2>/dev/null
FPR="$(gpg --list-keys --with-colons "$EMAIL" | awk -F: '/^fpr:/{print $10; exit}')"
GPG_KEY_ID="${FPR: -16}"
NODE_SUFFIX="$(printf '%s' "${FPR: -8}" | tr 'A-Z' 'a-z')"
export GPG_KEY_ID
export NODE_NAME="node-$NODE_SUFFIX"
ok "Key fingerprint: $FPR"
ok "Node name:       $NODE_NAME"
ok "GPG key id:      $GPG_KEY_ID"

# ----------------------------------------------------------------------------
# 2. Produce node configuration (exported private key + env)
# ----------------------------------------------------------------------------
log "Step 2/4 — Writing node configuration"
# Docker auto-creates a *directory* at a bind-mount source if the file is
# missing; clear any such stale path before exporting the key.
rm -rf "$SECRETS_DIR" "$MOUNT_DIR"
mkdir -p "$SECRETS_DIR" "$MOUNT_DIR"
gpg --batch --pinentry-mode loopback --export-secret-keys --armor "$GPG_KEY_ID" \
  > "$SECRETS_DIR/gpg_private_key.asc"
ok "Exported private key -> docker/secrets/gpg_private_key.asc"
ok "NODE_NAME=$NODE_NAME VPN_IP=$VPN_IP shares=$SHARES_NEEDED/$SHARES_HAPPY/$SHARES_TOTAL"

# ----------------------------------------------------------------------------
# 3. Build and start the components
# ----------------------------------------------------------------------------
if [ "$BUILD" = "1" ]; then
  log "Building images"
  "${COMPOSE[@]}" build
fi

log "Step 3/4 — Starting components"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true

log "Starting tinc VPN"
"${COMPOSE[@]}" up -d tinc
wait_healthy tinc 120

log "Starting tahoe introducer"
"${COMPOSE[@]}" up -d tahoe-introducer
wait_healthy tahoe-introducer 180
wait_for_furl 120

log "Starting tahoe storage + client"
"${COMPOSE[@]}" up -d tahoe-storage tahoe-client
wait_healthy tahoe-storage 180
wait_healthy tahoe-client 180

# ----------------------------------------------------------------------------
# 4. Operate the grid: upload and download a file
# ----------------------------------------------------------------------------
log "Step 4/4 — Upload a file and download it back through the grid"
CLIENT_CID="$("${COMPOSE[@]}" ps -q tahoe-client)"

PAYLOAD="RedundaNet end-to-end test payload — unique marker 4f3c9a2e"
printf '%s\n' "$PAYLOAD" > "$WORKDIR/input.txt"
docker cp "$WORKDIR/input.txt" "$CLIENT_CID:/tmp/input.txt"

# The grid needs the storage server to connect to the introducer/client first;
# retry the upload until servers are available.
CAP=""
for attempt in $(seq 1 30); do
  if CAP="$(docker exec "$CLIENT_CID" tahoe -d /var/lib/tahoe-client put /tmp/input.txt 2>/dev/null)" \
      && [ -n "$CAP" ]; then
    break
  fi
  echo "  …waiting for grid to accept upload (attempt $attempt)"
  sleep 5
done
[ -n "$CAP" ] || { fail "Upload never succeeded"; exit 1; }
ok "Uploaded, capability: $CAP"

docker exec "$CLIENT_CID" tahoe -d /var/lib/tahoe-client get "$CAP" /tmp/output.txt
docker cp "$CLIENT_CID:/tmp/output.txt" "$WORKDIR/output.txt"

if diff -q "$WORKDIR/input.txt" "$WORKDIR/output.txt" >/dev/null; then
  ok "Downloaded file is byte-identical to the original"
else
  fail "Downloaded file differs from the original!"
  echo "--- expected ---"; cat "$WORKDIR/input.txt"
  echo "--- got ---";      cat "$WORKDIR/output.txt"
  exit 1
fi

log "SUCCESS — full bootstrap + file round-trip passed 🎉"
