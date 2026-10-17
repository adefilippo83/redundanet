#!/usr/bin/env bash
#
# Scale test for RedundaNet: how the parts that touch every object behave
# as the object count grows. Three tiers, all local, none touching the
# live network:
#
#   tier1  Pure Python, no Docker. The storage node's share census and the
#          hub's status collection at N synthetic objects (tests/scale/bench_*).
#   tier2  A real grid (docker-compose.grid-test.yml + the scale overlay):
#          introducer, three storage nodes, one client. Backs up a generated
#          tree with `tahoe backup`, then times every client-side walker the
#          way the sidecars run it: incremental snapshots, the usage meter,
#          the lease sweep, the backup re-check that Tahoe starts after two
#          months, and the census on each storage node, warm and cold.
#   all    Both.
#
# Usage:
#   scripts/scale-test.sh tier1|tier2|all
#
# Knobs (env):
#   OBJECTS=250000     tier1 census objects
#   PER_NODE=250000    tier1 hub objects per storage node (8 nodes, 4 copies)
#   FILES=5000 DIRS=500  tier2 tree; SNAPSHOTS=2 incremental runs; CHURN=1 (%)
#   BUILD=1            build images first (0 reuses existing ones)
#   KEEP=0             1 keeps the tier2 stack running afterwards
#   SCALE_ROOT=/tmp/redundanet-scale   where trees, censuses and results go
#
# Output: a results file under $SCALE_ROOT with one JSON line per measurement,
# echoed as it goes. Timings are wall clock seconds.
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TIER="${1:-all}"
OBJECTS="${OBJECTS:-250000}"
PER_NODE="${PER_NODE:-250000}"
FILES="${FILES:-5000}"
DIRS="${DIRS:-500}"
SNAPSHOTS="${SNAPSHOTS:-2}"
CHURN="${CHURN:-1}"
BUILD="${BUILD:-1}"
KEEP="${KEEP:-0}"
export SCALE_ROOT="${SCALE_ROOT:-/tmp/redundanet-scale}"
mkdir -p "$SCALE_ROOT"
RESULTS="$SCALE_ROOT/results-$(date +%Y%m%d-%H%M%S).jsonl"

PROJECT="rn-scale"
COMPOSE=(docker compose -p "$PROJECT" -f docker/docker-compose.grid-test.yml -f docker/docker-compose.scale-test.yml)
CLIENT=rn-grid-client
TAHOE=(docker exec "$CLIENT" tahoe -d /var/lib/tahoe-client)
PY="${PY:-poetry run python}"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }
now()  { python3 -c 'import time; print(time.time())'; }
since() { python3 -c "import time; print(round(time.time() - $1, 1))"; }
record() {  # record <step> <seconds> [key=value ...]
  local step="$1" seconds="$2"; shift 2
  local extra=""
  for kv in "$@"; do extra="$extra, \"${kv%%=*}\": \"${kv#*=}\""; done
  local line="{\"step\": \"$step\", \"seconds\": $seconds$extra}"
  echo "$line" | tee -a "$RESULTS"
}

# ----------------------------------------------------------------------------
# Tier 1
# ----------------------------------------------------------------------------
tier1() {
  log "Tier 1: share census at $OBJECTS objects (tests/scale/bench_census.py)"
  $PY tests/scale/bench_census.py --objects "$OBJECTS" --json | tee -a "$RESULTS"
  log "Tier 1: hub collection at $PER_NODE objects per node, 8 nodes, 4 copies"
  $PY tests/scale/bench_hub.py --objects-per-node "$PER_NODE" | tee -a "$RESULTS"
}

# ----------------------------------------------------------------------------
# Tier 2
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
    [ "$waited" -ge "$timeout" ] && { fail "$svc not healthy after ${timeout}s (status=$status)"; return 1; }
    sleep 3; waited=$((waited + 3))
  done
}

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "Scale test failed (exit $rc). Recent logs:"
    "${COMPOSE[@]}" ps || true
    "${COMPOSE[@]}" logs --tail=40 || true
  fi
  if [ "$KEEP" = "1" ]; then
    log "KEEP=1: stack left running. Tear down: ${COMPOSE[*]} down -v"
  else
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  return $rc
}

