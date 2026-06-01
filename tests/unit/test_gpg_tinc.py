"""Tests for converting a GPG RSA key into Tinc's PEM key format."""

import os
import shutil
import subprocess
import tempfile
from collections.abc import Iterator
from pathlib import Path

import pytest
from cryptography.hazmat.primitives import serialization as ser
from cryptography.hazmat.primitives.serialization import load_pem_private_key

from redundanet.vpn.gpg_tinc import gpg_public_to_tinc_pub, gpg_secret_to_tinc_priv

pytestmark = pytest.mark.skipif(shutil.which("gpg") is None, reason="gpg not available")


@pytest.fixture
def gnupg_home() -> Iterator[Path]:
    # Use a short path: gpg-agent's unix socket has a ~104-char limit, which the
    # default (long) pytest tmp dir can exceed on macOS.
    path = Path(tempfile.mkdtemp(dir="/tmp", prefix="rn-gpgt-"))
    try:
        yield path
    finally:
        subprocess.run(
            ["gpgconf", "--kill", "gpg-agent"],
            env={**os.environ, "GNUPGHOME": str(path)},
            capture_output=True,
        )
        shutil.rmtree(path, ignore_errors=True)


def _gen_gpg_key(gnupg_home: Path) -> tuple[str, str]:
    """Generate a throwaway RSA GPG key with the gpg CLI; return (pub, secret) armored."""
    params = gnupg_home / "params"
    params.write_text(
        "%no-protection\n"
        "Key-Type: RSA\n"
        "Key-Length: 2048\n"
        "Name-Real: Conv Test\n"
        "Name-Email: conv@test.local\n"
        "Expire-Date: 0\n"
        "%commit\n"
    )
    env = {**os.environ, "GNUPGHOME": str(gnupg_home)}
    subprocess.run(
        ["gpg", "--batch", "--gen-key", str(params)], env=env, check=True, capture_output=True
    )
    pub = subprocess.run(
        ["gpg", "--armor", "--export", "conv@test.local"],
        env=env, check=True, capture_output=True, text=True,
    ).stdout
    sec = subprocess.run(
        ["gpg", "--batch", "--pinentry-mode", "loopback", "--armor",
         "--export-secret-keys", "conv@test.local"],
        env=env, check=True, capture_output=True, text=True,
    ).stdout
    return pub, sec


def test_gpg_rsa_converts_to_consistent_tinc_pem(gnupg_home: Path):
    pub, sec = _gen_gpg_key(gnupg_home)

    tinc_pub = gpg_public_to_tinc_pub(pub)
    tinc_priv = gpg_secret_to_tinc_priv(sec)

    # Tinc 1.0 expects PKCS#1 PEM blocks.
    assert tinc_pub.startswith("-----BEGIN RSA PUBLIC KEY-----")
    assert tinc_priv.startswith("-----BEGIN RSA PRIVATE KEY-----")

    # The public key derived from the converted private key must match the
    # converted public key (i.e. they form a consistent keypair).
    priv = load_pem_private_key(tinc_priv.encode(), password=None)
    derived_pub = (
        priv.public_key().public_bytes(ser.Encoding.PEM, ser.PublicFormat.PKCS1).decode()
    )
    assert derived_pub.strip() == tinc_pub.strip()
