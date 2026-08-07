#!/usr/bin/env bash
#
# Multi-node erasure-coding grid test for RedundaNet.
#
# Exercises the product's core promise — a file stored on the grid survives
# node failure:
#   1. Start 1 introducer + 3 independent Tahoe storage nodes + 1 client
#      (shares.needed=2, shares.happy=3, shares.total=3)
#   2. Upload a file; happy=3 means the upload only succeeds once the shares
#      are placed on THREE DISTINCT storage servers
#   3. Verify share placement with `tahoe check` (3 good share hosts)
#   4. Stop one storage node
#   5. Download the file and verify it is byte-identical (2-of-3 reconstruction)
#
# Runs the same way locally and in CI (.github/workflows/grid-test.yml).
#
# Env knobs:
#   BUILD=0   Skip image build (use already-built images)
#   KEEP=1    Keep containers/volumes running after the test (debug)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PROJECT="rn-grid"
COMPOSE=(docker compose -p "$PROJECT" -f docker/docker-compose.grid-test.yml)
BUILD="${BUILD:-1}"
KEEP="${KEEP:-0}"
WORKDIR="$(mktemp -d)"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "Grid test failed (exit $rc). Recent logs:"
    "${COMPOSE[@]}" ps || true
    "${COMPOSE[@]}" logs --tail=60 || true
  fi
  if [ "$KEEP" = "1" ]; then
    log "KEEP=1 — leaving stack up. Tear down: docker compose -p $PROJECT -f docker/docker-compose.grid-test.yml down -v"
  else
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" 2>/dev/null || true
  return $rc
}
trap cleanup EXIT

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

# ----------------------------------------------------------------------------
# 1. Build and start the grid
# ----------------------------------------------------------------------------
if [ "$BUILD" = "1" ]; then
  log "Building images"
  "${COMPOSE[@]}" build
fi

log "Step 1/5 — Starting introducer + 3 storage nodes + client"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true

"${COMPOSE[@]}" up -d introducer
wait_healthy introducer 180

"${COMPOSE[@]}" up -d storage-1 storage-2 storage-3
wait_healthy storage-1 180
wait_healthy storage-2 180
wait_healthy storage-3 180

"${COMPOSE[@]}" up -d client
wait_healthy client 180

CLIENT=rn-grid-client
TAHOE=(docker exec "$CLIENT" tahoe -d /var/lib/tahoe-client)

# ----------------------------------------------------------------------------
# 2. Upload — happy=3 forces placement on three distinct servers
# ----------------------------------------------------------------------------
log "Step 2/5 — Uploading a file (shares 2-of-3, happy=3)"
PAYLOAD_FILE="$WORKDIR/input.bin"
head -c 65536 /dev/urandom > "$PAYLOAD_FILE"
docker cp "$PAYLOAD_FILE" "$CLIENT:/tmp/input.bin"

CAP=""
for attempt in $(seq 1 30); do
  if CAP="$("${TAHOE[@]}" put /tmp/input.bin 2>/dev/null)" && [ -n "$CAP" ]; then
    break
  fi
  echo "  …waiting for all 3 storage servers to join the grid (attempt $attempt)"
  sleep 5
done
[ -n "$CAP" ] || { fail "Upload never succeeded — grid did not reach happy=3 servers"; exit 1; }
ok "Uploaded, capability: ${CAP:0:40}…"
ok "shares.happy=3 satisfied: shares are on 3 distinct storage servers"

# ----------------------------------------------------------------------------
# 3. Verify share placement explicitly
# ----------------------------------------------------------------------------
log "Step 3/5 — Verifying share placement with 'tahoe check'"
CHECK_JSON="$("${TAHOE[@]}" check --raw "$CAP")"
HOSTS="$(printf '%s' "$CHECK_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["results"]["count-good-share-hosts"])')"
GOOD="$(printf '%s' "$CHECK_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["results"]["count-shares-good"])')"
[ "$HOSTS" -ge 3 ] || { fail "Expected shares on >=3 hosts, got $HOSTS"; exit 1; }
[ "$GOOD" -ge 3 ] || { fail "Expected >=3 good shares, got $GOOD"; exit 1; }
ok "$GOOD good shares spread across $HOSTS storage hosts"

# ----------------------------------------------------------------------------
# 4. Kill a storage node
# ----------------------------------------------------------------------------
log "Step 4/5 — Stopping storage-2 (simulated node failure)"
docker stop rn-grid-storage-2 >/dev/null
ok "storage-2 is down; only 2 of 3 share hosts remain"

# ----------------------------------------------------------------------------
# 5. Download must still work (2-of-3 erasure reconstruction)
# ----------------------------------------------------------------------------
log "Step 5/5 — Downloading with one node down"
DOWNLOADED=""
for attempt in $(seq 1 12); do
  if "${TAHOE[@]}" get "$CAP" /tmp/output.bin 2>/dev/null; then
    DOWNLOADED=1
    break
  fi
  echo "  …retrying download (attempt $attempt)"
  sleep 5
done
[ -n "$DOWNLOADED" ] || { fail "Download failed with one storage node down"; exit 1; }

docker cp "$CLIENT:/tmp/output.bin" "$WORKDIR/output.bin"
if cmp -s "$PAYLOAD_FILE" "$WORKDIR/output.bin"; then
  ok "Downloaded file is byte-identical to the original"
else
  fail "Downloaded file differs from the original!"
  exit 1
fi

log "SUCCESS — erasure-coded grid survived a storage-node failure 🎉"
