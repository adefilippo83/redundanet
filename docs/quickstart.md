# Quick Start Guide

Get your RedundaNet node up and running in minutes.

## Step 1: Installation

```bash
pip install redundanet
```

## Step 2: Generate GPG Key

If you don't already have a GPG key:

```bash
gpg --full-generate-key
# Follow prompts:
# - Key type: RSA and RSA (default)
# - Key size: 4096
# - Expiration: 0 (doesn't expire) or your preference
# - Name and email
# - Passphrase (remember this!)
```

Upload your key to a keyserver:

```bash
gpg --keyserver keyserver.ubuntu.com --send-keys YOUR_KEY_ID
```

## Step 3: Initialize Your Node

Run the interactive setup:

```bash
redundanet init
```

You'll be prompted for:
- **Node name**: Unique identifier for your node (e.g., `my-home-server`)
- **VPN IP**: Internal VPN address (e.g., `10.100.0.10`)
- **Public IP**: Your public IP (optional, for publicly accessible nodes)
- **GPG Key ID**: Your GPG key identifier
- **Storage contribution**: Amount of storage to contribute (e.g., `500GB`)

## Step 4: Join a Network

### Joining an Existing Network

If you're joining an existing RedundaNet network:

```bash
redundanet network join --manifest-repo https://github.com/org/network-manifest.git
```

### Creating a New Network

If you're starting a new network:

```bash
# Create a new manifest
redundanet init --create-network --network-name my-network
```

## Step 5: Start Services

### Using Docker Compose

```bash
cd project-earthgrid

# Start as a storage node (contributes storage)
docker compose -f docker/docker-compose.yml --profile storage up -d

# Or start as a client only (uses storage)
docker compose -f docker/docker-compose.yml --profile client up -d

# Or start as an introducer (network coordinator - only one needed per network)
docker compose -f docker/docker-compose.yml --profile introducer up -d
```

### Environment Variables

Create a `.env` file:

```bash
NODE_NAME=my-node
VPN_IP=10.100.0.10
PUBLIC_IP=1.2.3.4
GPG_KEY_ID=ABCD1234
MANIFEST_REPO=https://github.com/org/network-manifest.git
```

## Step 6: Verify Connection

Check your node status:

```bash
redundanet status
```

Expected output:
```
Node: my-node
VPN Status: Connected
Storage Status: Running
Network Peers: 5
Storage Capacity: 500GB
Storage Used: 120GB
```

## Using Storage

### Upload a File

```bash
redundanet storage upload /path/to/file.txt
# Returns a capability string like: URI:CHK:abc123...
```

### Download a File

```bash
redundanet storage download URI:CHK:abc123... /path/to/output.txt
```

### Mount as Filesystem

```bash
# Mount Tahoe filesystem
redundanet storage mount /mnt/redundanet

# Access files
ls /mnt/redundanet

# Unmount when done
redundanet storage unmount /mnt/redundanet
```

## Troubleshooting

### VPN Won't Connect

1. Check firewall settings (port 655 TCP/UDP)
2. Verify GPG key is uploaded to keyserver
3. Check logs: `docker compose logs tinc`

### Storage Node Not Available

1. Ensure introducer is running and reachable
2. Check Tahoe logs: `docker compose logs tahoe-storage`
3. Verify manifest is synced: `redundanet sync`

### Connection Issues

```bash
# Check peer connectivity
redundanet network peers

# Ping a specific node
redundanet network ping node-name

# View detailed status
redundanet status --verbose
```

## Next Steps

- [Configuration Reference](configuration.md) - Customize your setup
- [Architecture Overview](architecture.md) - Understand how RedundaNet works
- [Contributing](../README.md#contributing) - Help improve RedundaNet
