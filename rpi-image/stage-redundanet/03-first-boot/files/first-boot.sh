#!/bin/bash
# RedundaNet First Boot Setup Script
# This script runs once on first boot to configure the node

set -e

CONFIG_DIR="${REDUNDANET_CONFIG_DIR:-/etc/redundanet}"
DATA_DIR="${REDUNDANET_DATA_DIR:-/var/lib/redundanet}"
LOG_FILE="/var/log/redundanet/first-boot.log"

# Logging function
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG_FILE"
}

log "Starting RedundaNet first boot initialization..."

# Ensure directories exist
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$(dirname $LOG_FILE)"

# Generate unique node name if not set
if [ ! -f "$CONFIG_DIR/node-name" ]; then
    # Generate name based on hostname and random suffix
    NODE_NAME="rpi-$(hostname -s)-$(head /dev/urandom | tr -dc 'a-z0-9' | head -c 4)"
    echo "$NODE_NAME" > "$CONFIG_DIR/node-name"
    log "Generated node name: $NODE_NAME"
else
    NODE_NAME=$(cat "$CONFIG_DIR/node-name")
    log "Using existing node name: $NODE_NAME"
fi

# Check for configuration file
if [ -f "$CONFIG_DIR/config.yaml" ]; then
    log "Found existing configuration"
else
    log "No configuration found - creating default config"
    cat > "$CONFIG_DIR/config.yaml" << EOF
# RedundaNet Node Configuration
# Generated on first boot: $(date)

node:
  name: ${NODE_NAME}

network:
  # Set your network's manifest repository URL
  manifest_repo: ""
  manifest_branch: "main"

storage:
  # Amount of storage to contribute to the network
  contribution: "100GB"
  # Path for storage data
  data_path: "${DATA_DIR}/storage"

# Set to true after running 'redundanet init'
initialized: false
EOF
fi

# Pull Docker images (if network is available)
log "Checking network connectivity..."
if ping -c 1 -W 5 8.8.8.8 > /dev/null 2>&1; then
    log "Network available - pulling Docker images..."

    if [ -f "/opt/redundanet/docker/docker-compose.yml" ]; then
        cd /opt/redundanet/docker
        docker compose pull || log "Warning: Failed to pull Docker images"
    fi
else
    log "Network not available - skipping Docker image pull"
fi

# Create marker file for web setup wizard
touch "$CONFIG_DIR/.needs-setup"

log "First boot initialization complete!"
log ""
log "============================================"
log "  RedundaNet is ready for configuration!"
log "============================================"
log ""
log "Next steps:"
log "  1. Connect to your network"
log "  2. Run: redundanet init"
log "  3. Or visit: http://$(hostname -I | awk '{print $1}'):8080"
log ""

exit 0
