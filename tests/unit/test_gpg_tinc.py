"""Tests for converting a GPG RSA key into Tinc's PEM key format."""

import os
import re
import shutil
import subprocess
import tempfile
from collections.abc import Iterator
from pathlib import Path

import pgpy
import pytest
from cryptography.hazmat.primitives import serialization as ser
from cryptography.hazmat.primitives.serialization import load_pem_private_key
from pgpy.constants import (
    CompressionAlgorithm,
    EllipticCurveOID,
    HashAlgorithm,
    KeyFlags,
    PubKeyAlgorithm,
    SymmetricKeyAlgorithm,
)

from redundanet.core.exceptions import VPNError
from redundanet.vpn.gpg_tinc import (
    gpg_key_fingerprint,
    gpg_public_to_tinc_pub,
    gpg_secret_to_tinc_priv,
)


def _new_key(algorithm: PubKeyAlgorithm, size) -> pgpy.PGPKey:
    """Generate a throwaway pgpy key with a minimal UID."""
    key = pgpy.PGPKey.new(algorithm, size)
    uid = pgpy.PGPUID.new("Conv Test", email="conv@test.local")
    key.add_uid(
        uid,
        usage={KeyFlags.Sign, KeyFlags.EncryptCommunications},
        hashes=[HashAlgorithm.SHA256],
        ciphers=[SymmetricKeyAlgorithm.AES256],
        compression=[CompressionAlgorithm.ZLIB],
    )
    return key


@pytest.fixture(scope="module")
def rsa_key() -> pgpy.PGPKey:
    return _new_key(PubKeyAlgorithm.RSAEncryptOrSign, 2048)


class TestConversionFailureModes:
    """The conversion must fail loudly and helpfully on unusable keys."""

    def test_garbage_input_raises(self):
        with pytest.raises(VPNError, match="Could not parse"):
            gpg_secret_to_tinc_priv("not a key at all")

    def test_public_only_key_has_no_secret_material(self, rsa_key: pgpy.PGPKey):
        armored_public = str(rsa_key.pubkey)
        with pytest.raises(VPNError, match="no secret material"):
            gpg_secret_to_tinc_priv(armored_public)

    def test_passphrase_protected_key_is_rejected_with_guidance(self, rsa_key: pgpy.PGPKey):
        # Copy the key via a serialization round-trip, then protect the copy.
        protected, _ = pgpy.PGPKey.from_blob(str(rsa_key))
        protected.protect("hunter2", SymmetricKeyAlgorithm.AES256, HashAlgorithm.SHA256)
        with pytest.raises(VPNError, match="passphrase-protected"):
            gpg_secret_to_tinc_priv(str(protected))

    def test_non_rsa_key_is_rejected(self):
        eddsa = _new_key(PubKeyAlgorithm.EdDSA, EllipticCurveOID.Ed25519)
        with pytest.raises(VPNError, match="not RSA"):
            gpg_public_to_tinc_pub(str(eddsa.pubkey))
        with pytest.raises(VPNError, match="not RSA"):
            gpg_secret_to_tinc_priv(str(eddsa))


class TestConversionPgpy:
    def test_pgpy_rsa_converts_to_consistent_pem_pair(self, rsa_key: pgpy.PGPKey):
        tinc_pub = gpg_public_to_tinc_pub(str(rsa_key.pubkey))
        tinc_priv = gpg_secret_to_tinc_priv(str(rsa_key))

        assert tinc_pub.startswith("-----BEGIN RSA PUBLIC KEY-----")
        assert tinc_priv.startswith("-----BEGIN RSA PRIVATE KEY-----")

        priv = load_pem_private_key(tinc_priv.encode(), password=None)
        derived_pub = (
            priv.public_key().public_bytes(ser.Encoding.PEM, ser.PublicFormat.PKCS1).decode()
        )
        assert derived_pub.strip() == tinc_pub.strip()

    def test_fingerprint_extraction(self, rsa_key: pgpy.PGPKey):
        fingerprint = gpg_key_fingerprint(str(rsa_key.pubkey))
        assert re.fullmatch(r"[0-9A-F]{40}", fingerprint)
        assert fingerprint == str(rsa_key.fingerprint).replace(" ", "").upper()


needs_gpg = pytest.mark.skipif(shutil.which("gpg") is None, reason="gpg not available")


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
        env=env,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    sec = subprocess.run(
        [
            "gpg",
            "--batch",
            "--pinentry-mode",
            "loopback",
            "--armor",
            "--export-secret-keys",
            "conv@test.local",
        ],
        env=env,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return pub, sec


@needs_gpg
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
    derived_pub = priv.public_key().public_bytes(ser.Encoding.PEM, ser.PublicFormat.PKCS1).decode()
    assert derived_pub.strip() == tinc_pub.strip()
