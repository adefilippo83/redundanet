#!/bin/bash -e
# Install RedundaNet application

# Create RedundaNet directories
on_chroot << EOF
mkdir -p /opt/redundanet
mkdir -p /etc/redundanet
mkdir -p /var/lib/redundanet
mkdir -p /var/log/redundanet

# Set ownership
chown -R redundanet:redundanet /opt/redundanet
chown -R redundanet:redundanet /etc/redundanet
chown -R redundanet:redundanet /var/lib/redundanet
chown -R redundanet:redundanet /var/log/redundanet
EOF

echo "RedundaNet directories created"
