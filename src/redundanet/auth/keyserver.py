"""Keyserver interactions for RedundaNet."""

from __future__ import annotations

from typing import TYPE_CHECKING

import httpx

from redundanet.core.exceptions import KeyServerError
from redundanet.utils.logging import get_logger

if TYPE_CHECKING:
    from redundanet.auth.gpg import GPGManager

logger = get_logger(__name__)

# Well-known keyservers
DEFAULT_KEYSERVERS = [
    "keys.openpgp.org",
    "keyserver.ubuntu.com",
    "pgp.mit.edu",
]


class KeyServerClient:
    """Client for interacting with GPG keyservers."""

    def __init__(
        self,
        gpg_manager: GPGManager,
        keyservers: list[str] | None = None,
        timeout: float = 30.0,
    ) -> None:
        """Initialize keyserver client.

        Args:
            gpg_manager: GPG manager instance
            keyservers: List of keyserver hostnames
            timeout: HTTP request timeout in seconds
        """
        self.gpg = gpg_manager
        self.keyservers = keyservers or DEFAULT_KEYSERVERS
        self.timeout = timeout
        self._client = httpx.Client(timeout=timeout)

    def __del__(self) -> None:
        """Clean up HTTP client."""
        if hasattr(self, "_client"):
            self._client.close()

    def search_key(self, search_term: str) -> list[dict[str, str]]:
        """Search for keys on keyservers.

        Args:
            search_term: Email, key ID, or name to search for

        Returns:
            List of matching keys with their info
        """
        results = []

        for server in self.keyservers:
            try:
                url = f"https://{server}/pks/lookup"
                params = {
                    "op": "index",
                    "search": search_term,
                    "options": "mr",  # Machine readable
                }

                response = self._client.get(url, params=params)
                response.raise_for_status()

                # Parse machine-readable output
                for line in response.text.split("\n"):
                    if line.startswith("pub:"):
                        parts = line.split(":")
                        if len(parts) >= 5:
                            results.append(
                                {
                                    "key_id": parts[1],
                                    "created": parts[4],
                                    "keyserver": server,
                                }
                            )
                    elif line.startswith("uid:"):
                        parts = line.split(":")
                        if len(parts) >= 2 and results:
                            results[-1]["uid"] = parts[1]

                if results:
                    break  # Found results, no need to try other servers

            except httpx.HTTPError as e:
                logger.debug("Keyserver search failed", server=server, error=str(e))
                continue

        return results

    def fetch_key(self, key_id: str) -> str | None:
        """Fetch a key from keyservers.

        Args:
            key_id: Key ID or fingerprint

        Returns:
            ASCII-armored public key, or None if not found
        """
        # Clean up key ID
        key_id = key_id.replace(" ", "").upper()
        if not key_id.startswith("0x"):
            key_id = f"0x{key_id}"

        for server in self.keyservers:
            try:
                url = f"https://{server}/pks/lookup"
                params = {
                    "op": "get",
                    "search": key_id,
                    "options": "mr",
                }

                response = self._client.get(url, params=params)
                response.raise_for_status()

                if "BEGIN PGP PUBLIC KEY BLOCK" in response.text:
                    logger.info("Fetched key from keyserver", key_id=key_id, server=server)
                    return response.text

            except httpx.HTTPError as e:
                logger.debug("Keyserver fetch failed", server=server, error=str(e))
                continue

        logger.warning("Key not found on any keyserver", key_id=key_id)
        return None

    def upload_key(self, key_id: str) -> bool:
        """Upload a public key to keyservers.

        Args:
            key_id: Key ID to upload

        Returns:
            True if upload succeeded
        """
        try:
            public_key = self.gpg.export_public_key(key_id)
        except Exception as e:
            raise KeyServerError(f"Failed to export key: {e}") from e

        uploaded = False

        for server in self.keyservers:
            try:
                url = f"https://{server}/pks/add"
                data = {"keytext": public_key}

                response = self._client.post(url, data=data)
                response.raise_for_status()

                logger.info("Uploaded key to keyserver", key_id=key_id, server=server)
                uploaded = True

            except httpx.HTTPError as e:
                logger.debug("Keyserver upload failed", server=server, error=str(e))
                continue

        return uploaded

    def import_key_from_server(self, key_id: str) -> bool:
        """Fetch a key from keyservers and import it.

        Args:
            key_id: Key ID to fetch and import

        Returns:
            True if import succeeded
        """
        key_data = self.fetch_key(key_id)

        if not key_data:
            return False

        try:
            self.gpg.import_key(key_data)
            return True
        except Exception as e:
            logger.error("Failed to import key", key_id=key_id, error=str(e))
            return False

    def verify_key_on_server(self, key_id: str) -> bool:
        """Verify that a key exists on keyservers.

        Args:
            key_id: Key ID to verify

        Returns:
            True if key exists on at least one keyserver
        """
        return self.fetch_key(key_id) is not None

    def refresh_keys(self, key_ids: list[str] | None = None) -> dict[str, bool]:
        """Refresh keys from keyservers.

        Args:
            key_ids: List of key IDs to refresh, or None for all keys

        Returns:
            Dictionary mapping key ID to success status
        """
        if key_ids is None:
            key_ids = [k.key_id for k in self.gpg.list_keys()]

        results = {}

        for key_id in key_ids:
            results[key_id] = self.import_key_from_server(key_id)

        return results
