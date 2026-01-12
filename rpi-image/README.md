# RedundaNet Raspberry Pi Image

This directory contains the configuration for building custom Raspberry Pi OS images with RedundaNet pre-installed.

## Overview

The image is built using [pi-gen](https://github.com/RPi-Distro/pi-gen), the official tool for creating Raspberry Pi OS images. The build is automated via GitHub Actions.

## Image Features

- **Base**: Raspberry Pi OS Lite (Bookworm)
- **Pre-installed**:
  - Docker & Docker Compose
  - Python 3.11+ with virtual environment
  - Tinc VPN
  - GnuPG for key management
  - RedundaNet CLI and services
- **Ready to use**: SSH enabled, systemd services configured
- **Default credentials**: `redundanet` / `redundanet` (change on first login!)

## Building the Image

### Automatic (GitHub Actions)

Images are automatically built when:
- A new release is published
- Manually triggered via workflow dispatch

The built images are uploaded to GitHub Releases.

### Manual Build

To build locally (requires Linux with Docker):

```bash
# Clone pi-gen
git clone https://github.com/RPi-Distro/pi-gen.git
cd pi-gen

# Copy our custom stage
cp -r ../rpi-image/stage-redundanet ./

# Configure the build
cat > config << EOF
IMG_NAME=redundanet-rpi
RELEASE=bookworm
DEPLOY_COMPRESSION=xz
LOCALE_DEFAULT=en_US.UTF-8
TARGET_HOSTNAME=redundanet
KEYBOARD_KEYMAP=us
KEYBOARD_LAYOUT="English (US)"
TIMEZONE_DEFAULT=UTC
FIRST_USER_NAME=redundanet
FIRST_USER_PASS=redundanet
ENABLE_SSH=1
STAGE_LIST="stage0 stage1 stage2 stage-redundanet"
EOF

# Build
./build-docker.sh
```

## Directory Structure

```
rpi-image/
├── README.md                      # This file
└── stage-redundanet/              # Custom pi-gen stage
    ├── prerun.sh                  # Stage pre-run script
    ├── 00-install-deps/           # Install system dependencies
    │   ├── 00-packages            # APT packages to install
    │   └── 01-run.sh              # Post-install configuration
    ├── 01-install-redundanet/     # Install RedundaNet
    │   ├── 00-run.sh              # Create directories
    │   ├── 01-run.sh              # Setup Python environment
    │   ├── 02-run.sh              # Copy files
    │   └── files/
    │       └── install-redundanet.sh
    ├── 02-configure-services/     # Configure systemd services
    │   ├── 00-run.sh              # Install services
    │   └── files/
    │       ├── redundanet.service
    │       ├── redundanet-docker.service
    │       └── redundanet-init.service
    └── 03-first-boot/             # First boot setup
        ├── 00-run.sh              # Install first-boot script
        └── files/
            ├── first-boot.sh      # Runs on first boot
            └── motd               # Welcome message
```

## Using the Image

1. **Download** the latest image from [GitHub Releases](../../releases)
2. **Flash** to SD card using [Raspberry Pi Imager](https://www.raspberrypi.com/software/) or `dd`
3. **Boot** your Raspberry Pi
4. **Connect** via SSH: `ssh redundanet@redundanet.local`
5. **Change password**: `passwd`
6. **Initialize**: `redundanet init`

### First Boot

On first boot, the system will:
1. Generate a unique node name
2. Create default configuration
3. Pull Docker images (if network available)
4. Prepare for setup wizard

### Configuration

After first boot, configure your node:

```bash
# Interactive setup
redundanet init --name my-node --network my-network

# Or edit configuration directly
sudo nano /etc/redundanet/config.yaml

# Start services
sudo systemctl enable --now redundanet-docker
```

## Supported Hardware

- Raspberry Pi 4 (recommended)
- Raspberry Pi 3B/3B+
- Raspberry Pi 5
- Raspberry Pi Zero 2 W

Both 32-bit (armhf) and 64-bit (arm64) images are available.

## Troubleshooting

### Check service status
```bash
systemctl status redundanet-docker
journalctl -u redundanet-docker -f
```

### View first boot log
```bash
cat /var/log/redundanet/first-boot.log
```

### Rebuild configuration
```bash
sudo rm /etc/redundanet/.initialized
sudo systemctl restart redundanet-init
```
