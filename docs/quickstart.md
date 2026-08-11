# Quick Start Guide

Get your RedundaNet node up and running in minutes.

## Overview

There are two paths to joining RedundaNet:

1. **Join the public network** - Apply to become a node in the existing network
2. **Create a private network** - Start your own RedundaNet for your organization

## Path 1: Join the Public Network

### Step 1: Install the CLI

```bash
pip install redundanet
```

### Step 2: Generate and Publish Your GPG Key

Your GPG key is your node's identity. It must be published to a public keyserver for verification.

```bash
# Generate a new GPG key for your node
redundanet node keys generate --name my-node --email you@example.com

# You'll see output like:
#   Key ID:      1234ABCD5678EF90
#   Fingerprint: ABCD1234...5678EF90
#   User ID:     RedundaNet Node my-node <you@example.com>

# Publish your key to public keyservers
redundanet node keys publish --key-id 1234ABCD5678EF90
```

### Step 3: Submit Your Application

Visit [redundanet.com/join.html](https://redundanet.com/join.html) and fill out the application:

| Field | Description |
|-------|-------------|
| **GPG Key ID** | The key ID from step 2 (e.g., `1234ABCD5678EF90`; the full 40-character fingerprint is preferred) |
| **Storage Contribution** | How much disk space you'll share (e.g., 100GB) |
| **Region** | Your geographic location |
| **Device Type** | Raspberry Pi, server, VPS, etc. |
| **Public IP** | Optional - only if your node is publicly accessible |

Submitting the form creates a GitHub issue that's automatically processed.

### Step 4: Wait for Approval

A maintainer will:
1. Verify your GPG key exists on keyservers
2. Review your application
3. Merge the PR that adds your node to the manifest

You'll receive a comment on the issue with your assigned node name and VPN IP.

### Step 5: Set Up Your Node

Once your application is approved:

```bash
# Initialize your node (use the name from your approval)
redundanet init --name node-12345678

# Join the network. This clones the manifest repository, installs the docker
# files to /opt/redundanet, and generates /opt/redundanet/.env from your
# manifest entry (node name, VPN IP, GPG key, tahoe encoding parameters).
redundanet network join --repo https://github.com/adefilippo83/redundanet.git --name node-12345678

# Start the services (storage node). The profile picks your role; the env file
# is required — without it the containers have no node identity.
cd /opt/redundanet/docker
docker compose --env-file /opt/redundanet/.env --profile storage up -d

# Check everything is running
redundanet status
```

### Step 6: Verify Connection

```bash
# Service status (tinc + tahoe containers) and VPN interface
redundanet status

# VPN connection details and reachable peers
redundanet network status
redundanet network peers
```

`redundanet status` lists each service's state/health and the VPN interface IP.
`redundanet network peers` pings every node in the manifest over the VPN and shows
which are online.

## Path 2: Create a Private Network

### Step 1: Install and Initialize

```bash
pip install redundanet

# Generate your node's GPG identity
redundanet node keys generate --name node-primary --email you@myorg.com

# Initialize the node for your new network
redundanet init --name node-primary --network my-org-network

# This creates the local configuration and data directories.
```

### Step 2: Create the Manifest

`init` does not create a manifest — write one for your network (start from
`manifests/example.yaml` in the repository) and keep it in a Git repository
your nodes can pull from:

```yaml
network:
  name: my-org-network
  version: "1.0.0"
  domain: mynetwork.local
  vpn_network: 10.100.0.0/16
  tahoe:
    shares_needed: 3
    shares_happy: 5
    shares_total: 7

nodes:
  - name: node-primary
    internal_ip: 192.168.1.10
    vpn_ip: 10.100.0.1
    gpg_key_id: YOUR_KEY_ID
    roles:
      - tinc_vpn
      - tahoe_introducer
      - tahoe_storage
    storage_contribution: 500GB
```

### Step 3: Start the Introducer

On your first node (which runs the introducer), join against your manifest
repository — this generates the `/opt/redundanet/.env` the containers need —
then start with the introducer profile:

```bash
redundanet network join --repo https://github.com/myorg/network-manifest.git --name node-primary
cd /opt/redundanet/docker
docker compose --env-file /opt/redundanet/.env --profile introducer --profile storage up -d
```

### Step 4: Add More Nodes

On additional nodes:

```bash
# Install CLI
pip install redundanet

# Generate GPG key
redundanet node keys generate --name node-2 --email node2@myorg.com

# Initialize and join with your network's manifest repo
redundanet init --name node-2 --manifest-repo https://github.com/myorg/network-manifest.git
redundanet network join --repo https://github.com/myorg/network-manifest.git --name node-2

# Start
cd /opt/redundanet/docker
docker compose --env-file /opt/redundanet/.env --profile storage up -d
```

## Using Storage

Once your node is connected, you can upload and download files.

### Upload a File

```bash
redundanet storage upload /path/to/file.txt

# Output:
# Uploading file.txt...
# Success! Capability: URI:CHK:abc123...
```

Save the capability string - you'll need it to download the file.

### Download a File

```bash
redundanet storage download URI:CHK:abc123... /path/to/output.txt
```

### Organize files in directories

Tahoe addresses files by capability, but you can group them into a named,
browsable directory (an *alias*) so you don't have to track raw caps:

```bash
# Create a directory aliased "home"
redundanet storage mkdir home

# Upload files into it (instead of getting back a bare capability)
redundanet storage upload report.pdf home:report.pdf
redundanet storage upload notes.txt  home:notes.txt

# List what's in the directory
redundanet storage ls home:
redundanet storage ls --long home:

# Show your directories and their capabilities
redundanet storage aliases

# Download by name
redundanet storage download home:report.pdf ./report.pdf
```

Share a whole directory by giving someone its capability (`URI:DIR2:...` from
`storage aliases`).

### Mount as a Filesystem (SFTP)

The client node can expose a directory over **SFTP**, turning the grid into a
normal remote filesystem you can browse, mount, or serve as WebDAV. (This
replaces the FUSE mounting that Tahoe-LAFS removed in 1.20.)

**Enable it on the client node** (one time): add to `/opt/redundanet/.env`

```bash
SFTP_ENABLED=true
SFTP_BIND=0.0.0.0        # LAN access; omit or 127.0.0.1 for loopback-only
SFTP_PORT=8022
```

then apply it: `redundanet update` (or recreate the stack). The client
generates an SFTP host key and starts the server on port 8022.

> **Security:** bind SFTP to your LAN only — never port-forward 8022 to the
> internet. An SFTP account grants full read/write to its directory subtree.

**Grant a user access** with their SSH public key:

```bash
redundanet storage sftp adduser --user alice ~/alice_id_ed25519.pub
redundanet storage sftp listusers
```

This maps the key to a dedicated `sftp:` directory (created automatically).

**Connect** from any machine on the node's LAN:

```bash
# browse / transfer
sftp -P 8022 alice@<node-lan-ip>

# mount as a drive (Linux/macOS)
sshfs -p 8022 alice@<node-lan-ip>:/ /mnt/grid

# expose as WebDAV (no extra server code — rclone bridges SFTP to WebDAV)
rclone serve webdav :sftp:host=<node-lan-ip>,port=8022,user=alice
```

Files written through the mount are encrypted and erasure-coded across the
network exactly like `storage upload` — the SFTP layer is just a friendlier
front door. Expect higher latency than a local disk (every operation is
encrypt + erasure-code + distribute), so it suits archival and browsing more
than a hot working directory.

## Node Roles

When starting services, you can choose different roles:

| Role | Command | Description |
|------|---------|-------------|
| **Storage** | `--profile storage` | Contributes disk space to the network |
| **Client** | `--profile client` | Can upload/download but doesn't store |
| **Introducer** | `--profile introducer` | Coordinates storage node discovery |

Examples:
```bash
# Storage node (most common)
docker compose --profile storage up -d

# Client only (uses network storage without contributing)
docker compose --profile client up -d

# Introducer + storage (for network operators)
docker compose --profile introducer --profile storage up -d
```

## Troubleshooting

### VPN Won't Connect

1. **Check firewall** - Port 655 (TCP/UDP) must be open
2. **Verify GPG key** - Must be published to keyserver
3. **Check logs**: `docker compose logs tinc`

### Storage Node Not Appearing

1. **Check introducer** - Is the introducer running?
2. **Check logs**: `docker compose logs tahoe-storage`
3. **Sync manifest**: `redundanet sync`

### Can't Upload Files

1. **Check storage nodes** - Enough servers must be online to satisfy
   `shares.happy` (see the network's manifest)
2. **Check client logs**: `docker compose logs tahoe-client`
3. **Verify connection**: `redundanet network peers`

### Client shows "0 shares" / "no recoverable versions" after an update

The `tahoe-storage`, `tahoe-client`, and `tahoe-introducer` containers share the
**`tinc` container's network namespace** (`network_mode: service:tinc`) — that is
how they reach the VPN. If the `tinc` container is **recreated** (for example by
`docker compose pull` fetching a new image), the tahoe containers stay attached
to the *old*, now-deleted namespace and lose all network — the client then
reports `0 shares` or `no recoverable versions` even though the data is safe on
disk.

Whenever the tinc image changes, recreate the tahoe containers so they rejoin the
current namespace:

```bash
cd /opt/redundanet/docker
docker compose --env-file /opt/redundanet/.env --profile storage --profile client \
  up -d --force-recreate
```

A plain `docker restart tahoe-client` will **fail** in this state
(`joining network namespace ... No such container`) — use `up -d --force-recreate`
(or a full `down` then `up`). After the tinc containers restart, allow a minute
for the mesh to reconverge before the client reconnects to the grid.

### General Debugging

```bash
# View all logs
docker compose logs -f

# Check network connectivity
redundanet network peers

# Detailed status
redundanet status --verbose

# Validate manifest
redundanet validate manifests/manifest.yaml
```

## Next Steps

- [Configuration Reference](configuration.md) - Customize your setup
- [Architecture Overview](architecture.md) - Understand how RedundaNet works
- [GitHub Repository](https://github.com/adefilippo83/redundanet) - Contribute to the project
