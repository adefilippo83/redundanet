"""GPG key management for RedundaNet node authentication."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import gnupg

from redundanet.core.exceptions import GPGError
from redundanet.utils.files import ensure_dir
from redundanet.utils.logging import get_logger

logger = get_logger(__name__)


@dataclass
class GPGKeyInfo:
    """Information about a GPG key."""

    key_id: str
    fingerprint: str
    user_id: str
    email: str | None = None
    created: str | None = None
    expires: str | None = None
    trust: str | None = None

    @classmethod
    def from_gpg_key(cls, key: dict[str, Any]) -> GPGKeyInfo:
        """Create GPGKeyInfo from gnupg key dict."""
        uids = key.get("uids", [])
        user_id = uids[0] if uids else ""

        # Parse email from user_id
        email = None
        if "<" in user_id and ">" in user_id:
            email = user_id.split("<")[1].split(">")[0]

        return cls(
            key_id=key.get("keyid", ""),
            fingerprint=key.get("fingerprint", ""),
            user_id=user_id,
            email=email,
            created=key.get("date"),
            expires=key.get("expires"),
            trust=key.get("trust"),
        )


class GPGManager:
    """Manages GPG keys for node authentication."""

    def __init__(
        self,
        gnupg_home: Path | str | None = None,
        node_name: str = "",
    ) -> None:
        """Initialize GPG manager.

        Args:
            gnupg_home: Path to GPG home directory
            node_name: Name of the local node
        """
        self.node_name = node_name
        self._gnupg_home: Path | None = None

        if gnupg_home:
            self._gnupg_home = Path(gnupg_home)
            ensure_dir(self._gnupg_home, mode=0o700)

        self._gpg = gnupg.GPG(gnupghome=str(self._gnupg_home) if self._gnupg_home else None)

    def generate_key(
        self,
        name: str,
        email: str,
        passphrase: str | None = None,
        key_type: str = "RSA",
        key_length: int = 4096,
        expire_date: str = "0",  # Never expire
    ) -> GPGKeyInfo:
        """Generate a new GPG keypair.

        Args:
            name: Real name for the key
            email: Email address for the key
            passphrase: Optional passphrase (None for no passphrase)
            key_type: Key algorithm (RSA, DSA, etc.)
            key_length: Key length in bits
            expire_date: Expiration (0 = never, 1y, 2m, etc.)

        Returns:
            Information about the generated key
        """
        logger.info("Generating GPG key", name=name, email=email)

        input_data = self._gpg.gen_key_input(
            key_type=key_type,
            key_length=key_length,
            name_real=name,
            name_email=email,
            expire_date=expire_date,
            passphrase=passphrase,
        )

        key = self._gpg.gen_key(input_data)

        if not key.fingerprint:
            raise GPGError(f"Failed to generate GPG key: {key.status}")

        logger.info("GPG key generated", fingerprint=str(key))

        # Get full key info
        keys = self._gpg.list_keys(keys=[str(key)])
        if not keys:
            raise GPGError("Key generated but could not be retrieved")

        return GPGKeyInfo.from_gpg_key(keys[0])

    def import_key(self, key_data: str) -> GPGKeyInfo:
        """Import a GPG public key.

        Args:
            key_data: ASCII-armored public key

        Returns:
            Information about the imported key
        """
        result = self._gpg.import_keys(key_data)

        if not result.ok:
            raise GPGError(f"Failed to import key: {result.status}")

        if not result.fingerprints:
            raise GPGError("No keys imported")

        fingerprint = result.fingerprints[0]
        logger.info("Imported GPG key", fingerprint=fingerprint)

        # Get key info
        keys = self._gpg.list_keys(keys=[fingerprint])
        if not keys:
            raise GPGError("Key imported but could not be retrieved")

        return GPGKeyInfo.from_gpg_key(keys[0])

    def export_public_key(self, key_id: str) -> str:
        """Export a public key in ASCII-armored format.

        Args:
            key_id: Key ID or fingerprint

        Returns:
            ASCII-armored public key
        """
        result = self._gpg.export_keys(key_id, armor=True)

        if not result:
            raise GPGError(f"Failed to export public key: {key_id}")

        return str(result)

    def export_private_key(self, key_id: str, passphrase: str | None = None) -> str:
        """Export a private key in ASCII-armored format.

        Args:
            key_id: Key ID or fingerprint
            passphrase: Passphrase for the key

        Returns:
            ASCII-armored private key
        """
        result = self._gpg.export_keys(key_id, secret=True, armor=True, passphrase=passphrase)

        if not result:
            raise GPGError(f"Failed to export private key: {key_id}")

        return str(result)

    def list_keys(self, secret: bool = False) -> list[GPGKeyInfo]:
        """List all keys in the keyring.

        Args:
            secret: If True, list secret keys instead of public keys

        Returns:
            List of key information
        """
        keys = self._gpg.list_keys(secret=secret)
        return [GPGKeyInfo.from_gpg_key(k) for k in keys]

    def get_key(self, key_id: str) -> GPGKeyInfo | None:
        """Get information about a specific key.

        Args:
            key_id: Key ID or fingerprint

        Returns:
            Key information or None if not found
        """
        keys = self._gpg.list_keys(keys=[key_id])
        if not keys:
            return None
        return GPGKeyInfo.from_gpg_key(keys[0])

    def delete_key(self, key_id: str, secret: bool = False) -> bool:
        """Delete a key from the keyring.

        Args:
            key_id: Key ID or fingerprint
            secret: If True, delete secret key first

        Returns:
            True if deleted successfully
        """
        if secret:
            result = self._gpg.delete_keys(key_id, secret=True)
            if not result.ok:
                logger.warning("Failed to delete secret key", key_id=key_id)
                return False

        result = self._gpg.delete_keys(key_id)
        return bool(result.ok)

    def sign_data(self, data: str | bytes, key_id: str, passphrase: str | None = None) -> str:
        """Sign data with a private key.

        Args:
            data: Data to sign
            key_id: Key ID to sign with
            passphrase: Key passphrase

        Returns:
            ASCII-armored signature
        """
        if isinstance(data, str):
            data = data.encode()

        result = self._gpg.sign(data, keyid=key_id, passphrase=passphrase, detach=True)

        if not result.ok:
            raise GPGError(f"Failed to sign data: {result.status}")

        return str(result)

    def verify_signature(self, data: str | bytes, signature: str) -> bool:
        """Verify a detached signature.

        Args:
            data: Original data
            signature: Detached signature

        Returns:
            True if signature is valid
        """
        if isinstance(data, str):
            data = data.encode()

        # Write data to temp file for verification
        import tempfile

        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(data)
            data_file = f.name

        try:
            result = self._gpg.verify_data(signature, data)
            return bool(result.valid)
        finally:
            Path(data_file).unlink(missing_ok=True)

    def encrypt(
        self,
        data: str | bytes,
        recipients: list[str],
        sign: bool = False,
        passphrase: str | None = None,
    ) -> str:
        """Encrypt data for one or more recipients.

        Args:
            data: Data to encrypt
            recipients: List of recipient key IDs
            sign: Whether to sign the encrypted data
            passphrase: Signing key passphrase (if signing)

        Returns:
            ASCII-armored encrypted data
        """
        if isinstance(data, str):
            data = data.encode()

        result = self._gpg.encrypt(
            data,
            recipients,
            sign=sign,
            passphrase=passphrase,
            armor=True,
        )

        if not result.ok:
            raise GPGError(f"Failed to encrypt data: {result.status}")

        return str(result)

    def decrypt(self, data: str, passphrase: str | None = None) -> str:
        """Decrypt data.

        Args:
            data: Encrypted data
            passphrase: Decryption key passphrase

        Returns:
            Decrypted data
        """
        result = self._gpg.decrypt(data, passphrase=passphrase)

        if not result.ok:
            raise GPGError(f"Failed to decrypt data: {result.status}")

        return str(result)