timed_exec() {  # timed_exec <step> <container> <cmd...>: records wall time, prints the tail
  local step="$1" container="$2"; shift 2
  local t0 out rc=0
  t0=$(now)
  out="$(docker exec "$container" "$@" 2>&1)" || rc=$?
  local secs; secs=$(since "$t0")
  printf '%s\n' "$out" | tail -n 6 | sed 's/^/    /'
  record "$step" "$secs" "exit=$rc" "last=$(printf '%s' "$out" | tail -n 1 | tr -d '"' | cut -c1-160)"
  LAST_OUTPUT="$out"
}

backupdb_sql() {  # run SQL against the client's backupdb
  docker exec "$CLIENT" python3 -c "
import sqlite3, sys
conn = sqlite3.connect('/var/lib/tahoe-client/private/backupdb.sqlite')
for stmt in sys.argv[1].split(';'):
    if stmt.strip():
        cur = conn.execute(stmt)
        if stmt.strip().lower().startswith('select'):
            print(cur.fetchall())
conn.commit()
" "$1"
}

census_line() {  # latest 'Census computed' line of a storage service as JSON fields
  docker logs "rn-grid-$1" 2>&1 | grep "Census computed" | tail -n 1 \
    | sed -e 's/\x1b\[[0-9;]*m//g' -E -e 's/.*objects=([0-9]+).*seconds=([0-9.]+).*/objects=\1 seconds=\2/'
}

census_walk() {  # time scan_shares inside a storage container
  docker exec "rn-grid-$1" python3 -c "
import time
from pathlib import Path
from redundanet.monitor.census import scan_shares
t = time.monotonic(); indexes, used = scan_shares(Path('/data/storage/shares'))
print(f'{len(indexes)} {time.monotonic() - t:.2f}')"
}

