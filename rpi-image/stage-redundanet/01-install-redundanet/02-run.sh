#!/bin/bash -e
# Copy RedundaNet files to the image

# Get the project root directory (two levels up from stage-redundanet)
PROJECT_ROOT="$(cd "$(dirname "$0")/../../../.." && pwd)"

# Copy installation script
install -m 755 files/install-redundanet.sh "${ROOTFS_DIR}/opt/redundanet/install-redundanet.sh"

# Create docker directory
mkdir -p "${ROOTFS_DIR}/opt/redundanet/docker"

# Copy docker-compose files from project root
if [ -d "${PROJECT_ROOT}/docker" ]; then
    cp -r "${PROJECT_ROOT}/docker/"* "${ROOTFS_DIR}/opt/redundanet/docker/"
    echo "Docker configuration files copied"
else
    echo "Warning: docker/ directory not found at ${PROJECT_ROOT}/docker"
fi

# Copy first-boot script
install -m 755 ../03-first-boot/files/first-boot.sh "${ROOTFS_DIR}/opt/redundanet/first-boot.sh"

echo "RedundaNet files copied"
