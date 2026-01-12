"""Manifest management for RedundaNet network configuration."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import yaml
from jsonschema import ValidationError as JsonSchemaValidationError
from jsonschema import validate

from redundanet.core.config import NetworkConfig, NodeConfig
from redundanet.core.exceptions import ManifestError, ValidationError

# JSON Schema for manifest validation
MANIFEST_SCHEMA: dict[str, Any] = {
    "$schema": "http://json-schema.org/draft-07/schema#",
    "type": "object",
    "required": ["network", "nodes"],
    "properties": {
        "network": {
            "type": "object",
            "required": ["name", "version", "domain", "vpn_network"],
            "properties": {
                "name": {"type": "string"},
                "version": {"type": "string"},
                "domain": {"type": "string"},
                "vpn_network": {
                    "type": "string",
                    "pattern": r"^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}/\d{1,2}$",
                },
                "tahoe": {
                    "type": "object",
                    "properties": {
                        "shares_needed": {"type": "integer", "minimum": 1},
                        "shares_happy": {"type": "integer", "minimum": 1},
                        "shares_total": {"type": "integer", "minimum": 1},
                        "reserved_space": {"type": "string"},
                    },
                },
            },
        },
        "introducer_furl": {"type": ["string", "null"]},
        "nodes": {
            "type": "array",
            "items": {
                "type": "object",
                "required": ["name", "internal_ip"],
                "properties": {
                    "name": {"type": "string"},
                    "internal_ip": {"type": "string"},
                    "vpn_ip": {"type": "string"},
                    "public_ip": {"type": "string"},
                    "gpg_key_id": {"type": "string"},
                    "region": {"type": "string"},
                    "status": {"type": "string", "enum": ["active", "pending", "inactive"]},
                    "roles": {
                        "type": "array",
                        "items": {
                            "type": "string",
                            "enum": [
                                "tinc_vpn",
                                "tahoe_introducer",
                                "tahoe_storage",
                                "tahoe_client",
                            ],
                        },
                    },
                    "ports": {
                        "type": "object",
                        "properties": {
                            "tinc": {"type": "integer"},
                            "tahoe_storage": {"type": "integer"},
                            "tahoe_client": {"type": "integer"},
                            "tahoe_introducer": {"type": "integer"},
                        },
                    },
                    "storage_contribution": {"type": "string"},
                    "storage_allocation": {"type": "string"},
                    "is_publicly_accessible": {"type": "boolean"},
                },
            },
        },
    },
}


class Manifest:
    """Manages the RedundaNet network manifest."""

    def __init__(
        self,
        network: NetworkConfig,
        nodes: list[NodeConfig],
        introducer_furl: str | None = None,
    ) -> None:
        self.network = network
        self.nodes = nodes
        self.introducer_furl = introducer_furl
        self._path: Path | None = None

    @classmethod
    def from_file(cls, path: Path | str) -> Manifest:
        """Load manifest from a YAML file."""
        path = Path(path)
        if not path.exists():
            raise ManifestError(f"Manifest file not found: {path}")

        try:
            with path.open() as f:
                data = yaml.safe_load(f)
        except yaml.YAMLError as e:
            raise ManifestError(f"Failed to parse YAML: {e}") from e

        manifest = cls.from_dict(data)
        manifest._path = path
        return manifest

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> Manifest:
        """Create a Manifest from a dictionary."""
        # Validate against schema first
        try:
            validate(instance=data, schema=MANIFEST_SCHEMA)
        except JsonSchemaValidationError as e:
            raise ValidationError(
                "Manifest validation failed",
                errors=[str(e.message)],
            ) from e

        # Parse network config
        network_data = data.get("network", {})
        tahoe_data = network_data.get("tahoe", {})

        from redundanet.core.config import (
            NodeRole,
            NodeStatus,
            PortConfig,
            TahoeConfig,
        )

        network = NetworkConfig(
            name=network_data.get("name", "redundanet"),
            version=network_data.get("version", "2.0.0"),
            domain=network_data.get("domain", "redundanet.local"),
            vpn_network=network_data.get("vpn_network", "10.100.0.0/16"),
            tahoe=TahoeConfig(**tahoe_data) if tahoe_data else TahoeConfig(),
        )

        # Parse nodes
        nodes: list[NodeConfig] = []
        for node_data in data.get("nodes", []):
            ports_data = node_data.get("ports", {})
            roles_data = node_data.get("roles", [])

            node = NodeConfig(
                name=node_data["name"],
                internal_ip=node_data["internal_ip"],
                vpn_ip=node_data.get("vpn_ip"),
                public_ip=node_data.get("public_ip"),
                gpg_key_id=node_data.get("gpg_key_id"),
                region=node_data.get("region"),
                status=NodeStatus(node_data.get("status", "pending")),
                roles=[NodeRole(r) for r in roles_data],
                ports=PortConfig(**ports_data) if ports_data else PortConfig(),
                storage_contribution=node_data.get("storage_contribution"),
                storage_allocation=node_data.get("storage_allocation"),
                is_publicly_accessible=node_data.get("is_publicly_accessible", False),
            )
            nodes.append(node)

        return cls(
            network=network,
            nodes=nodes,
            introducer_furl=data.get("introducer_furl"),
        )

    def to_dict(self) -> dict[str, Any]:
        """Convert manifest to dictionary representation."""

        def filter_none(d: dict[str, Any]) -> dict[str, Any]:
            """Remove None values from dictionary."""
            return {k: v for k, v in d.items() if v is not None}

        nodes_list = []
        for node in self.nodes:
            node_dict = filter_none(
                {
                    "name": node.name,
                    "internal_ip": node.internal_ip,
                    "vpn_ip": node.vpn_ip,
                    "public_ip": node.public_ip,
                    "gpg_key_id": node.gpg_key_id,
                    "region": node.region,
                    "status": node.status.value,
                    "roles": [r.value for r in node.roles],
                    "ports": {
                        "tinc": node.ports.tinc,
                        "tahoe_storage": node.ports.tahoe_storage,
                        "tahoe_client": node.ports.tahoe_client,
                        "tahoe_introducer": node.ports.tahoe_introducer,
                    },
                    "storage_contribution": node.storage_contribution,
                    "storage_allocation": node.storage_allocation,
                    "is_publicly_accessible": node.is_publicly_accessible,
                }
            )
            nodes_list.append(node_dict)

        result = {
            "network": {
                "name": self.network.name,
                "version": self.network.version,
                "domain": self.network.domain,
                "vpn_network": self.network.vpn_network,
                "tahoe": {
                    "shares_needed": self.network.tahoe.shares_needed,
                    "shares_happy": self.network.tahoe.shares_happy,
                    "shares_total": self.network.tahoe.shares_total,
                    "reserved_space": self.network.tahoe.reserved_space,
                },
            },
            "nodes": nodes_list,
        }

        if self.introducer_furl:
            result["introducer_furl"] = self.introducer_furl

        return result

    def save(self, path: Path | str | None = None) -> None:
        """Save manifest to a YAML file."""
        path = Path(path) if path else self._path
        if path is None:
            raise ManifestError("No path specified for saving manifest")

        path.parent.mkdir(parents=True, exist_ok=True)

        with path.open("w") as f:
            yaml.dump(self.to_dict(), f, default_flow_style=False, sort_keys=False)

        self._path = path

    def validate(self) -> list[str]:
        """Validate the manifest and return a list of warnings/errors."""
        errors: list[str] = []

        # Check for duplicate node names
        names = [node.name for node in self.nodes]
        duplicates = [name for name in set(names) if names.count(name) > 1]
        if duplicates:
            errors.append(f"Duplicate node names: {duplicates}")

        # Check for duplicate IPs
        ips = [node.internal_ip for node in self.nodes]
        ips.extend([node.vpn_ip for node in self.nodes if node.vpn_ip])
        duplicate_ips = [ip for ip in set(ips) if ips.count(ip) > 1]
        if duplicate_ips:
            errors.append(f"Duplicate IP addresses: {duplicate_ips}")

        # Check introducer count
        introducers = [n for n in self.nodes if "tahoe_introducer" in [r.value for r in n.roles]]
        if len(introducers) > 1:
            errors.append(
                f"Found {len(introducers)} introducer nodes. "
                "Having more than one introducer is unusual."
            )

        # Check storage node count vs shares_happy
        storage_nodes = [n for n in self.nodes if "tahoe_storage" in [r.value for r in n.roles]]
        if len(storage_nodes) < self.network.tahoe.shares_happy:
            errors.append(
                f"Not enough storage nodes ({len(storage_nodes)}) "
                f"to satisfy shares_happy ({self.network.tahoe.shares_happy})"
            )

        return errors

    def get_node(self, name: str) -> NodeConfig | None:
        """Get a node by name."""
        for node in self.nodes:
            if node.name == name:
                return node
        return None

    def get_nodes_by_role(self, role: str) -> list[NodeConfig]:
        """Get all nodes with a specific role."""
        return [node for node in self.nodes if role in [r.value for r in node.roles]]

    def get_introducers(self) -> list[NodeConfig]:
        """Get all introducer nodes."""
        return self.get_nodes_by_role("tahoe_introducer")

    def get_storage_nodes(self) -> list[NodeConfig]:
        """Get all storage nodes."""
        return self.get_nodes_by_role("tahoe_storage")

    def get_publicly_accessible_nodes(self) -> list[NodeConfig]:
        """Get all nodes that are publicly accessible."""
        return [node for node in self.nodes if node.is_publicly_accessible]

    def add_node(self, node: NodeConfig) -> None:
        """Add a new node to the manifest."""
        if self.get_node(node.name):
            raise ManifestError(f"Node '{node.name}' already exists")
        self.nodes.append(node)

    def remove_node(self, name: str) -> bool:
        """Remove a node from the manifest."""
        node = self.get_node(name)
        if node:
            self.nodes.remove(node)
            return True
        return False

    def update_introducer_furl(self, furl: str) -> None:
        """Update the introducer FURL."""
        self.introducer_furl = furl

    def export_schema(self, path: Path | str) -> None:
        """Export the JSON schema to a file."""
        path = Path(path)
        with path.open("w") as f:
            json.dump(MANIFEST_SCHEMA, f, indent=2)
