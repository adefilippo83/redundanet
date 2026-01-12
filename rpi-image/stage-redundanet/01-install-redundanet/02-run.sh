#!/bin/bash -e
# Copy RedundaNet files to the image

# Copy installation script
install -m 755 files/install-redundanet.sh "${ROOTFS_DIR}/opt/redundanet/install-redundanet.sh"

echo "RedundaNet files copied"
