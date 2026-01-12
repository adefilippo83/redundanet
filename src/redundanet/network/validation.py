"""Network validation for RedundaNet."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from redundanet.utils.logging import get_logger
from redundanet.utils.process import run_command

if TYPE_CHECKING:
    from redundanet.core.manifest import Manifest

logger = get_logger(__name__)


@dataclass
class ValidationResult:
    """Result of a validation check."""

    name: str
    passed: bool
    message: str
    details: dict[str, object] = field(default_factory=dict)


@dataclass
class NetworkValidationReport:
    """Complete network validation report."""

    checks: list[ValidationResult] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        """Check if all validations passed."""
        return all(c.passed for c in self.checks)

    @property
    def failed_checks(self) -> list[ValidationResult]:
        """Get all failed checks."""
        return [c for c in self.checks if not c.passed]

    def add_check(self, result: ValidationResult) -> None:
        """Add a validation result."""
        self.checks.append(result)


class NetworkValidator:
    """Validates network configuration and connectivity."""

    def __init__(
        self,
        manifest: Manifest,
        local_node_name: str,
        vpn_interface: str = "tinc0",
    ) -> None:
        """Initialize network validator.

        Args:
            manifest: Network manifest
            local_node_name: Name of the local node
            vpn_interface: VPN interface name
        """
        self.manifest = manifest
        self.local_node_name = local_node_name
        self.vpn_interface = vpn_interface

    def validate_all(self) -> NetworkValidationReport:
        """Run all validation checks.

        Returns:
            Complete validation report
        """
        report = NetworkValidationReport()

        # Run all checks
        report.add_check(self.check_vpn_interface())
        report.add_check(self.check_local_node_config())
        report.add_check(self.check_manifest_validity())
        report.add_check(self.check_peer_connectivity())
        report.add_check(self.check_introducer_reachability())

        return report

    def check_vpn_interface(self) -> ValidationResult:
        """Check if the VPN interface is up and configured."""
        result = run_command(f"ip link show {self.vpn_interface}", check=False)

        if not result.success:
            return ValidationResult(
                name="vpn_interface",
                passed=False,
                message=f"VPN interface {self.vpn_interface} does not exist",
            )

        if "UP" not in result.stdout:
            return ValidationResult(
                name="vpn_interface",
                passed=False,
                message=f"VPN interface {self.vpn_interface} is not up",
            )

        # Get IP address
        result = run_command(f"ip addr show {self.vpn_interface}", check=False)
        ip_configured = "inet " in result.stdout

        return ValidationResult(
            name="vpn_interface",
            passed=ip_configured,
            message="VPN interface is up and configured"
            if ip_configured
            else "VPN interface has no IP",
            details={"interface": self.vpn_interface},
        )

    def check_local_node_config(self) -> ValidationResult:
        """Check if the local node is properly configured in the manifest."""
        node = self.manifest.get_node(self.local_node_name)

        if not node:
            return ValidationResult(
                name="local_node_config",
                passed=False,
                message=f"Local node {self.local_node_name} not found in manifest",
            )

        issues = []

        if not node.vpn_ip and not node.internal_ip:
            issues.append("No VPN IP configured")

        if not node.roles:
            issues.append("No roles assigned")

        if issues:
            return ValidationResult(
                name="local_node_config",
                passed=False,
                message=f"Local node configuration issues: {', '.join(issues)}",
                details={"node_name": self.local_node_name, "issues": issues},
            )

        return ValidationResult(
            name="local_node_config",
            passed=True,
            message="Local node properly configured",
            details={"node_name": self.local_node_name, "roles": [r.value for r in node.roles]},
        )

    def check_manifest_validity(self) -> ValidationResult:
        """Check if the manifest is valid."""
        errors = self.manifest.validate()

        if errors:
            return ValidationResult(
                name="manifest_validity",
                passed=False,
                message=f"Manifest has {len(errors)} validation error(s)",
                details={"errors": errors},
            )

        return ValidationResult(
            name="manifest_validity",
            passed=True,
            message="Manifest is valid",
            details={"node_count": len(self.manifest.nodes)},
        )

    def check_peer_connectivity(self) -> ValidationResult:
        """Check connectivity to peer nodes."""
        peers = [n for n in self.manifest.nodes if n.name != self.local_node_name]

        if not peers:
            return ValidationResult(
                name="peer_connectivity",
                passed=True,
                message="No peers to check",
            )

        reachable = 0
        unreachable = []

        for peer in peers:
            ip = peer.vpn_ip or peer.internal_ip
            result = run_command(f"ping -c 1 -W 2 {ip}", check=False)
            if result.success:
                reachable += 1
            else:
                unreachable.append(peer.name)

        if len(unreachable) == len(peers):
            return ValidationResult(
                name="peer_connectivity",
                passed=False,
                message="Cannot reach any peers",
                details={"unreachable": unreachable},
            )

        if unreachable:
            return ValidationResult(
                name="peer_connectivity",
                passed=True,  # Some peers reachable is OK
                message=f"Reached {reachable}/{len(peers)} peers",
                details={"reachable": reachable, "unreachable": unreachable},
            )

        return ValidationResult(
            name="peer_connectivity",
            passed=True,
            message=f"All {reachable} peers reachable",
            details={"reachable": reachable},
        )

    def check_introducer_reachability(self) -> ValidationResult:
        """Check if the introducer is reachable."""
        introducers = self.manifest.get_introducers()

        if not introducers:
            return ValidationResult(
                name="introducer_reachability",
                passed=False,
                message="No introducer configured in manifest",
            )

        # Try to reach the first introducer
        introducer = introducers[0]
        ip = introducer.vpn_ip or introducer.internal_ip
        port = introducer.ports.tahoe_introducer

        # Check if port is open
        result = run_command(f"nc -z -w 5 {ip} {port}", check=False)

        if result.success:
            return ValidationResult(
                name="introducer_reachability",
                passed=True,
                message=f"Introducer {introducer.name} is reachable",
                details={"introducer": introducer.name, "address": f"{ip}:{port}"},
            )

        # Port not open, but maybe node is reachable
        ping_result = run_command(f"ping -c 1 -W 2 {ip}", check=False)

        if ping_result.success:
            return ValidationResult(
                name="introducer_reachability",
                passed=False,
                message=f"Introducer {introducer.name} reachable but port {port} closed",
                details={"introducer": introducer.name, "address": f"{ip}:{port}"},
            )

        return ValidationResult(
            name="introducer_reachability",
            passed=False,
            message=f"Introducer {introducer.name} is not reachable",
            details={"introducer": introducer.name, "address": f"{ip}:{port}"},
        )

    def wait_for_vpn(self, timeout: float = 60.0, interval: float = 5.0) -> bool:
        """Wait for the VPN interface to come up.

        Args:
            timeout: Maximum wait time in seconds
            interval: Check interval in seconds

        Returns:
            True if VPN came up within timeout
        """
        import time

        start = time.time()

        while time.time() - start < timeout:
            result = self.check_vpn_interface()
            if result.passed:
                return True
            time.sleep(interval)

        return False
