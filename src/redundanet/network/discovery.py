"""Node discovery for RedundaNet."""

from __future__ import annotations

import contextlib
from dataclasses import dataclass, field
from datetime import datetime
from typing import TYPE_CHECKING

from redundanet.utils.logging import get_logger
from redundanet.utils.process import run_command

if TYPE_CHECKING:
    from redundanet.core.config import NodeConfig
    from redundanet.core.manifest import Manifest

logger = get_logger(__name__)


@dataclass
class DiscoveredNode:
    """Information about a discovered node."""

    name: str
    vpn_ip: str
    reachable: bool = False
    latency_ms: float | None = None
    last_seen: datetime | None = None
    services: list[str] = field(default_factory=list)


class NodeDiscovery:
    """Discovers and monitors nodes in the network."""

    def __init__(
        self,
        manifest: Manifest,
        local_node_name: str,
    ) -> None:
        """Initialize node discovery.

        Args:
            manifest: Network manifest
            local_node_name: Name of the local node
        """
        self.manifest = manifest
        self.local_node_name = local_node_name
        self._discovered: dict[str, DiscoveredNode] = {}

    def discover_all(self) -> dict[str, DiscoveredNode]:
        """Discover all nodes defined in the manifest.

        Returns:
            Dictionary mapping node name to DiscoveredNode
        """
        logger.info("Discovering nodes", count=len(self.manifest.nodes))

        for node in self.manifest.nodes:
            if node.name == self.local_node_name:
                continue

            discovered = self._probe_node(node)
            self._discovered[node.name] = discovered

            if discovered.reachable:
                logger.debug(
                    "Node reachable",
                    node=node.name,
                    latency_ms=discovered.latency_ms,
                )
            else:
                logger.debug("Node unreachable", node=node.name)

        return self._discovered

    def _probe_node(self, node: NodeConfig) -> DiscoveredNode:
        """Probe a single node for reachability.

        Args:
            node: Node configuration

        Returns:
            DiscoveredNode with probe results
        """
        vpn_ip = node.vpn_ip or node.internal_ip

        discovered = DiscoveredNode(
            name=node.name,
            vpn_ip=vpn_ip,
        )

        # Ping the node
        result = run_command(f"ping -c 2 -W 2 -q {vpn_ip}", check=False)

        if result.success:
            discovered.reachable = True
            discovered.last_seen = datetime.now()

            # Parse latency from ping output
            for line in result.stdout.split("\n"):
                if "rtt" in line or "round-trip" in line:
                    parts = line.split("=")
                    if len(parts) >= 2:
                        times = parts[1].strip().split("/")
                        if len(times) >= 2:
                            with contextlib.suppress(ValueError):
                                discovered.latency_ms = float(times[1])
                    break

            # Probe services
            discovered.services = self._probe_services(vpn_ip, node)

        return discovered

    def _probe_services(self, ip: str, node: NodeConfig) -> list[str]:
        """Probe which services are available on a node.

        Args:
            ip: Node IP address
            node: Node configuration

        Returns:
            List of available service names
        """
        services = []

        # Check Tahoe-LAFS ports based on roles
        role_ports = {
            "tahoe_introducer": node.ports.tahoe_introducer,
            "tahoe_storage": node.ports.tahoe_storage,
            "tahoe_client": node.ports.tahoe_client,
        }

        for role in node.roles:
            role_str = role.value
            if role_str in role_ports:
                port = role_ports[role_str]
                if self._port_open(ip, port):
                    services.append(role_str)

        return services

    def _port_open(self, ip: str, port: int, timeout: float = 2.0) -> bool:
        """Check if a TCP port is open.

        Args:
            ip: IP address
            port: Port number
            timeout: Timeout in seconds

        Returns:
            True if port is open
        """
        result = run_command(
            f"nc -z -w {int(timeout)} {ip} {port}",
            check=False,
            timeout=timeout + 1,
        )
        return result.success

    def get_reachable_nodes(self) -> list[DiscoveredNode]:
        """Get all reachable nodes.

        Returns:
            List of reachable nodes
        """
        return [n for n in self._discovered.values() if n.reachable]

    def get_unreachable_nodes(self) -> list[DiscoveredNode]:
        """Get all unreachable nodes.

        Returns:
            List of unreachable nodes
        """
        return [n for n in self._discovered.values() if not n.reachable]

    def get_nodes_with_service(self, service: str) -> list[DiscoveredNode]:
        """Get nodes that have a specific service running.

        Args:
            service: Service name (e.g., 'tahoe_storage')

        Returns:
            List of nodes with the service
        """
        return [n for n in self._discovered.values() if service in n.services]

    def refresh_node(self, node_name: str) -> DiscoveredNode | None:
        """Refresh the status of a single node.

        Args:
            node_name: Name of the node to refresh

        Returns:
            Updated DiscoveredNode or None if not found
        """
        node = self.manifest.get_node(node_name)
        if not node:
            return None

        discovered = self._probe_node(node)
        self._discovered[node_name] = discovered
        return discovered

    def get_discovery_stats(self) -> dict[str, int]:
        """Get discovery statistics.

        Returns:
            Dictionary with stats
        """
        total = len(self._discovered)
        reachable = len(self.get_reachable_nodes())

        return {
            "total_nodes": total,
            "reachable": reachable,
            "unreachable": total - reachable,
        }
