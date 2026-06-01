#!/usr/bin/env bash
#
# Two-node GPG-authenticated Tinc mesh test.
#
# Generates two GPG (RSA) keys, wires them into a manifest, starts two tinc
# nodes that each reuse their GPG key as their Tinc key, and verifies the VPN
# mesh actually forms by pinging across it. This exercises the GPG-only
# transport auth end to end.
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PROJECT="rn-mesh"
COMPOSE=(docker compose -p "$PROJECT" -f docker/docker-compose.mesh-test.yml)
BASE="$REPO_ROOT/docker/mesh-test"
BUILD="${BUILD:-1}"
KEEP="${KEEP:-0}"

WORKDIR="$(mktemp -d)"
export GNUPGHOME="$WORKDIR/gh"

log()  { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }

cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "Mesh test failed (exit $rc). Recent logs:"
    "${COMPOSE[@]}" logs --tail=50 || true
  fi
  if [ "$KEEP" = "1" ]; then
    log "KEEP=1 — leaving stack up. Tear down: docker compose -p $PROJECT -f docker/docker-compose.mesh-test.yml down -v"
  else
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR" "$BASE" 2>/dev/null || true
  return $rc
}
trap cleanup EXIT

gen_key() {
  local email="$1"
  cat > "$WORKDIR/params" <<EOF
%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: RedundaNet Mesh Node
Name-Email: $email
Expire-Date: 0
%commit
EOF
  gpg --batch --gen-key "$WORKDIR/params" 2>/dev/null
  gpg --list-keys --with-colons "$email" | awk -F: '/^fpr:/{print $10; exit}'
}

log "Step 1/4 — Generate two GPG (RSA) node keys"
mkdir -p "$GNUPGHOME"; chmod 700 "$GNUPGHOME"
FPR_A="$(gen_key node-a@mesh.local)"
FPR_B="$(gen_key node-b@mesh.local)"
ok "node-a key: $FPR_A"
ok "node-b key: $FPR_B"

log "Step 2/4 — Lay out manifest, public keys, and private-key secrets"
rm -rf "$BASE"; mkdir -p "$BASE/manifest/gpg" "$BASE/secrets"
gpg --armor --export "$FPR_A" > "$BASE/manifest/gpg/$FPR_A.asc"
gpg --armor --export "$FPR_B" > "$BASE/manifest/gpg/$FPR_B.asc"
gpg --batch --pinentry-mode loopback --armor --export-secret-keys "$FPR_A" > "$BASE/secrets/a.asc"
gpg --batch --pinentry-mode loopback --armor --export-secret-keys "$FPR_B" > "$BASE/secrets/b.asc"
cat > "$BASE/manifest/manifest.yaml" <<EOF
network:
  name: redundanet
  version: "2.0.0"
  domain: redundanet.local
  vpn_network: 10.100.0.0/16
introducer_furl: null
nodes:
  - name: node-a
    internal_ip: 10.100.0.10
    vpn_ip: 10.100.0.10
    public_ip: tinc-a
    gpg_key_id: $FPR_A
    is_publicly_accessible: true
    roles: [tinc_vpn]
    ports: {tinc: 655}
  - name: node-b
    internal_ip: 10.100.0.11
    vpn_ip: 10.100.0.11
    public_ip: tinc-b
    gpg_key_id: $FPR_B
    is_publicly_accessible: true
    roles: [tinc_vpn]
    ports: {tinc: 655}
EOF
ok "manifest + 2 public keys + 2 private-key secrets written"

if [ "$BUILD" = "1" ]; then
  log "Building tinc image"
  "${COMPOSE[@]}" build
fi

log "Step 3/4 — Start both nodes and wait for the VPN interface"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
"${COMPOSE[@]}" up -d
for svc in rn-mesh-a rn-mesh-b; do
  waited=0
  until docker exec "$svc" ip link show redundanet >/dev/null 2>&1; do
    [ "$waited" -ge 60 ] && { fail "$svc: redundanet interface never came up"; exit 1; }
    sleep 2; waited=$((waited + 2))
  done
  ok "$svc: redundanet interface up"
done

log "Step 4/4 — Ping across the GPG-authenticated mesh"
# Give tinc a moment to establish the peer connection.
ab=""; ba=""
for attempt in $(seq 1 20); do
  if [ -z "$ab" ] && docker exec rn-mesh-a ping -c 1 -W 2 10.100.0.11 >/dev/null 2>&1; then ab=1; fi
  if [ -z "$ba" ] && docker exec rn-mesh-b ping -c 1 -W 2 10.100.0.10 >/dev/null 2>&1; then ba=1; fi
  [ -n "$ab" ] && [ -n "$ba" ] && break
  sleep 2
done
[ -n "$ab" ] || { fail "node-a could not reach node-b (10.100.0.11) over the VPN"; exit 1; }
ok "node-a -> node-b (10.100.0.11) reachable over the VPN"
[ -n "$ba" ] || { fail "node-b could not reach node-a (10.100.0.10) over the VPN"; exit 1; }
ok "node-b -> node-a (10.100.0.10) reachable over the VPN"

log "SUCCESS — two GPG-keyed nodes formed an authenticated Tinc mesh 🎉"
