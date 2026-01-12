"""FURL management for Tahoe-LAFS introducers."""

from __future__ import annotations

import re
from pathlib import Path

from redundanet.core.exceptions import StorageError
from redundanet.utils.files import read_file, read_yaml, write_file, write_yaml
from redundanet.utils.logging import get_logger

logger = get_logger(__name__)

# FURL format: pb://<tubid>@<location>/<swissnum>
FURL_PATTERN = re.compile(r"^pb://([a-z2-7]+)@([^/]+)/([a-z2-7]+)$")


def parse_furl(furl: str) -> dict[str, str]:
    """Parse a FURL into its components.

    Args:
        furl: The FURL string

    Returns:
        Dictionary with tubid, location, and swissnum
    """
    match = FURL_PATTERN.match(furl.strip())
    if not match:
        raise StorageError(f"Invalid FURL format: {furl}")

    return {
        "tubid": match.group(1),
        "location": match.group(2),
        "swissnum": match.group(3),
    }


def validate_furl(furl: str) -> bool:
    """Validate a FURL format.

    Args:
        furl: The FURL string

    Returns:
        True if valid
    """
    try:
        parse_furl(furl)
        return True
    except StorageError:
        return False


class FURLManager:
    """Manages FURL distribution and synchronization."""

    def __init__(
        self,
        manifest_path: Path,
        cache_path: Path | None = None,
    ) -> None:
        """Initialize FURL manager.

        Args:
            manifest_path: Path to the network manifest
            cache_path: Path to cache the FURL locally
        """
        self.manifest_path = manifest_path
        self.cache_path = cache_path or Path("/var/lib/redundanet/introducer_furl.cache")

    def get_cached_furl(self) -> str | None:
        """Get the cached FURL if available.

        Returns:
            Cached FURL or None
        """
        if self.cache_path.exists():
            try:
                return read_file(self.cache_path).strip()
            except Exception:
                return None
        return None

    def cache_furl(self, furl: str) -> None:
        """Cache a FURL locally.

        Args:
            furl: FURL to cache
        """
        if not validate_furl(furl):
            raise StorageError(f"Cannot cache invalid FURL: {furl}")

        self.cache_path.parent.mkdir(parents=True, exist_ok=True)
        write_file(self.cache_path, furl, mode=0o600)
        logger.debug("Cached FURL", path=str(self.cache_path))

    def get_furl_from_manifest(self) -> str | None:
        """Get the introducer FURL from the manifest.

        Returns:
            FURL from manifest or None
        """
        if not self.manifest_path.exists():
            return None

        try:
            data = read_yaml(self.manifest_path)
            furl = data.get("introducer_furl")
            if furl and furl != "null":
                return str(furl)
            return None
        except Exception as e:
            logger.error("Failed to read FURL from manifest", error=str(e))
            return None

    def update_manifest_furl(self, furl: str) -> None:
        """Update the FURL in the manifest.

        Args:
            furl: New FURL to set
        """
        if not validate_furl(furl):
            raise StorageError(f"Cannot update manifest with invalid FURL: {furl}")

        if not self.manifest_path.exists():
            raise StorageError(f"Manifest not found: {self.manifest_path}")

        data = read_yaml(self.manifest_path)
        data["introducer_furl"] = furl
        write_yaml(self.manifest_path, data)

        logger.info("Updated manifest with FURL", furl=furl[:50] + "...")

    def get_effective_furl(self) -> str | None:
        """Get the effective FURL to use.

        Checks manifest first, then cache.

        Returns:
            FURL to use or None
        """
        # First try manifest
        furl = self.get_furl_from_manifest()
        if furl:
            # Update cache
            self.cache_furl(furl)
            return furl

        # Fall back to cache
        return self.get_cached_furl()

    def sync_furl(self, local_furl: str | None = None) -> str | None:
        """Synchronize FURL between local and manifest.

        If local_furl is provided (this is an introducer), update the manifest.
        Otherwise, fetch FURL from manifest and cache it.

        Args:
            local_furl: Local FURL if this is an introducer

        Returns:
            The synchronized FURL
        """
        if local_furl:
            # This is an introducer - publish FURL to manifest
            if validate_furl(local_furl):
                self.update_manifest_furl(local_furl)
                self.cache_furl(local_furl)
                return local_furl
            else:
                raise StorageError(f"Invalid local FURL: {local_furl}")
        else:
            # This is a client/storage - get FURL from manifest
            furl = self.get_effective_furl()
            if furl:
                self.cache_furl(furl)
            return furl

    def extract_introducer_ip(self) -> str | None:
        """Extract the introducer IP from the FURL.

        Returns:
            IP address of the introducer or None
        """
        furl = self.get_effective_furl()
        if not furl:
            return None

        try:
            parsed = parse_furl(furl)
            location = parsed["location"]
            # Location format: ip:port or hostname:port
            host = location.split(":")[0]
            return host
        except Exception:
            return None
