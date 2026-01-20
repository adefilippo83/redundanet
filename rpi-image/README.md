# RedundaNet Raspberry Pi Image

Pre-built Raspberry Pi OS images with RedundaNet pre-installed for easy deployment.

## Overview

The images are built automatically via GitHub Actions using [arm-runner-action](https://github.com/pguyot/arm-runner-action), which modifies official Raspberry Pi OS images with QEMU emulation.

## Image Features

- **Base**: Raspberry Pi OS Lite (64-bit, Bookworm)
- **Pre-installed**:
  - Docker & Docker Compose
  - Python 3.11+ with pip
  - Tinc VPN
  - GnuPG for key management
  - RedundaNet CLI
- **Ready to use**: SSH enabled, systemd services configured
- **Default credentials**: `redundanet` / `redundanet` (change immediately on first login!)

## Quick Start

### 1. Download the Image

Download the latest image from [GitHub Releases](https://github.com/adefilippo83/redundanet/releases).

### 2. Flash to SD Card

Use [Raspberry Pi Imager](https://www.raspberrypi.com/software/) or `dd`:

```bash
# Using Raspberry Pi Imager (recommended)
# Select "Use custom" and choose the downloaded .img file

# Or using dd (Linux/macOS)
xzcat redundanet-rpi-*.img.xz | sudo dd of=/dev/sdX bs=4M status=progress
sync
```

### 3. Boot and Connect

1. Insert SD card into your Raspberry Pi
2. Connect Ethernet (or configure WiFi in Imager)
3. Power on
4. Wait ~2 minutes for first boot

### 4. SSH In

```bash
ssh redundanet@redundanet.local
# Password: redundanet
```

**Important**: Change the default password immediately!

```bash
passwd
```

### 5. Configure Your Node

If you haven't already joined the network, follow the join process:

```bash
# Generate GPG key
redundanet node keys generate --name my-pi-node --email you@example.com

# Publish to keyservers
redundanet node keys publish --key-id YOUR_KEY_ID

# Note your Key ID, then visit:
# https://redundanet.com/join.html
```

If you've already been approved:

```bash
# Initialize with your assigned node name
redundanet init --name node-XXXXXXXX

# Sync the manifest
redundanet sync

# Start services
sudo systemctl enable --now redundanet-docker
# Or manually:
cd /opt/redundanet && docker compose up -d
```

### 6. Verify

```bash
redundanet status
docker compose ps
```

## Supported Hardware

| Model | Status | Notes |
|-------|--------|-------|
| Raspberry Pi 5 | Supported | Recommended |
| Raspberry Pi 4 | Supported | 2GB+ RAM recommended |
| Raspberry Pi 3B/3B+ | Supported | Minimum viable |
| Raspberry Pi Zero 2 W | Supported | Limited performance |

64-bit (arm64) images are provided.

## Directory Structure

After setup, RedundaNet files are located at:

| Path | Purpose |
|------|---------|
| `/opt/redundanet/` | RedundaNet installation |
| `/etc/redundanet/` | Configuration files |
| `/var/lib/redundanet/` | Data storage |
| `/var/log/redundanet/` | Log files |

## Services

The image includes systemd services:

```bash
# Enable and start RedundaNet
sudo systemctl enable --now redundanet-docker

# Check status
sudo systemctl status redundanet-docker

# View logs
sudo journalctl -u redundanet-docker -f
```

## Building the Image Locally

To build the image yourself (requires Linux with Docker):

```bash
# Clone the repository
git clone https://github.com/adefilippo83/redundanet.git
cd redundanet

# Run the build script (uses QEMU via Docker)
./rpi-image/build.sh
```

Or trigger a build via GitHub Actions:
1. Go to Actions > Build RPi Image
2. Click "Run workflow"

## Troubleshooting

### Can't find `redundanet.local`

1. Ensure your Pi is connected to the network
2. Try using the IP address directly (check your router)
3. On macOS/Linux: `ping redundanet.local`

### SSH connection refused

Wait 2-3 minutes after boot for services to start, then try again.

### Docker not starting

```bash
sudo systemctl status docker
sudo journalctl -u docker -n 50
```

### `redundanet` command not found

The `redundanet` CLI is installed in a virtual environment at `/opt/redundanet/venv`.

```bash
# Option 1: Run the install/update script
sudo /opt/redundanet/install-redundanet.sh

# Option 2: Use the venv directly
/opt/redundanet/venv/bin/redundanet --version

# Option 3: Run as Python module
/opt/redundanet/venv/bin/python -m redundanet --version

# Option 4: Recreate the symlink
sudo ln -sf /opt/redundanet/venv/bin/redundanet /usr/local/bin/redundanet
```

**Note:** Do NOT run `pip install redundanet` directly - use the virtual environment.

### Check first boot log

```bash
cat /var/log/redundanet/first-boot.log
```

### Reset configuration

```bash
sudo rm /etc/redundanet/.initialized
sudo systemctl restart redundanet-init
```

### Storage issues

Ensure your SD card has sufficient space:

```bash
df -h
```

The image requires at least 8GB SD card, 16GB+ recommended.

## Network Configuration

### WiFi Setup

If using WiFi, configure during flashing with Raspberry Pi Imager, or after boot:

```bash
sudo raspi-config
# Navigate to: System Options > Wireless LAN
```

### Static IP

Edit `/etc/dhcpcd.conf`:

```bash
interface eth0
static ip_address=192.168.1.100/24
static routers=192.168.1.1
static domain_name_servers=8.8.8.8
```

Then restart: `sudo systemctl restart dhcpcd`

## Security Recommendations

1. **Change default password** immediately after first login
2. **Set up SSH keys** and disable password authentication
3. **Keep the system updated**: `sudo apt update && sudo apt upgrade`
4. **Configure firewall** if directly exposed to internet

```bash
# Set up SSH key authentication
mkdir -p ~/.ssh
echo "your-public-key" >> ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys

# Disable password authentication (after confirming key works)
sudo sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
sudo systemctl restart sshd
```
