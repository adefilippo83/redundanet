# Configuration Reference

RedundaNet configuration is managed through a YAML manifest file, environment variables, and the CLI.

## Manifest File

The manifest file defines the network configuration and node information. It's typically stored in a Git repository for version control and distribution.

### Location

The manifest is stored at:
- **Public network**: `manifests/manifest.yaml` in the main repository
- **Private networks**: Your own Git repository

### Full Schema

```yaml
# Network configuration
network:
  name: redundanet              # Network identifier
  version: "2.0.0"              # Manifest version
  domain: redundanet.local      # Domain for internal DNS
  vpn_network: 10.100.0.0/16    # VPN address range

  # Tahoe-LAFS settings
  tahoe:
    shares_needed: 3            # Minimum shares to reconstruct (k)
    shares_happy: 7             # Minimum shares for upload success
    shares_total: 10            # Total shares to create (n)
    reserved_space: 1G          # Reserved space per storage node

# Introducer FURL (auto-populated by introducer node)
introducer_furl: pb://...

# Node definitions
nodes:
  - name: node-12345678         # Unique node name
    internal_ip: 10.100.0.10    # VPN IP address
    vpn_ip: 10.100.0.10         # Optional; defaults to internal_ip
    public_ip: 1.2.3.4          # Public IP (optional)
    # GPG key identifier: the full 40-character fingerprint, no 0x prefix
    # (short 8/16-char ids are rejected).
    gpg_key_id: 1234567890ABCDEF1234567890ABCDEF12345678
    region: north-america       # Geographic region
    status: active              # Node status (pending/active/inactive)
    roles:                      # Node roles
      - tinc_vpn
      - tahoe_storage
    ports:
      tinc: 655
      tahoe_storage: 3457
      tahoe_client: 3456
      tahoe_introducer: 3458
    storage_contribution: 500GB # Storage offered to network
    is_publicly_accessible: true # Can accept incoming connections
    # tahoe_introducer nodes only: the FURL of the introducer this node runs.
    # Clients use every introducer in the manifest, so either can be down.
    introducer_furl: pb://...
```

### Network Section

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | required | Network identifier |
| `version` | string | required | Manifest version |
| `domain` | string | `redundanet.local` | Internal DNS domain |
| `vpn_network` | string | `10.100.0.0/16` | VPN CIDR range |

### Tahoe Section

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `shares_needed` | int | `3` | Minimum shares for reconstruction (k) |
| `shares_happy` | int | `7` | Minimum shares for successful upload |
| `shares_total` | int | `10` | Total shares to create (n) |
| `reserved_space` | string | `1G` | Reserved space per node |

**Understanding Erasure Coding:**
- With `shares_needed: 3` and `shares_total: 10`:
  - Each file is split into 10 encrypted shares
  - Any 3 shares can reconstruct the original file
  - 7 nodes can fail and data is still recoverable

**Changing the encoding:** parameters are baked into each file at upload
time, so a manifest change affects only new uploads. Existing files are
converged automatically: every client node runs a rebalance loop (enabled by
default) that detects files carrying old parameters and re-encodes them from
the grid itself — serially, rate-limited, resuming across cycles. After a
change, update each node's `.env` (re-run `network join`) and recreate the
tahoe containers; then watch the status page's census climb to the new
target. Tunables in `.env`: `REBALANCE_ENABLED` (default `true`),
`REBALANCE_INTERVAL` (default 86400s). Old shares stop being lease-renewed
once replaced and are reclaimed by garbage collection. Files held only as
bare `URI:` capabilities are not reachable by the loop and must be
re-uploaded by their owner.

### Node Section

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique node identifier |
| `internal_ip` | string | yes | VPN IP address |
| `vpn_ip` | string | no | Defaults to `internal_ip` |
| `public_ip` | string | no | Public IP for external access |
| `gpg_key_id` | string | yes | The full 40-char hex fingerprint, no `0x` prefix (short 8/16-char ids are rejected) |
| `region` | string | no | Geographic region |
| `status` | string | no | `pending`, `active`, or `inactive` |
| `roles` | list | no | `tinc_vpn`, `tahoe_storage`, `tahoe_introducer`, `tahoe_client` |
| `storage_contribution` | string | no | Storage to contribute |
| `is_publicly_accessible` | bool | no | Can accept incoming connections |
| `introducer_furl` | string | no | `tahoe_introducer` nodes only: the FURL this introducer publishes. Storage nodes announce to, and clients learn servers from, every introducer in the manifest (the top-level `introducer_furl` plus these), so the grid survives losing one. See the bootstrap hub runbook for adding a second introducer. |

## Environment Variables

Environment variables override manifest settings and configure runtime behavior.

### Core Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_NODE_NAME` | - | Node identifier |
| `REDUNDANET_CONFIG_DIR` | `/etc/redundanet` | Config directory (holds the persisted `.env` written by `init`) |
| `REDUNDANET_DATA_DIR` | `/var/lib/redundanet` | Data directory (synced manifest, repo clone) |
| `REDUNDANET_SECRETS_DIR` | `/opt/redundanet/docker/secrets` | Where `node keys generate` exports the private key |
| `REDUNDANET_DEBUG` | `false` | Enable debug mode |
| `REDUNDANET_LOG_LEVEL` | `INFO` | Logging level |

