# Installation Guide

This guide covers the various ways to install and deploy RedundaNet.

## Prerequisites

Before installing RedundaNet, ensure you have:

- **Python 3.11+** (for CLI and development)
- **Docker** and **Docker Compose** (for containerized deployment)
- **GPG** (for node authentication)
- **Git** (for manifest synchronization)

## Installation Methods

### 1. PyPI Installation (Recommended for CLI)

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
git clone https://github.com/adefilippo83/project-earthgrid.git
cd project-earthgrid

# Install Poetry if not already installed
curl -sSL https://install.python-poetry.org | python3 -

# Install dependencies
poetry install

# Activate virtual environment
poetry shell

# Verify installation
redundanet --version
```

### 3. Docker Deployment (Recommended for Production)

Pull pre-built containers:

```bash
docker pull ghcr.io/redundanet/tinc:latest
docker pull ghcr.io/redundanet/tahoe-client:latest
docker pull ghcr.io/redundanet/tahoe-storage:latest
docker pull ghcr.io/redundanet/tahoe-introducer:latest
```

Or build locally:

```bash
git clone https://github.com/adefilippo83/project-earthgrid.git
cd project-earthgrid

docker compose -f docker/docker-compose.yml build
```

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

For publicly accessible nodes:

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

Generate a GPG key for node authentication:

```bash
# Generate new key
gpg --full-generate-key

# List keys to get your key ID
gpg --list-keys

# Export public key to keyserver
gpg --keyserver keyserver.ubuntu.com --send-keys YOUR_KEY_ID
```

## Verification

After installation, verify everything is working:

```bash
# Check CLI
redundanet --version

# Check Docker (if using containers)
docker compose -f docker/docker-compose.yml config

# Check GPG
gpg --list-keys
```

## Next Steps

- [Quick Start Guide](quickstart.md) - Get up and running quickly
- [Configuration Reference](configuration.md) - Detailed configuration options
- [Architecture Overview](architecture.md) - Understand the system design
