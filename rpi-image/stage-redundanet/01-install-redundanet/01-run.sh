#!/bin/bash -e
# Install Poetry and RedundaNet Python package

on_chroot << EOF
# Install pipx for isolated tool installation
python3 -m pip install --user pipx
python3 -m pipx ensurepath

# Install Poetry via pipx
python3 -m pipx install poetry

# Create a virtual environment for RedundaNet
python3 -m venv /opt/redundanet/venv
source /opt/redundanet/venv/bin/activate

# Install RedundaNet (will be available from PyPI or GitHub)
# For now, we'll set up the structure and install from local copy
pip install --upgrade pip wheel setuptools
EOF

echo "Python environment configured"
