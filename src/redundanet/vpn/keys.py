"""VPN key management for RedundaNet."""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from redundanet.core.exceptions import VPNError
from redundanet.utils.files import ensure_dir, read_file, write_file
from redundanet.utils.logging import get_logger

if TYPE_CHECKING:
    from redundanet.vpn.tinc import TincConfig

logger = get_logger(__name__)


class VPNKeyManager:
    """Manages VPN keys for node authentication."""

    def __init__(self, config: TincConfig) -> None:
        self.config = config
        self._keys_dir = config.network_dir / "keys"

    def ensure_keys_directory(self) -> Path:
        """Ensure the keys directory exists."""
        return ensure_dir(self._keys_dir, mode=0o700)

    def import_public_key(self, node_name: str, public_key: str) -> Path:
        """Import a public key for a remote node.

        Args:
            node_name: Name of the remote node
            public_key: The public key content

        Returns:
            Path to the saved key file
        """
        if not public_key.strip():
            raise VPNError(f"Empty public key for node {node_name}")

        # Validate it looks like an RSA public key
        if "BEGIN RSA PUBLIC KEY" not in public_key:
            raise VPNError(f"Invalid public key format for node {node_name}")

        host_file = self.config.hosts_dir / node_name

        # Read existing host file or create new one
        if host_file.exists():
            content = read_file(host_file)
            # Remove old key if present
            lines = content.split("\n")
            new_lines = []
            in_key = False
            for line in lines:
                if "BEGIN RSA PUBLIC KEY" in line:
                    in_key = True
                if not in_key:
                    new_lines.append(line)
                if "END RSA PUBLIC KEY" in line:
                    in_key = False

            content = "\n".join(new_lines)
        else:
            content = f"# Host file for {node_name}\n"

        # Append the new key
        content = content.rstrip() + "\n\n" + public_key.strip() + "\n"

        write_file(host_file, content, mode=0o644)
        logger.info("Imported public key", node=node_name)

        return host_file

    def export_public_key(self) -> str:
        """Export this node's public key.

        Returns:
            The public key as a string
        """
        host_file = self.config.hosts_dir / self.config.node_name

        if not host_file.exists():
            raise VPNError(f"No public key found for {self.config.node_name}")

        content = read_file(host_file)

        # Extract just the key portion
        lines = content.split("\n")
        key_lines = []
        in_key = False

        for line in lines:
            if "BEGIN RSA PUBLIC KEY" in line:
                in_key = True
            if in_key:
                key_lines.append(line)
            if "END RSA PUBLIC KEY" in line:
                break

        if not key_lines:
            raise VPNError(f"No public key found in host file for {self.config.node_name}")

        return "\n".join(key_lines)

    def export_host_file(self) -> str:
        """Export the complete host file for this node.

        Returns:
            The complete host file content
        """
        host_file = self.config.hosts_dir / self.config.node_name

        if not host_file.exists():
            raise VPNError(f"No host file found for {self.config.node_name}")

        return read_file(host_file)

    def list_imported_keys(self) -> list[str]:
        """List all imported node keys.

        Returns:
            List of node names with imported keys
        """
        nodes = []
        if self.config.hosts_dir.exists():
            for host_file in self.config.hosts_dir.iterdir():
                if host_file.is_file() and host_file.name != self.config.node_name:
                    content = read_file(host_file)
                    if "BEGIN RSA PUBLIC KEY" in content:
                        nodes.append(host_file.name)
        return sorted(nodes)

    def remove_key(self, node_name: str) -> bool:
        """Remove a node's key.

        Args:
            node_name: Name of the node to remove

        Returns:
            True if removed, False if not found
        """
        if node_name == self.config.node_name:
            raise VPNError("Cannot remove own node's key")

        host_file = self.config.hosts_dir / node_name
        if host_file.exists():
            host_file.unlink()
            logger.info("Removed key", node=node_name)
            return True
        return False

    def verify_key(self, node_name: str) -> bool:
        """Verify that a node's key is properly formatted.

        Args:
            node_name: Name of the node to verify

        Returns:
            True if key is valid
        """
        host_file = self.config.hosts_dir / node_name
        if not host_file.exists():
            return False

        content = read_file(host_file)

        # Check for required components
        has_subnet = "Subnet = " in content
        has_key_start = "BEGIN RSA PUBLIC KEY" in content
        has_key_end = "END RSA PUBLIC KEY" in content

        return has_subnet and has_key_start and has_key_end
