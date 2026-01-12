#!/bin/bash
# RedundaNet installation script
# This script can be run post-install to update RedundaNet

set -e

REDUNDANET_VERSION="${REDUNDANET_VERSION:-latest}"
INSTALL_DIR="/opt/redundanet"
VENV_DIR="${INSTALL_DIR}/venv"

echo "Installing RedundaNet ${REDUNDANET_VERSION}..."

# Activate virtual environment
source "${VENV_DIR}/bin/activate"

# Install/update RedundaNet
if [ "$REDUNDANET_VERSION" = "latest" ]; then
    pip install --upgrade redundanet
else
    pip install "redundanet==${REDUNDANET_VERSION}"
fi

# Pull latest Docker images
cd "${INSTALL_DIR}"
if [ -f "docker/docker-compose.yml" ]; then
    docker compose -f docker/docker-compose.yml pull
fi

echo "RedundaNet installation complete!"