### Manifest Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_MANIFEST_REPO` | - | Git repository URL |
| `REDUNDANET_MANIFEST_BRANCH` | `main` | Git branch |
| `REDUNDANET_MANIFEST_FILENAME` | `manifest.yaml` | Manifest file name inside the repo's `manifests/` dir |
| `REDUNDANET_SYNC_INTERVAL` | `300` | Seconds between manifest re-syncs in the tinc container (see below) |

The tinc container runs a manifest-sync sidecar: every `REDUNDANET_SYNC_INTERVAL`
seconds it re-syncs the manifest repository, refreshes the Tinc peer host files
(adding newly joined nodes, removing revoked ones), and reloads tincd — so
membership changes reach running nodes without a restart. Set it via
`SYNC_INTERVAL` in the compose `.env`.

### Deployment Settings (host CLI)

The `redundanet network`/`storage` commands drive the docker-compose stack;
these tell the CLI where it is:

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_COMPOSE_FILE` | auto-detected | Path to `docker-compose.yml` |
| `REDUNDANET_COMPOSE_PROJECT` | `redundanet` | Compose project name |
| `REDUNDANET_COMPOSE_ENV_FILE` | `/opt/redundanet/.env` | Compose env file |

### Tahoe Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_SHARES_NEEDED` | `3` | k value |
| `REDUNDANET_SHARES_HAPPY` | `7` | Happy threshold |
| `REDUNDANET_SHARES_TOTAL` | `10` | n value |
| `REDUNDANET_RESERVED_SPACE` | `1G` | Reserved space |
| `REDUNDANET_INTRODUCER_FURL` | - | Introducer FURL |

### VPN Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `TINC_PORT` | `655` | Tinc VPN port |

## Docker Compose Configuration

### Profiles

RedundaNet uses Docker Compose profiles to enable different node configurations:

```bash
# Storage node (contributes storage)
docker compose --profile storage up -d

# Client node (uses storage)
docker compose --profile client up -d

# Introducer node (network coordinator)
docker compose --profile introducer up -d

# Multiple profiles (e.g., introducer that also stores)
docker compose --profile introducer --profile storage up -d
```

### Volumes

| Volume | Purpose |
|--------|---------|
| `tinc-config` | Tinc VPN configuration |
| `tahoe-introducer` | Introducer state |
| `tahoe-storage` | Storage node state |
| `tahoe-client` | Client state |
| `storage-data` | Actual stored data |
| `manifest` | Manifest files (shared; also carries the introducer FURL) |
| `logs` | Service logs |

### Environment File

`redundanet network join` generates `/opt/redundanet/.env` from your manifest
entry — you normally never write it by hand. It looks like:

```bash
NODE_NAME=node-12345678
VPN_IP=10.100.0.10
PUBLIC_IP=auto
GPG_KEY_ID=1234567890ABCDEF1234567890ABCDEF12345678
GPG_KEY_FILE=/opt/redundanet/docker/secrets/gpg_private_key.asc
MANIFEST_REPO=https://github.com/adefilippo83/redundanet.git
MANIFEST_BRANCH=main
TINC_PORT=655
SHARES_NEEDED=3
SHARES_HAPPY=7
SHARES_TOTAL=10
RESERVED_SPACE=1G
```

Pass it to every compose command:
`docker compose --env-file /opt/redundanet/.env --profile storage up -d`.

## CLI Configuration

The CLI reads configuration from these sources, in decreasing precedence:

1. Command-line arguments
2. `REDUNDANET_*` environment variables
3. The persisted node config `<config_dir>/.env` (default
   `/etc/redundanet/.env`, written by `redundanet init`)
4. A `.env` file in the current directory
5. Built-in defaults

There is no YAML config file — persistent settings live in the `.env` written
by `init`, using the same `REDUNDANET_*` names as the environment variables.

## GPG Key Configuration

### Key Requirements

- **Algorithm**: RSA (4096 bits recommended) — the GPG key doubles as the
  node's Tinc transport key, and Tinc requires RSA
- **No passphrase** on the node key (the raw RSA parameters are needed to
  derive the Tinc key; `redundanet node keys generate` does this correctly)
- **Publication**: Must be on a public keyserver
- **Identity in the manifest**: use the full 40-character fingerprint as
  `gpg_key_id` — peers verify fetched keys against it (fingerprint pinning),
  and short ids are collision-prone
- **Supported keyservers**:
  - keys.openpgp.org (recommended)
  - keyserver.ubuntu.com
  - pgp.mit.edu

### Managing Keys with CLI

```bash
# Generate a new key
redundanet node keys generate --name my-node --email me@example.com

# List all keys
redundanet node keys list

# Publish to keyservers
redundanet node keys publish --key-id 0x12345678

# Fetch a key from keyservers
redundanet node keys fetch --key-id 0x12345678

# Export public key to file
redundanet node keys export --key-id 0x12345678 --output my-key.asc

# Import a public key from file
redundanet node keys import --input peer-key.asc
```

