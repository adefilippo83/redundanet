# Configuration Reference

RedundaNet configuration is managed through a YAML manifest file and environment variables.

## Manifest File

The manifest file defines the network configuration and node information. It's typically stored in a Git repository for version control and distribution.

### Full Schema

```yaml
# Network configuration
network:
  name: my-network              # Network identifier
  version: "1.0.0"              # Manifest version
  domain: redundanet.local      # Domain for internal DNS
  vpn_network: 10.100.0.0/16    # VPN address range

# Tahoe-LAFS configuration
tahoe:
  shares_needed: 3              # Minimum shares to reconstruct (k)
  shares_happy: 7               # Minimum shares for upload success
  shares_total: 10              # Total shares to create (n)
  reserved_space: 50G           # Reserved space per storage node
  introducer_furl: pb://...     # Introducer FURL (auto-populated)

# Node definitions
nodes:
  - name: node1                 # Unique node name
    internal_ip: 192.168.1.10   # LAN IP address
    vpn_ip: 10.100.0.1          # VPN IP address
    public_ip: 1.2.3.4          # Public IP (optional)
    gpg_key_id: ABCD1234        # GPG key identifier
    roles:                      # Node roles
      - introducer
      - storage
    storage_contribution: 500GB # Storage offered to network
    storage_allocation: 1TB     # Maximum storage usage
    metadata:                   # Optional metadata
      region: us-east
      owner: user@example.com

  - name: node2
    internal_ip: 192.168.2.20
    vpn_ip: 10.100.0.2
    roles:
      - storage
      - client
    storage_contribution: 1TB
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
| `shares_needed` | int | `3` | Minimum shares for reconstruction |
| `shares_happy` | int | `7` | Minimum shares for successful upload |
| `shares_total` | int | `10` | Total shares to create |
| `reserved_space` | string | `50G` | Reserved space per node |
| `introducer_furl` | string | auto | Introducer capability URL |

### Node Section

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique node identifier |
| `internal_ip` | string | yes | LAN IP address |
| `vpn_ip` | string | yes | VPN IP address |
| `public_ip` | string | no | Public IP for external access |
| `gpg_key_id` | string | no | GPG key for authentication |
| `roles` | list | no | Node roles (`introducer`, `storage`, `client`, `gateway`) |
| `storage_contribution` | string | no | Storage to contribute |
| `storage_allocation` | string | no | Maximum storage usage |
| `metadata` | object | no | Custom metadata |

## Environment Variables

Environment variables override manifest settings and configure runtime behavior.

### Core Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_NODE_NAME` | - | Node identifier |
| `REDUNDANET_INTERNAL_VPN_IP` | - | VPN IP address |
| `REDUNDANET_PUBLIC_IP` | auto | Public IP (auto-detected) |
| `REDUNDANET_GPG_KEY_ID` | - | GPG key identifier |
| `REDUNDANET_LOG_LEVEL` | `INFO` | Logging level |
| `REDUNDANET_DEBUG` | `false` | Enable debug mode |

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
| `REDUNDANET_RESERVED_SPACE` | `50G` | Reserved space |
| `REDUNDANET_INTRODUCER_FURL` | - | Introducer FURL |

### VPN Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `TINC_PORT` | `655` | Tinc VPN port |

### Path Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `REDUNDANET_CONFIG_DIR` | `~/.config/redundanet` | Config directory |
| `REDUNDANET_DATA_DIR` | `~/.local/share/redundanet` | Data directory |

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

# Multiple profiles
docker compose --profile storage --profile client up -d
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
| `logs` | Application logs |

### Secrets

GPG private key should be mounted as a secret:

```yaml
services:
  tinc:
    volumes:
      - ./secrets/gpg_private_key.asc:/run/secrets/gpg_private_key:ro
```

## CLI Configuration

The CLI reads configuration from multiple sources in order:

1. Command-line arguments
2. Environment variables
3. Config file (`~/.config/redundanet/config.yaml`)
4. Manifest file
5. Default values

### Config File

```yaml
# ~/.config/redundanet/config.yaml
node_name: my-node
log_level: DEBUG
manifest_path: /path/to/manifest.yaml
```

## Role Definitions

### Introducer

- Runs Tahoe-LAFS introducer service
- Only one needed per network
- Coordinates storage node discovery

### Storage

- Runs Tahoe-LAFS storage service
- Contributes disk space to network
- Stores encrypted data shares

### Client

- Runs Tahoe-LAFS client service
- Can upload/download files
- Can mount FUSE filesystem

### Gateway

- Provides external access to network
- Typically has public IP
- May run web interface
