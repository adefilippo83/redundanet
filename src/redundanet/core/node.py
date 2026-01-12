"""Node representation and management for RedundaNet."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import TYPE_CHECKING

from redundanet.core.config import NodeRole

if TYPE_CHECKING:
    from redundanet.core.config import NodeConfig


class NodeStatus(str, Enum):
    """Runtime status of a node."""

    ONLINE = "online"
    OFFLINE = "offline"
    CONNECTING = "connecting"
    ERROR = "error"
    UNKNOWN = "unknown"


@dataclass
class NodeHealth:
    """Health information for a node."""

    vpn_connected: bool = False
    storage_available: bool = False
    introducer_reachable: bool = False
    last_seen: datetime | None = None
    uptime_seconds: int = 0
    errors: list[str] = field(default_factory=list)

    @property
    def is_healthy(self) -> bool:
        """Check if the node is in a healthy state."""
        return self.vpn_connected and len(self.errors) == 0


@dataclass
class Node:
    """Represents a node in the RedundaNet network."""

    name: str
    internal_ip: str
    vpn_ip: str
    roles: list[NodeRole] = field(default_factory=list)
    public_ip: str | None = None
    gpg_key_id: str | None = None
    region: str | None = None
    is_publicly_accessible: bool = False
    storage_contribution: str | None = None
    storage_allocation: str | None = None

    # Runtime state
    status: NodeStatus = NodeStatus.UNKNOWN
    health: NodeHealth = field(default_factory=NodeHealth)

    @classmethod
    def from_config(cls, config: NodeConfig) -> Node:
        """Create a Node instance from NodeConfig."""
        return cls(
            name=config.name,
            internal_ip=config.internal_ip,
            vpn_ip=config.vpn_ip or config.internal_ip,
            roles=[NodeRole(r.value) for r in config.roles],
            public_ip=config.public_ip,
            gpg_key_id=config.gpg_key_id,
            region=config.region,
            is_publicly_accessible=config.is_publicly_accessible,
            storage_contribution=config.storage_contribution,
            storage_allocation=config.storage_allocation,
        )

    def has_role(self, role: NodeRole) -> bool:
        """Check if this node has a specific role."""
        return role in self.roles

    @property
    def is_introducer(self) -> bool:
        """Check if this node is a Tahoe-LAFS introducer."""
        return self.has_role(NodeRole.TAHOE_INTRODUCER)

    @property
    def is_storage(self) -> bool:
        """Check if this node provides storage."""
        return self.has_role(NodeRole.TAHOE_STORAGE)

    @property
    def is_client(self) -> bool:
        """Check if this node is a Tahoe-LAFS client."""
        return self.has_role(NodeRole.TAHOE_CLIENT)

    @property
    def is_vpn_node(self) -> bool:
        """Check if this node participates in the VPN mesh."""
        return self.has_role(NodeRole.TINC_VPN)

    @property
    def fqdn(self) -> str:
        """Get the fully qualified domain name for this node."""
        return f"{self.name}.redundanet.local"

    def to_dict(self) -> dict[str, object]:
        """Convert node to dictionary representation."""
        return {
            "name": self.name,
            "internal_ip": self.internal_ip,
            "vpn_ip": self.vpn_ip,
            "public_ip": self.public_ip,
            "gpg_key_id": self.gpg_key_id,
            "region": self.region,
            "roles": [r.value for r in self.roles],
            "is_publicly_accessible": self.is_publicly_accessible,
            "storage_contribution": self.storage_contribution,
            "storage_allocation": self.storage_allocation,
            "status": self.status.value,
        }


@dataclass
class LocalNode(Node):
    """Represents the local node with additional local-only information."""

    config_path: str | None = None
    data_path: str | None = None
    private_key_path: str | None = None

    @property
    def is_configured(self) -> bool:
        """Check if the local node is properly configured."""
        return bool(self.config_path and self.private_key_path)
