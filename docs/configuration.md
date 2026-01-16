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
    vpn_ip: 10.100.0.10         # Same as internal_ip for VPN
    public_ip: 1.2.3.4          # Public IP (optional)
    gpg_key_id: 0xABCD1234      # GPG key identifier
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

### Node Section

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique node identifier |
| `internal_ip` | string | yes | VPN IP address |
| `vpn_ip` | string | yes | VPN IP address (same as internal_ip) |
| `public_ip` | string | no | Public IP for external access |
| `gpg_key_id` | string | yes | GPG key for authentication |
| `region` | string | no | Geographic region |
| `status` | string | no | `pending`, `active`, or `inactive` |
| `roles` | list | no | `tinc_vpn`, `tahoe_storage`, `tahoe_introducer` |
| `storage_contribution` | string | no | Storage to contribute |
| `is_publicly_accessible` | bool | no | Can accept incoming connections |

## Environment Variables

Environment variables override manifest settings and configure runtime behavior.

### Core Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_NODE_NAME` | - | Node identifier |
| `REDUNDANET_CONFIG_DIR` | `~/.config/redundanet` | Config directory |
| `REDUNDANET_DATA_DIR` | `~/.local/share/redundanet` | Data directory |
| `REDUNDANET_DEBUG` | `false` | Enable debug mode |
| `REDUNDANET_LOG_LEVEL` | `INFO` | Logging level |

### Manifest Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_MANIFEST_REPO` | - | Git repository URL |
| `REDUNDANET_MANIFEST_BRANCH` | `main` | Git branch |
| `REDUNDANET_MANIFEST_PATH` | - | Local manifest path |

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
| `manifest` | Manifest files |

### Environment File

Create a `.env` file in the project root:

```bash
NODE_NAME=node-12345678
REDUNDANET_MANIFEST_REPO=https://github.com/adefilippo83/redundanet.git
REDUNDANET_LOG_LEVEL=INFO
```

## CLI Configuration

The CLI reads configuration from multiple sources in order:

1. Command-line arguments (highest priority)
2. Environment variables
3. Config file (`~/.config/redundanet/config.yaml`)
4. Manifest file
5. Default values (lowest priority)

### Config File

```yaml
# ~/.config/redundanet/config.yaml
node_name: node-12345678
log_level: INFO
debug: false
manifest_repo: https://github.com/adefilippo83/redundanet.git
manifest_branch: main
```

## GPG Key Configuration

### Key Requirements

- **Algorithm**: RSA (4096 bits recommended)
- **Publication**: Must be on a public keyserver
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
- Can mount FUSE filesystem
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

### Key Rotation

While GPG keys don't expire by default, consider:
- Generating new keys periodically
- Using subkeys for node authentication
- Having a key revocation plan

### Network Isolation

The Tinc VPN provides:
- Encrypted communication between all nodes
- No direct access to storage without VPN membership
- Protection from network eavesdropping