tier2() {
  trap cleanup EXIT
  local tree="$SCALE_ROOT/sync-$FILES-$DIRS"
  if [ ! -d "$tree" ]; then
    log "Tier 2: generating $FILES files in $DIRS directories"
    $PY tests/scale/gen_tree.py "$tree" --files "$FILES" --dirs "$DIRS"
  fi
  export SCALE_SYNC_DIR="$tree"

  if [ "$BUILD" = "1" ]; then
    log "Building images"
    docker build -t "${TAHOE_BASE:-ghcr.io/adefilippo83/redundanet-tahoe-base:latest}" -f docker/Dockerfile.tahoe-base .
    "${COMPOSE[@]}" build
  fi

  log "Tier 2: starting introducer, 3 storage nodes, client"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  "${COMPOSE[@]}" up -d introducer
  wait_healthy introducer 180
  "${COMPOSE[@]}" up -d storage-1 storage-2 storage-3
  wait_healthy storage-1 180; wait_healthy storage-2 180; wait_healthy storage-3 180
  "${COMPOSE[@]}" up -d client
  wait_healthy client 180

  log "Waiting for the client to see all three storage servers"
  docker exec "$CLIENT" sh -c 'head -c 4096 /dev/urandom > /tmp/probe.bin'
  local attempt
  for attempt in $(seq 1 40); do
    if "${TAHOE[@]}" put /tmp/probe.bin >/dev/null 2>&1; then break; fi
    sleep 5
  done
  "${TAHOE[@]}" create-alias backups >/dev/null 2>&1 || true
  ok "grid ready"

  log "Step 1: first backup of $FILES files / $DIRS directories"
  timed_exec "backup-first" "$CLIENT" tahoe -d /var/lib/tahoe-client backup /data/sync backups:

  local i
  for i in $(seq 1 "$SNAPSHOTS"); do
    log "Step 2.$i: churn $CHURN% of the files, incremental backup"
    $PY tests/scale/gen_tree.py "$tree" --churn-percent "$CHURN"
    timed_exec "backup-incremental-$i" "$CLIENT" tahoe -d /var/lib/tahoe-client backup /data/sync backups:
  done

  log "Step 3: usage meter, one measurement (tahoe ls per directory, as the sidecar does)"
  timed_exec "usage-meter-once" "$CLIENT" python /app/usage_report.py --once

  log "Step 4: the same walk through the web API (candidate replacement)"
  docker cp tests/scale/walk_webapi.py "$CLIENT:/tmp/walk_webapi.py"
  timed_exec "walk-webapi" "$CLIENT" python3 /tmp/walk_webapi.py backups:

  log "Step 5: lease sweep (deep-check --add-lease --repair, as lease_renew.sh does)"
  timed_exec "deep-check" "$CLIENT" tahoe -d /var/lib/tahoe-client deep-check --add-lease --repair backups:

  log "Step 6: age the backupdb by 70 days, then back up again (Tahoe re-checks every file)"
  backupdb_sql "UPDATE last_upload SET last_checked = last_checked - 70*86400, last_uploaded = last_uploaded - 70*86400; UPDATE directories SET last_checked = last_checked - 70*86400, last_uploaded = last_uploaded - 70*86400; SELECT count(*) FROM last_upload" || true
  timed_exec "backup-after-70-days" "$CLIENT" tahoe -d /var/lib/tahoe-client backup /data/sync backups:

  log "Step 7: share census on each storage node (warm walk from the sidecar log, then a measured cold and warm walk)"
  local s line
  for s in storage-1 storage-2 storage-3; do
    line="$(census_line "$s")"
    record "census-sidecar-$s" "$(printf '%s' "$line" | sed -E 's/.*seconds=([0-9.]+).*/\1/')" "$line"
  done
  docker run --rm --privileged alpine:3.20 sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null 2>&1 \
    && ok "page cache dropped" || fail "could not drop the page cache (cold walk below is warm)"
  line="$(census_walk storage-1)"
  record "census-walk-cold-storage-1" "${line#* }" "objects=${line%% *}"
  line="$(census_walk storage-1)"
  record "census-walk-warm-storage-1" "${line#* }" "objects=${line%% *}"
  local size
  size="$(docker exec "$CLIENT" python3 -c "
import time, urllib.request
t = time.monotonic(); body = urllib.request.urlopen('http://172.30.0.11:3459/census', timeout=600).read()
print(f'{len(body)} {time.monotonic() - t:.2f}')")"
  record "census-fetch-storage-1" "${size#* }" "bytes=${size%% *}"

  log "Step 8: Tahoe 1.20 lease expirer after a restart mid-cycle (state file as the JSON round trip leaves it)"
  # Stop first: the crawler writes its own state on shutdown and would
  # overwrite a file copied into a running container.
  docker stop rn-grid-storage-1 >/dev/null
  docker cp tests/scale/fixtures/lease_checker.state.json rn-grid-storage-1:/data/storage/lease_checker.state.json
  docker start rn-grid-storage-1 >/dev/null
  sleep 420   # the crawler's slow_start is 360 s; the first slice follows
  if docker logs --since 8m rn-grid-storage-1 2>&1 | grep -q "list indices must be integers or slices, not tuple"; then
    record "expirer-crash-after-restart" 0 "reproduced=yes"
  else
    record "expirer-crash-after-restart" 0 "reproduced=no"
  fi
  log "Step 9: the remedy, a deleted state file (the crawler then starts a fresh cycle)"
  docker stop rn-grid-storage-1 >/dev/null
  docker run --rm --volumes-from rn-grid-storage-1 alpine:3.20 rm -f /data/storage/lease_checker.state.json
  docker start rn-grid-storage-1 >/dev/null
  sleep 420
  if docker logs --since 8m rn-grid-storage-1 2>&1 | grep -q "list indices must be integers or slices, not tuple"; then
    record "expirer-after-state-delete" 0 "crawler=crashed"
  elif docker exec rn-grid-storage-1 test -f /data/storage/lease_checker.state.json; then
    record "expirer-after-state-delete" 0 "crawler=running"
  else
    record "expirer-after-state-delete" 0 "crawler=no-state-written-yet"
  fi

  log "Tier 2 done: $RESULTS"
}

case "$TIER" in
  tier1) tier1 ;;
  tier2) tier2 ;;
  all) tier1; tier2 ;;
  *) echo "usage: $0 tier1|tier2|all" >&2; exit 2 ;;
esac
