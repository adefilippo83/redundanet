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
#   Key ID:      0x1234ABCD5678EF90
#   Fingerprint: ABCD 1234 5678 EF90 ...
#   User ID:     RedundaNet Node my-node <you@example.com>

# Publish your key to public keyservers
redundanet node keys publish --key-id 0x1234ABCD5678EF90
```

### Step 3: Submit Your Application

Visit [redundanet.com/join.html](https://redundanet.com/join.html) and fill out the application:

| Field | Description |
|-------|-------------|
| **GPG Key ID** | The key ID from step 2 (e.g., `0x1234ABCD5678EF90`) |
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
# Clone the repository
git clone https://github.com/adefilippo83/redundanet.git
cd redundanet

# Initialize your node (use the name from your approval)
redundanet init --name node-12345678

# Sync the network manifest
redundanet sync

# Start the services
docker compose up -d

# Check everything is running
redundanet status
```

### Step 6: Verify Connection

```bash
# Check your node status
redundanet status

# Expected output:
# Node: node-12345678
# VPN Status: Connected
# Storage Status: Running
# Network Peers: 5
```

## Path 2: Create a Private Network

### Step 1: Install and Initialize

```bash
pip install redundanet

# Create a new network
redundanet init --create-network --network-name my-org-network

# This creates:
# - Configuration directories
# - Initial manifest file
# - GPG key for your node
```

### Step 2: Configure the Manifest

Edit the generated manifest file (`manifests/manifest.yaml`):

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
    roles: [introducer, storage]
    storage_contribution: 500GB
```

### Step 3: Start the Introducer

On your first node (which runs the introducer):

```bash
docker compose --profile introducer --profile storage up -d
```

### Step 4: Add More Nodes

On additional nodes:

```bash
# Install CLI
pip install redundanet

# Generate GPG key
redundanet node keys generate --name node-2 --email node2@myorg.com

# Initialize with your network's manifest repo
redundanet init --manifest-repo https://github.com/myorg/network-manifest.git

# Sync and start
redundanet sync
docker compose --profile storage up -d
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

### Mount as Filesystem (Advanced)

```bash
# Mount the Tahoe filesystem
redundanet storage mount /mnt/redundanet

# Access files through the mount point
ls /mnt/redundanet
cp /mnt/redundanet/myfile.txt ./

# Unmount when done
redundanet storage unmount /mnt/redundanet
```

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

1. **Check storage nodes** - Need at least 7 online for successful uploads
2. **Check client logs**: `docker compose logs tahoe-client`
3. **Verify connection**: `redundanet network peers`

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
