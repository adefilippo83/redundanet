"""Configuration management for RedundaNet using Pydantic."""

from __future__ import annotations

import ipaddress
import re
from enum import Enum
from pathlib import Path
from typing import Annotated, Self

from pydantic import BaseModel, Field, field_validator, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class NodeRole(str, Enum):
    """Available roles for a RedundaNet node."""

    TINC_VPN = "tinc_vpn"
    TAHOE_INTRODUCER = "tahoe_introducer"
    TAHOE_STORAGE = "tahoe_storage"
    TAHOE_CLIENT = "tahoe_client"


class NodeStatus(str, Enum):
    """Status of a node in the network."""

    ACTIVE = "active"
    PENDING = "pending"
    INACTIVE = "inactive"


class TahoeConfig(BaseModel):
    """Tahoe-LAFS erasure coding configuration."""

    shares_needed: Annotated[int, Field(ge=1, le=256)] = 3
    shares_happy: Annotated[int, Field(ge=1, le=256)] = 7
    shares_total: Annotated[int, Field(ge=1, le=256)] = 10
    reserved_space: str = "50G"

    @model_validator(mode="after")
    def validate_shares(self) -> Self:
        """Ensure shares configuration is valid: needed <= happy <= total."""
        if not (self.shares_needed <= self.shares_happy <= self.shares_total):
            raise ValueError(
                f"Invalid shares configuration: needed ({self.shares_needed}) <= "
                f"happy ({self.shares_happy}) <= total ({self.shares_total}) must be satisfied"
            )
        return self

    @field_validator("reserved_space")
    @classmethod
    def validate_reserved_space(cls, v: str) -> str:
        """Validate reserved space format (e.g., 50G, 100M, 1T)."""
        pattern = r"^\d+[KMGT]?$"
        if not re.match(pattern, v.upper()):
            raise ValueError(
                f"Invalid reserved_space format: {v}. "
                "Expected format: number followed by K, M, G, or T (e.g., 50G)"
            )
        return v.upper()


class PortConfig(BaseModel):
    """Port configuration for node services."""

    tinc: int = 655
    tahoe_storage: int = 3457
    tahoe_client: int = 3456
    tahoe_introducer: int = 3458


class NodeConfig(BaseModel):
    """Configuration for a single RedundaNet node."""

    name: Annotated[str, Field(min_length=1, max_length=64, pattern=r"^[a-zA-Z][a-zA-Z0-9_-]*$")]
    internal_ip: str
    vpn_ip: str | None = None
    public_ip: str | None = None
    gpg_key_id: str | None = None
    region: str | None = None
    status: NodeStatus = NodeStatus.PENDING
    roles: list[NodeRole] = Field(default_factory=list)
    ports: PortConfig = Field(default_factory=PortConfig)
    storage_contribution: str | None = None
    storage_allocation: str | None = None
    is_publicly_accessible: bool = False

    @field_validator("internal_ip", "vpn_ip", "public_ip", mode="before")
    @classmethod
    def validate_ip(cls, v: str | None) -> str | None:
        """Validate IP address format."""
        if v is None or v == "auto":
            return v
        try:
            ipaddress.ip_address(v)
            return v
        except ValueError as e:
            raise ValueError(f"Invalid IP address: {v}") from e

    @field_validator("gpg_key_id")
    @classmethod
    def validate_gpg_key_id(cls, v: str | None) -> str | None:
        """Validate GPG key ID format (8, 16, or 40 character hex string)."""
        if v is None:
            return None
        v = v.upper().replace(" ", "")
        if not re.match(r"^[A-F0-9]{8}$|^[A-F0-9]{16}$|^[A-F0-9]{40}$", v):
            raise ValueError(
                f"Invalid GPG key ID: {v}. Expected 8, 16, or 40 character hex string."
            )
        return v

    @model_validator(mode="after")
    def set_vpn_ip_default(self) -> Self:
        """Set vpn_ip to internal_ip if not specified."""
        if self.vpn_ip is None:
            self.vpn_ip = self.internal_ip
        return self

    def has_role(self, role: NodeRole) -> bool:
        """Check if this node has a specific role."""
        return role in self.roles


class NetworkConfig(BaseModel):
    """Network-wide configuration for RedundaNet."""

    name: Annotated[str, Field(min_length=1, max_length=64)] = "redundanet"
    version: str = "2.0.0"
    domain: str = "redundanet.local"
    vpn_network: str = "10.100.0.0/16"
    tahoe: TahoeConfig = Field(default_factory=TahoeConfig)

    @field_validator("vpn_network")
    @classmethod
    def validate_vpn_network(cls, v: str) -> str:
        """Validate VPN network CIDR format."""
        try:
            ipaddress.ip_network(v)
            return v
        except ValueError as e:
            raise ValueError(f"Invalid VPN network CIDR: {v}") from e


class AppSettings(BaseSettings):
    """Application-wide settings loaded from environment variables."""

    model_config = SettingsConfigDict(
        env_prefix="REDUNDANET_",
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
    )

    # Node identification
    node_name: str = ""
    internal_vpn_ip: str = ""
    public_ip: str = "auto"
    gpg_key_id: str = ""

    # Repository settings
    manifest_repo: str = ""
    manifest_branch: str = "main"
    manifest_filename: str = "manifest.yaml"

    # Paths
    config_dir: Path = Path("/etc/redundanet")
    data_dir: Path = Path("/var/lib/redundanet")
    log_dir: Path = Path("/var/log/redundanet")

    # Runtime settings
    debug: bool = False
    test_mode: bool = False
    log_level: str = "INFO"

    # Network settings
    enable_auto_discovery: bool = True
    sync_interval: int = 300  # seconds

    @field_validator("log_level")
    @classmethod
    def validate_log_level(cls, v: str) -> str:
        """Validate log level."""
        valid_levels = {"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}
        v = v.upper()
        if v not in valid_levels:
            raise ValueError(f"Invalid log level: {v}. Valid levels: {valid_levels}")
        return v


def load_settings() -> AppSettings:
    """Load application settings from environment."""
    return AppSettings()


def get_default_manifest_path(settings: AppSettings | None = None) -> Path:
    """Get the default manifest file path."""
    if settings is None:
        settings = load_settings()
    return settings.data_dir / "manifest" / settings.manifest_filename
