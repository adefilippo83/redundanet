#!/bin/bash -e
# Install additional dependencies and configure Docker

# Enable Docker service
on_chroot << EOF
systemctl enable docker
usermod -aG docker redundanet
EOF

echo "Dependencies installed successfully"
