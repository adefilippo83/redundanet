#!/bin/bash -e
# Install RedundaNet Python package

on_chroot << EOF
# Create a virtual environment for RedundaNet
python3 -m venv /opt/redundanet/venv

# Activate and install RedundaNet
source /opt/redundanet/venv/bin/activate
pip install --upgrade pip wheel setuptools

# Install RedundaNet CLI from PyPI
pip install redundanet

# Create symlink so 'redundanet' command is available system-wide
ln -sf /opt/redundanet/venv/bin/redundanet /usr/local/bin/redundanet

# Verify installation
redundanet --version || echo "Warning: redundanet CLI not yet available (package may not be on PyPI yet)"
EOF

echo "RedundaNet CLI installed"
