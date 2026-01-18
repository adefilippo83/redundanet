#!/bin/bash
# RedundaNet installation/update script
# This script can be run post-install to update RedundaNet

set -e

REDUNDANET_VERSION="${REDUNDANET_VERSION:-latest}"
INSTALL_DIR="/opt/redundanet"
VENV_DIR="${INSTALL_DIR}/venv"

echo "Installing RedundaNet ${REDUNDANET_VERSION}..."

# Create virtual environment if it doesn't exist
if [ ! -d "${VENV_DIR}" ]; then
    echo "Creating virtual environment..."
    python3 -m venv "${VENV_DIR}"
fi

# Activate virtual environment
source "${VENV_DIR}/bin/activate"

# Upgrade pip
pip install --upgrade pip wheel setuptools

# Install/update RedundaNet
if [ "$REDUNDANET_VERSION" = "latest" ]; then
    pip install --upgrade redundanet
else
    pip install "redundanet==${REDUNDANET_VERSION}"
fi

# Ensure symlink exists for system-wide access
if [ ! -L /usr/local/bin/redundanet ]; then
    echo "Creating symlink for redundanet command..."
    sudo ln -sf "${VENV_DIR}/bin/redundanet" /usr/local/bin/redundanet
fi

# Pull latest Docker images if docker-compose.yml exists
cd "${INSTALL_DIR}"
if [ -f "docker/docker-compose.yml" ]; then
    echo "Pulling latest Docker images..."
    docker compose -f docker/docker-compose.yml pull || true
fi

echo "RedundaNet installation complete!"
echo "Run 'redundanet --version' to verify."
