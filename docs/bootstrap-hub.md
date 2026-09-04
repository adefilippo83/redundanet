# Bootstrapping the Network Hub (fly.io)

The hub is the network's anchor: the **Tahoe introducer** and the public
**status page**, running as one fly.io machine from
`docker/Dockerfile.bootstrap` (one VM = one network namespace, the same
topology the compose stack builds with `network_mode: service:tinc`).

It was also the Tinc rendezvous node at genesis. It no longer is: fly's proxy
hides peers' real addresses (see "Not a rendezvous" under Notes below), so the
rendezvous role belongs to a plain VPS with a real public IP (`hub-hetzner`),
and the fly hub reaches the mesh through it like any other node.

The hub stores no data. Storage nodes join via the
[join form](https://redundanet.com/join.html) once the hub is up.

## Prerequisites

- `gpg`, `poetry` (repo checkout), and `fly` (authenticated) on your machine
- A fly.io account (costs ≈ $5/mo: shared-cpu-1x + dedicated IPv4 + 1GB volume)

## Runbook

### 1. Local half — identity + manifest (scripted)

```bash
scripts/bootstrap-hub.sh --email hub@redundanet.com
```

This generates the hub's RSA-4096 GPG key, publishes it to the keyservers
(**verifying it is actually fetchable** — the network's own trust rule),
exports the private key to `.bootstrap/hub_gpg_private_key.asc`, and patches
the key's full fingerprint into `manifests/manifest.yaml`.

### 2. fly.io half

```bash
fly apps create redundanet-hub
fly ips allocate-v4                # dedicated IPv4 (~$2/mo) — note the address
```

Put the dedicated IPv4 into `manifests/manifest.yaml` (`public_ip` of
`hub-fly`) and optionally into `fly.toml` (`REDUNDANET_PUBLIC_IP`).

```bash
fly volumes create introducer_data --region fra --size 1
fly secrets set GPG_PRIVATE_KEY_B64="$(base64 < .bootstrap/hub_gpg_private_key.asc)"
```

**Commit and push the manifest now** — the hub clones it at boot to find its
own entry:

```bash
git add manifests/manifest.yaml && git commit -m "Genesis manifest: bootstrap hub" && git push
```

### 3. Deploy and capture the FURL

```bash
fly deploy
fly logs                            # wait for "Published introducer FURL: pb://…"
fly ssh console -C "cat /var/lib/tahoe-introducer/private/introducer.furl"
```

Put that FURL into `manifests/manifest.yaml` (`introducer_furl`), commit,
push. Every joining node now gets the VPN entry point and the introducer from
the manifest alone.

### 4. Verify

```bash
nc -vz <dedicated-ipv4> 655        # tinc port reachable from the internet
fly checks list                    # machine healthcheck: VPN if up + FURL present
```

The real proof is the first storage node joining via the form and a file
round-tripping — until then the grid has an introducer but no storage
(`redundanet validate` warns about that; expected at genesis).

## Adding a second introducer (any public node)

One introducer is a single point of failure for discovery: while it is down,
a restarting node cannot find the storage servers. Any publicly reachable
node can run a second one. Storage nodes announce to every introducer in the
manifest and clients learn servers from all of them, so either can be down.

1. In the manifest give the node the roles `tinc_vpn` and `tahoe_introducer`,
   its `public_ip`, and `is_publicly_accessible: true`. Join it and start it
   with the introducer profile:

   ```bash
   cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
     --profile introducer up -d
   ```

2. Capture the FURL it generated:

   ```bash
   docker exec redundanet-tahoe-introducer cat /var/lib/tahoe-introducer/private/introducer.furl
   ```

3. Put it in that node's manifest entry as `introducer_furl`, commit, push.
   Keep the top-level `introducer_furl` as the primary.

4. Storage and client nodes apply it the next time their Tahoe containers are
   recreated (`redundanet update` on the next release, or a force-recreate of
   the whole stack). Tahoe reads its introducer list only at startup.

Back up the new node's introducer volume the same way as the hub's below: its
tub identity is what the FURL points at.

## Back up the anchor identity (do this immediately)

Losing either of these strands every node until the manifest is re-anchored:

1. **The GPG private key** — `.bootstrap/hub_gpg_private_key.asc` (gitignored).
   Store a copy off this machine (password manager / offline media).
2. **The introducer volume** — holds the tub identity behind the FURL:
   `fly volumes snapshots create <volume-id>` (find the id with
   `fly volumes list`). Snapshot again after any Tahoe upgrade.

## Updating the hub

`fly deploy` builds the image locally and replaces the machine, so it always
runs the current code — it does not depend on the `latest` tag. The introducer
volume (tub identity + FURL) persists across deploys, so the FURL is unchanged
and nodes stay connected.

Note the same network-namespace caveat as member nodes: everything on the hub
runs inside one machine, so a deploy restarts all of it together and there is no
stale-namespace issue to work around here — but if you ever `docker`-manage the
services manually, recreate them as a group, not individually.

## Notes & limitations

- **Not a rendezvous**: the fly hub is `is_publicly_accessible: false` in the
  manifest on purpose. Fly's edge proxy terminates every TCP connection, so
  tincd on the hub sees the proxy's internal address (172.16.x.x) as the
  source of every peer and advertises that as each peer's location. tinc keeps
  the first address it learns for a peer, so with fly as a rendezvous every
  node ends up with an unroutable address for every other node, UDP hole
  punching can never succeed, and all traffic relays through fly over TCP.
  A rendezvous node must see real client addresses: a plain VPS does
  (`hub-hetzner`). The hub connects outward to it like any other node and
  stays reachable over the VPN for the introducer and the status page.
- **UDP**: fly.io's UDP routing needs apps to bind `fly-global-services`,
  which tincd doesn't. Only 655/tcp is exposed; tinc falls back to TCP
  automatically. Fine for a rendezvous/relay hub; if the hub ever needs to
  relay heavy traffic between NAT'd nodes, move it to a plain VPS with
  unproxied TCP+UDP — same image, just `docker run` with
  `--device /dev/net/tun --cap-add NET_ADMIN -p 655:655 -p 655:655/udp`.
- **Encoding**: the genesis manifest ships `shares 1/1/2` so the first storage
  node makes the grid usable. Raise via PR as the fleet grows (affects new
  uploads only).
- **Growing the fleet**: nothing else to configure — the join pipeline adds
  nodes to the manifest, and every node's manifest-sync sidecar (the hub
  included) picks up changes within `SYNC_INTERVAL` (300s).
