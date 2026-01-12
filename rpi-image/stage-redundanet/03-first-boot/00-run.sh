#!/bin/bash -e
# Install first-boot script and MOTD

# Copy first-boot script
install -m 755 files/first-boot.sh "${ROOTFS_DIR}/opt/redundanet/first-boot.sh"

# Copy MOTD
install -m 644 files/motd "${ROOTFS_DIR}/etc/motd"

# Create symlink for redundanet command
on_chroot << EOF
ln -sf /opt/redundanet/venv/bin/redundanet /usr/local/bin/redundanet
EOF

echo "First boot setup configured"
