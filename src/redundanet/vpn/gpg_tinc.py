"""Reuse a node's GPG (OpenPGP) RSA key as its Tinc transport key.

Tinc 1.0 authenticates peers with PKCS#1 RSA keys
(``-----BEGIN RSA PUBLIC/PRIVATE KEY-----``). RedundaNet uses each node's GPG
identity key directly as its Tinc key instead of generating a separate one, so
the GPG key — which must be RSA — is converted to the PEM format Tinc reads.
Public keys are distributed via the GPG keyservers (by ``gpg_key_id``), so there
is no second key to publish.
"""

from __future__ import annotations

import pgpy
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

from redundanet.core.exceptions import VPNError


def _parse_key(armored: str) -> pgpy.PGPKey:
    """Parse an armored GPG key, or raise VPNError."""
    try:
        key, _ = pgpy.PGPKey.from_blob(armored)
    except Exception as e:  # PGPy raises a variety of parse errors
        raise VPNError(f"Could not parse GPG key: {e}") from e
    return key


def _rsa_keymaterial(key: pgpy.PGPKey) -> object:
    """Return the primary key's RSA key material, or raise for non-RSA keys."""
    keymaterial = key._key.keymaterial
    # RSA key material exposes the modulus ``n`` and public exponent ``e``.
    if not hasattr(keymaterial, "n") or not hasattr(keymaterial, "e"):
        raise VPNError(
            f"GPG key is {key.key_algorithm.name}, not RSA. Tinc requires an RSA "
            "key; generate your node key with 'redundanet node keys generate' "
            "(RSA is the default)."
        )
    return keymaterial


def gpg_key_fingerprint(armored: str) -> str:
    """Return the primary key fingerprint (40-char uppercase hex, no spaces)."""
    key = _parse_key(armored)
    return str(key.fingerprint).replace(" ", "").upper()


def gpg_public_to_tinc_pub(armored_public: str) -> str:
    """Convert an armored GPG public key to a PKCS#1 PEM public key (Tinc host file)."""
    km = _rsa_keymaterial(_parse_key(armored_public))
    public = rsa.RSAPublicNumbers(int(km.e), int(km.n)).public_key()  # type: ignore[attr-defined]
    return public.public_bytes(
        serialization.Encoding.PEM, serialization.PublicFormat.PKCS1
    ).decode()


def gpg_secret_to_tinc_priv(armored_secret: str) -> str:
    """Convert an armored GPG secret key to a PKCS#1 PEM private key (Tinc rsa_key.priv)."""
    key = _parse_key(armored_secret)
    km = _rsa_keymaterial(key)

    if key.is_public or not hasattr(km, "d"):
        raise VPNError("GPG key has no secret material (provide the private key).")
    if key.is_protected:
        raise VPNError(
            "GPG secret key is passphrase-protected. Tinc needs the raw RSA "
            "parameters, so export the key without a passphrase "
            "(gpg --export-secret-keys after removing the passphrase, or "
            "generate the node key with 'redundanet node keys generate', "
            "which uses no passphrase)."
        )

    n, e = int(km.n), int(km.e)  # type: ignore[attr-defined]
    d, p, q = int(km.d), int(km.p), int(km.q)  # type: ignore[attr-defined]
    dmp1 = d % (p - 1)
    dmq1 = d % (q - 1)
    iqmp = rsa.rsa_crt_iqmp(p, q)
    private = rsa.RSAPrivateNumbers(
        p, q, d, dmp1, dmq1, iqmp, rsa.RSAPublicNumbers(e, n)
    ).private_key()
    return private.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.TraditionalOpenSSL,
        serialization.NoEncryption(),
    ).decode()