## Data Retention: Leases & Garbage Collection

Every share on a storage node carries a **lease**. Storage nodes
garbage-collect shares whose lease has not been renewed within the lease
duration — that is how deleting data eventually frees disk space.

| Variable (storage node) | Default | Meaning |
|----------|---------|-------------|
| `EXPIRE_ENABLED` | `true` | Collect shares with lapsed leases |
| `LEASE_DURATION` | `90 days` | How long an unrenewed share survives |

**Keeping data alive and healthy:**

- The client container sweeps **all aliases** automatically once a week (the
  `lease-renew` job): every object gets its leases renewed, and any object
  with missing shares is repaired from the surviving ones (Tahoe
  `deep-check --add-lease --repair`). Repair covers the cases where a storage
  node lost its disk or a member left the network; it needs at least k
  surviving shares, below that an object is unrecoverable. The per-alias
  summary (objects checked, healthy/unhealthy before and after, repairs
  attempted/successful) is in the client's logs under `lease-repair:`.
  Overrides in `.env`: `LEASE_RENEW_INTERVAL` (seconds, default 604800),
  `REPAIR_ENABLED=false` to only renew.
- Manual renewal: `redundanet storage renew` (all aliases) or
  `redundanet storage renew URI:CHK:...` / `redundanet storage renew home:`.
- **Bare capabilities that are not linked into an alias are not renewed
  automatically** — keep long-lived data under an alias
  (`redundanet storage mkdir`), or renew such caps yourself more often than
  the lease duration.

The 90-day default means a client can be offline for ~12 weekly renewal
cycles before its data is at risk.

## Role Definitions

### Introducer

- Runs Tahoe-LAFS introducer service
- Only one needed per network (can have backups)
- Coordinates storage node discovery
- Does not have access to stored data

### Storage

- Runs Tahoe-LAFS storage service
- Contributes disk space to network
- Stores encrypted data shares
- Cannot decrypt the data it stores

### Client

- Runs Tahoe-LAFS client service
- Can upload and download files
- Encrypts data before sending to network

## Network Topology

### VPN Network

By default, RedundaNet uses `10.100.0.0/16`:
- First 10 IPs reserved for infrastructure
- Nodes assigned sequentially from `.10`
- Example: `10.100.0.10`, `10.100.0.11`, etc.

### Node Discovery

1. New node joins the Tinc mesh via existing nodes
2. Node announces itself to the Tahoe introducer
3. Introducer shares node list with clients
4. Clients can now use the storage node

## Security Considerations

### Secrets Management

- GPG private keys should never be committed to Git
- Use Docker secrets or environment variables for sensitive data
- Consider hardware security modules (HSM) for production

### Key Backup (do this when you join)

Your node's GPG key **is its identity** — the VPN transport key is derived
from it. If the disk dies and you have no backup, the identity is gone and
the only path back is rejoining as a new node.

The private key lives in two places after `redundanet node keys generate`:

- the exported file: `/opt/redundanet/docker/secrets/gpg_private_key.asc`
- your GPG keyring (`gpg --list-secret-keys`)

Back it up **off the node** (password manager or offline media):

```bash
gpg --armor --export-secret-keys YOUR_FINGERPRINT > redundanet-node-key.asc
# store redundanet-node-key.asc somewhere safe, then shred the local copy
```

### Restore (new hardware, same identity)

```bash
redundanet init --name node-XXXXXXXX          # your existing node name
gpg --import redundanet-node-key.asc
mkdir -p /opt/redundanet/docker/secrets
cp redundanet-node-key.asc /opt/redundanet/docker/secrets/gpg_private_key.asc
chmod 600 /opt/redundanet/docker/secrets/gpg_private_key.asc
redundanet network join --repo <manifest-repo> --name node-XXXXXXXX
cd /opt/redundanet/docker && docker compose --env-file /opt/redundanet/.env --profile storage up -d
```

No manifest change is needed — the identity (and therefore the VPN key) is
unchanged.

### Key Rotation (compromise or precaution)

1. Generate and publish a new key:
   `redundanet node keys generate --name node-XXXXXXXX --email you@example.com`
   then `redundanet node keys publish --key-id NEW_FINGERPRINT`
   (publish verifies the key is actually fetchable before reporting success)
2. Open a PR updating your node's `gpg_key_id` in the manifest to the new
   **full 40-character fingerprint**
3. Once merged, every running peer picks up the new key within
   `SYNC_INTERVAL` (default 300s) via the manifest-sync sidecar — no peer
   restarts needed
4. Restart your own tinc container so it derives its transport key from the
   new secret:
   `docker compose --env-file /opt/redundanet/.env restart tinc`

During the window between merge and your restart, peers expect the new key
while your node still presents the old one — plan for a few minutes of VPN
downtime for your node.

### Revocation

Removing a node's entry from the manifest revokes it: within `SYNC_INTERVAL`
every peer deletes its Tinc host file and reloads, refusing further
connections. Optionally also publish a GPG revocation certificate for the key
itself.

### Network Isolation

The Tinc VPN provides:
- Encrypted communication between all nodes
- No direct access to storage without VPN membership
- Protection from network eavesdropping
