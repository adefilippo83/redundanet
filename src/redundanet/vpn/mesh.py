"""Mesh network operations for RedundaNet."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import TYPE_CHECKING

from redundanet.utils.logging import get_logger
from redundanet.utils.process import run_command

if TYPE_CHECKING:
    from redundanet.core.config import NodeConfig
    from redundanet.vpn.tinc import TincConfig

logger = get_logger(__name__)


@dataclass
class PeerInfo:
    """Information about a connected peer."""

    name: str
    vpn_ip: str
    public_ip: str | None = None
    connected: bool = False
    last_seen: datetime | None = None
    latency_ms: float | None = None


@dataclass
class MeshStatus:
    """Status of the mesh network."""

    node_name: str
    vpn_ip: str
    interface: str = "tinc0"
    is_running: bool = False
    peers: list[PeerInfo] = field(default_factory=list)
    connected_count: int = 0
    total_peers: int = 0


class MeshNetwork:
    """Manages mesh network operations and peer discovery."""

    def __init__(self, config: TincConfig) -> None:
        self.config = config
        self._interface = "tinc0"

    def get_status(self) -> MeshStatus:
        """Get the current mesh network status."""
        status = MeshStatus(
            node_name=self.config.node_name,
            vpn_ip=self.config.vpn_ip,
            interface=self._interface,
        )

        # Check if interface is up
        result = run_command(f"ip link show {self._interface}", check=False)
        status.is_running = result.success and "UP" in result.stdout

        return status

    def ping_peer(self, peer_ip: str, count: int = 3, timeout: float = 5.0) -> float | None:
        """Ping a peer and return the average latency.

        Args:
            peer_ip: IP address to ping
            count: Number of ping packets
            timeout: Timeout in seconds

        Returns:
            Average latency in milliseconds, or None if unreachable
        """
        result = run_command(
            f"ping -c {count} -W {int(timeout)} -q {peer_ip}",
            timeout=timeout * count + 5,
            check=False,
        )

        if not result.success:
            return None

        # Parse ping output for average time
        # Example: rtt min/avg/max/mdev = 0.123/0.456/0.789/0.123 ms
        for line in result.stdout.split("\n"):
            if "rtt" in line or "round-trip" in line:
                parts = line.split("=")
                if len(parts) >= 2:
                    times = parts[1].strip().split("/")
                    if len(times) >= 2:
                        try:
                            return float(times[1])  # Average time
                        except ValueError:
                            pass
        return None

    def check_peer_connectivity(self, peers: list[NodeConfig]) -> dict[str, PeerInfo]:
        """Check connectivity to all peers.

        Args:
            peers: List of peer node configurations

        Returns:
            Dictionary mapping node name to PeerInfo
        """
        results = {}

        for peer in peers:
            if peer.name == self.config.node_name:
                continue

            peer_ip = peer.vpn_ip or peer.internal_ip
            latency = self.ping_peer(peer_ip)

            info = PeerInfo(
                name=peer.name,
                vpn_ip=peer_ip,
                public_ip=peer.public_ip,
                connected=latency is not None,
                last_seen=datetime.now() if latency else None,
                latency_ms=latency,
            )
            results[peer.name] = info

            if latency:
                logger.debug("Peer reachable", peer=peer.name, latency_ms=latency)
            else:
                logger.debug("Peer unreachable", peer=peer.name)

        return results

    def get_interface_info(self) -> dict[str, str | None]:
        """Get information about the VPN interface."""
        info: dict[str, str | None] = {
            "name": self._interface,
            "ip": None,
            "state": "down",
        }

        # Get interface state
        result = run_command(f"ip link show {self._interface}", check=False)
        if result.success:
            if "UP" in result.stdout:
                info["state"] = "up"
            elif "DOWN" in result.stdout:
                info["state"] = "down"

        # Get IP address
        result = run_command(f"ip addr show {self._interface}", check=False)
        if result.success:
            for line in result.stdout.split("\n"):
                if "inet " in line:
                    parts = line.strip().split()
                    if len(parts) >= 2:
                        info["ip"] = parts[1].split("/")[0]
                    break

        return info

    def discover_active_peers(self) -> list[str]:
        """Discover which peers are currently active on the network.

        This uses ARP table and ping to find active peers.

        Returns:
            List of active peer IPs
        """
        active_peers = []

        # Check ARP table for the VPN interface
        result = run_command(f"ip neigh show dev {self._interface}", check=False)
        if result.success:
            for line in result.stdout.strip().split("\n"):
                if line and "REACHABLE" in line:
                    parts = line.split()
                    if parts:
                        active_peers.append(parts[0])

        return active_peers

    def wait_for_interface(self, timeout: float = 30.0) -> bool:
        """Wait for the VPN interface to come up.

        Args:
            timeout: Maximum time to wait in seconds

        Returns:
            True if interface came up, False if timed out
        """
        import time

        start = time.time()
        while time.time() - start < timeout:
            info = self.get_interface_info()
            if info["state"] == "up" and info["ip"]:
                return True
            time.sleep(1)

        return False
