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

# Publish to keyservers — use your key's FULL 40-character fingerprint
# (find it with `gpg --fingerprint`; short key ids are rejected)
redundanet node keys publish --key-id YOUR_FULL_FINGERPRINT

# Note your full fingerprint, then apply at:
# https://redundanet.com/join.html
```

If you've already been approved:

```bash
# Initialize with your assigned node name
redundanet init --name node-XXXXXXXX

# Join the network: clones the manifest repo, installs the docker files and
# generates /opt/redundanet/.env from your manifest entry
redundanet network join --repo https://github.com/adefilippo83/redundanet.git --name node-XXXXXXXX

# Start services (the `join` command prints this too). The -p flag matters:
# it is the project name the `redundanet` CLI uses to find the containers.
cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
  --profile storage up -d

# To run a storage node AND a client on the same Pi, stack the profiles:
#   ... --profile storage --profile client up -d
```

The containers use `restart: unless-stopped`, so Docker brings them back
automatically after a reboot — no extra systemd unit is needed.

### 6. Verify

```bash
redundanet status
docker compose -p redundanet ps
```

### 7. Put Storage on an External Disk (strongly recommended)

**Without this step, all stored shares land on the SD card** (inside the
`storage-data` Docker volume) — SD cards are small and wear out quickly, and a
disk that is hand-mounted but missing from `/etc/fstab` reverts to the empty
volume after a reboot, making the node silently appear to have lost its data.

```bash
# 1. Identify the disk (double-check — formatting is destructive!)
lsblk

# 2. Format once and mount it
sudo mkfs.ext4 /dev/sda1
sudo mkdir -p /mnt/storage
sudo mount /dev/sda1 /mnt/storage
sudo mkdir -p /mnt/storage/redundanet

# 3. Persist the mount across reboots — always by UUID
UUID=$(sudo blkid -s UUID -o value /dev/sda1)
echo "UUID=$UUID /mnt/storage ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab

# 4. Point the storage container at the disk with a compose override
cat <<'EOF' | sudo tee /opt/redundanet/docker/docker-compose.override.yml
services:
  tahoe-storage:
    volumes:
      - /mnt/storage/redundanet:/data/storage
EOF

# 5. Recreate so the bind-mount takes effect
cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
  --profile storage up -d --force-recreate tinc tahoe-storage

# 6. Confirm the container reads from the disk
docker inspect redundanet-tahoe-storage \
  --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}' | grep /data/storage
# expected: /mnt/storage/redundanet -> /data/storage
```

The `redundanet` CLI (`update`, `storage`, ...) automatically includes
`docker-compose.override.yml` in every compose command, so the disk stays
attached across updates and recreates.

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

The image ships one systemd unit, `redundanet-firstboot.service`, which runs
once on first boot (generates a node-name, writes the first-boot log) and then
disables itself via `/etc/redundanet/.initialized`.

The RedundaNet containers themselves are **not** managed by systemd: they are
started with `docker compose` (step 5) and carry `restart: unless-stopped`, so
the Docker daemon restarts them on failure and after every reboot.

```bash
# Container status and logs
docker compose -p redundanet ps
docker compose -p redundanet logs -f tinc

# First-boot service (one-shot)
systemctl status redundanet-firstboot
```

## Building the Image

Images are built by the **Build RPi Image** GitHub Actions workflow
(`.github/workflows/build-rpi-image.yml`), which uses
[arm-runner-action](https://github.com/pguyot/arm-runner-action) to customize
an official Raspberry Pi OS image under QEMU. The customization steps are
inlined in the workflow itself — the pi-gen-style stage files under
`rpi-image/stage-redundanet/` are **not** currently used by the build. There
is no local build script.

To build one yourself:
1. Fork the repository (or use your push access)
2. Go to **Actions → Build RPi Image**
3. Click **"Run workflow"** and download the resulting image artifact

## Troubleshooting

### Can't find `redundanet.local`

1. Ensure your Pi is connected to the network
2. Try using the IP address directly (check your router)
3. On macOS/Linux: `ping redundanet.local`

**Running more than one Pi?** Every image boots with the same hostname
(`redundanet`), so two Pis on one LAN collide on `redundanet.local` — mDNS
will resolve to whichever answered first. Give each Pi a unique hostname
right after first login:

```bash
sudo hostnamectl set-hostname my-pi-node && sudo reboot
```

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
# Option 1: Use the venv directly
/opt/redundanet/venv/bin/redundanet --version

# Option 2: Run as Python module
/opt/redundanet/venv/bin/python -m redundanet --version

# Option 3: Recreate the symlink
sudo ln -sf /opt/redundanet/venv/bin/redundanet /usr/local/bin/redundanet
```

**Note:** Do NOT run `pip install redundanet` outside the venv - use the virtual environment.

### Upgrading the CLI

```bash
# --no-cache-dir matters: a stale pip HTTP cache can silently serve an old
# version and make the upgrade appear to do nothing.
sudo /opt/redundanet/venv/bin/pip install --upgrade --no-cache-dir redundanet
redundanet --version
```

### Check first boot log

```bash
cat /var/log/redundanet/first-boot.log
```

### Reset first-boot configuration

```bash
# The one-shot unit is named redundanet-firstboot; it re-runs only when the
# .initialized flag is removed.
sudo rm /etc/redundanet/.initialized
sudo systemctl restart redundanet-firstboot
```

### Storage issues

First check whether the storage container is actually reading from your
external disk (see **step 7** above) — after a reboot with a missing fstab
entry it silently falls back to the empty Docker volume on the SD card:

```bash
docker inspect redundanet-tahoe-storage \
  --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}' | grep /data/storage
df -h /mnt/storage
```

The image itself requires at least an 8GB SD card, 16GB+ recommended — but
contributed storage should live on an external disk, not the card.

## Network Configuration

### WiFi Setup

If using WiFi, configure during flashing with Raspberry Pi Imager, or after boot:

```bash
sudo raspi-config
# Navigate to: System Options > Wireless LAN
```

### Static IP

Raspberry Pi OS Bookworm uses **NetworkManager** (the old `/etc/dhcpcd.conf`
method no longer applies):

```bash
sudo nmcli connection modify "Wired connection 1" \
  ipv4.addresses 192.168.1.100/24 \
  ipv4.gateway 192.168.1.1 \
  ipv4.dns 8.8.8.8 \
  ipv4.method manual
sudo nmcli connection up "Wired connection 1"
```

(Or use the interactive `sudo nmtui`.)

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
