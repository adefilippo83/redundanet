#!/bin/bash -e
# Install and configure systemd services

# Copy service files
install -m 644 files/redundanet.service "${ROOTFS_DIR}/etc/systemd/system/redundanet.service"
install -m 644 files/redundanet-docker.service "${ROOTFS_DIR}/etc/systemd/system/redundanet-docker.service"
install -m 644 files/redundanet-init.service "${ROOTFS_DIR}/etc/systemd/system/redundanet-init.service"

# Enable services
on_chroot << EOF
systemctl enable redundanet-init.service
# Don't enable redundanet-docker yet - let first-boot configure it
EOF

echo "Systemd services configured"
