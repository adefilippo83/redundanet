# Installation Guide

This guide covers the various ways to install and deploy RedundaNet.

## Prerequisites

Before installing RedundaNet, ensure you have:

- **Python 3.11+** (for CLI and development)
- **Docker** and **Docker Compose** (for containerized deployment)
- **GPG** (for node authentication)
- **Git** (for manifest synchronization)

## Installation Methods

### 1. PyPI Installation (Recommended)

The simplest way to install the RedundaNet CLI:

```bash
pip install redundanet
```

Verify the installation:

```bash
redundanet --version
```

### 2. Poetry Installation (For Development)

Clone the repository and install with Poetry:

```bash
git clone https://github.com/adefilippo83/redundanet.git
cd redundanet

# Install Poetry if not already installed
curl -sSL https://install.python-poetry.org | python3 -

# Install dependencies
poetry install

# Activate virtual environment
poetry shell

# Verify installation
redundanet --version
```

### 3. Docker Deployment (For Running Services)

Clone the repository:

```bash
git clone https://github.com/adefilippo83/redundanet.git
cd redundanet
```

Start services:

```bash
# As a storage node
docker compose --profile storage up -d

# As a client
docker compose --profile client up -d
```

### 4. Raspberry Pi Image

Download the pre-built image from [GitHub Releases](https://github.com/adefilippo83/redundanet/releases):

1. Flash to SD card using [Raspberry Pi Imager](https://www.raspberrypi.com/software/)
2. Boot your Raspberry Pi
3. SSH in: `ssh redundanet@redundanet.local` (password: `redundanet`)
4. Change password immediately: `passwd`
5. Configure: `redundanet init`

## System Requirements

### Minimum Requirements

| Resource | Requirement |
|----------|-------------|
| CPU | 1 core |
| RAM | 1 GB |
| Storage | 10 GB (+ contributed storage) |
| Network | 1 Mbps |

### Recommended Requirements

| Resource | Requirement |
|----------|-------------|
| CPU | 2+ cores |
| RAM | 2+ GB |
| Storage | 50+ GB SSD |
| Network | 10+ Mbps |

## Platform Support

### Linux (Primary)

Fully supported on:
- Ubuntu 20.04+
- Debian 11+
- Raspberry Pi OS
- CentOS 8+
- Fedora 35+
- Arch Linux

### macOS

Supported for development and CLI usage. Docker Desktop required for containerized deployment.

### Windows

Supported via WSL2 (Windows Subsystem for Linux). Native Windows support is experimental.

## Network Requirements

### Ports

| Port | Protocol | Purpose |
|------|----------|---------|
| 655 | TCP/UDP | Tinc VPN |
| 3456 | TCP | Tahoe-LAFS Client |
| 3457 | TCP | Tahoe-LAFS Storage |
| 3458 | TCP | Tahoe-LAFS Introducer |

### Firewall Configuration

For publicly accessible nodes, open port 655:

```bash
# UFW (Ubuntu)
sudo ufw allow 655/tcp
sudo ufw allow 655/udp

# firewalld (CentOS/Fedora)
sudo firewall-cmd --permanent --add-port=655/tcp
sudo firewall-cmd --permanent --add-port=655/udp
sudo firewall-cmd --reload

# iptables
sudo iptables -A INPUT -p tcp --dport 655 -j ACCEPT
sudo iptables -A INPUT -p udp --dport 655 -j ACCEPT
```

## GPG Key Setup

GPG keys are required for node authentication. Keys must be published to a public keyserver.

### Using RedundaNet CLI (Recommended)

```bash
# Generate a new GPG key
redundanet node keys generate --name my-node --email you@example.com

# List your keys to get the Key ID
redundanet node keys list

# Publish to keyservers
redundanet node keys publish --key-id YOUR_KEY_ID
```

### Using Standard GPG Commands

```bash
# Generate new key
gpg --full-generate-key
# Choose: RSA and RSA, 4096 bits, no expiration

# List keys to get your key ID
gpg --list-keys --keyid-format long

# Export to keyserver
gpg --keyserver keys.openpgp.org --send-keys YOUR_KEY_ID
```

### Verifying Key Publication

```bash
# Search for your key on the keyserver
redundanet node keys fetch --key-id YOUR_KEY_ID

# Or using gpg
gpg --keyserver keys.openpgp.org --search-keys your@email.com
```

## Verification

After installation, verify everything is working:

```bash
# Check CLI
redundanet --version

# Check GPG keys
redundanet node keys list

# Check Docker (if using containers)
docker compose config

# Check status
redundanet status
```

## Next Steps

- [Quick Start Guide](quickstart.md) - Get up and running quickly
- [Configuration Reference](configuration.md) - Detailed configuration options
- [Architecture Overview](architecture.md) - Understand the system design
