"""DNS and hosts file management for RedundaNet."""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from redundanet.utils.files import read_file, write_file
from redundanet.utils.logging import get_logger

if TYPE_CHECKING:
    from redundanet.core.manifest import Manifest

logger = get_logger(__name__)

HOSTS_MARKER_START = "# BEGIN REDUNDANET NODES"
HOSTS_MARKER_END = "# END REDUNDANET NODES"


class DNSManager:
    """Manages local DNS resolution for network nodes."""

    def __init__(
        self,
        manifest: Manifest,
        hosts_file: Path = Path("/etc/hosts"),
        domain: str = "redundanet.local",
    ) -> None:
        """Initialize DNS manager.

        Args:
            manifest: Network manifest
            hosts_file: Path to hosts file
            domain: Domain name for nodes
        """
        self.manifest = manifest
        self.hosts_file = hosts_file
        self.domain = domain
        self._resolution_cache: dict[str, str] = {}

    def update_hosts_file(self) -> None:
        """Update /etc/hosts with node entries."""
        logger.info("Updating hosts file", path=str(self.hosts_file))

        # Read current hosts file
        content = read_file(self.hosts_file) if self.hosts_file.exists() else ""

        # Remove existing RedundaNet entries
        lines = content.split("\n")
        new_lines = []
        in_redundanet_section = False

        for line in lines:
            if HOSTS_MARKER_START in line:
                in_redundanet_section = True
                continue
            if HOSTS_MARKER_END in line:
                in_redundanet_section = False
                continue
            if not in_redundanet_section:
                new_lines.append(line)

        # Add new RedundaNet entries
        new_lines.append("")
        new_lines.append(HOSTS_MARKER_START)

        for node in self.manifest.nodes:
            ip = node.vpn_ip or node.internal_ip
            fqdn = f"{node.name}.{self.domain}"
            new_lines.append(f"{ip} {node.name} {fqdn}")
            self._resolution_cache[node.name] = ip
            self._resolution_cache[fqdn] = ip

        new_lines.append(HOSTS_MARKER_END)
        new_lines.append("")

        # Write updated hosts file
        content = "\n".join(new_lines)
        write_file(self.hosts_file, content, mode=0o644)

        logger.info("Hosts file updated", entries=len(self.manifest.nodes))

    def resolve_node(self, name: str) -> str | None:
        """Resolve a node name to IP address.

        Args:
            name: Node name or FQDN

        Returns:
            IP address or None
        """
        # Check cache first
        if name in self._resolution_cache:
            return self._resolution_cache[name]

        # Check FQDN
        fqdn = f"{name}.{self.domain}"
        if fqdn in self._resolution_cache:
            return self._resolution_cache[fqdn]

        # Look up in manifest
        node = self.manifest.get_node(name)
        if node:
            ip = node.vpn_ip or node.internal_ip
            self._resolution_cache[name] = ip
            return ip

        return None

    def get_all_resolutions(self) -> dict[str, str]:
        """Get all node name to IP resolutions.

        Returns:
            Dictionary mapping names to IPs
        """
        resolutions = {}

        for node in self.manifest.nodes:
            ip = node.vpn_ip or node.internal_ip
            resolutions[node.name] = ip
            resolutions[f"{node.name}.{self.domain}"] = ip

        return resolutions

    def remove_hosts_entries(self) -> None:
        """Remove RedundaNet entries from hosts file."""
        if not self.hosts_file.exists():
            return

        content = read_file(self.hosts_file)

        # Remove RedundaNet section
        lines = content.split("\n")
        new_lines = []
        in_redundanet_section = False

        for line in lines:
            if HOSTS_MARKER_START in line:
                in_redundanet_section = True
                continue
            if HOSTS_MARKER_END in line:
                in_redundanet_section = False
                continue
            if not in_redundanet_section:
                new_lines.append(line)

        content = "\n".join(new_lines)
        write_file(self.hosts_file, content, mode=0o644)

        self._resolution_cache.clear()
        logger.info("Removed RedundaNet entries from hosts file")

    def write_resolution_file(self, path: Path) -> None:
        """Write a separate resolution file for other scripts.

        Args:
            path: Path to write resolution file
        """
        lines = []

        for node in self.manifest.nodes:
            ip = node.vpn_ip or node.internal_ip
            fqdn = f"{node.name}.{self.domain}"
            lines.append(f"{ip} {node.name} {fqdn}")

        content = "\n".join(lines) + "\n"
        write_file(path, content, mode=0o644)

        logger.debug("Wrote resolution file", path=str(path))

    def validate_resolution(self, name: str) -> bool:
        """Validate that a name can be resolved.

        Args:
            name: Name to validate

        Returns:
            True if resolvable
        """
        return self.resolve_node(name) is not None
