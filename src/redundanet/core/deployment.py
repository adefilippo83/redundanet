"""Drive the RedundaNet Docker Compose deployment from the CLI.

The CLI runs on the host and manages the containerized stack (tinc + tahoe
services) that ``redundanet network join`` sets up. This module is a thin,
typed wrapper around ``docker compose`` built on :func:`run_command`.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Any

from redundanet.utils.logging import get_logger
from redundanet.utils.process import CommandResult, is_command_available, run_command

if TYPE_CHECKING:
    from redundanet.core.config import AppSettings

logger = get_logger(__name__)


class DeploymentError(Exception):
    """Raised when the Docker Compose deployment cannot be used."""


@dataclass
class ServiceStatus:
    """Status of a single compose service."""

    name: str
    state: str  # e.g. "running", "exited"
    health: str  # e.g. "healthy", "starting", or "" when no healthcheck


class Deployment:
    """Locate and drive the RedundaNet docker-compose deployment."""

    def __init__(self, settings: AppSettings) -> None:
        self.settings = settings
        self.project = settings.compose_project
        self.compose_file = self._locate_compose_file()
        self.env_file = self._locate_env_file()

    def _locate_compose_file(self) -> Path | None:
        if self.settings.compose_file is not None:
            return self.settings.compose_file if self.settings.compose_file.exists() else None
        candidates = [
            Path("docker/docker-compose.yml"),
            self.settings.data_dir / "repo" / "docker" / "docker-compose.yml",
            Path("/opt/redundanet/docker/docker-compose.yml"),
        ]
        for candidate in candidates:
            if candidate.exists():
                return candidate
        return None

    def _locate_env_file(self) -> Path | None:
        if self.settings.compose_env_file is not None and self.settings.compose_env_file.exists():
            return self.settings.compose_env_file
        default = Path("/opt/redundanet/.env")
        return default if default.exists() else None

    def is_configured(self) -> bool:
        """Whether a compose file was found."""
        return self.compose_file is not None

    def require(self) -> None:
        """Raise :class:`DeploymentError` if the deployment cannot be driven."""
        if not is_command_available("docker"):
            raise DeploymentError("Docker is not installed or not on PATH.")
        if self.compose_file is None:
            raise DeploymentError(
                "No docker-compose.yml found. Run 'redundanet network join' first, "
                "or set REDUNDANET_COMPOSE_FILE."
            )

    def _base(self) -> list[str]:
        cmd = ["docker", "compose", "-p", self.project, "-f", str(self.compose_file)]
        if self.env_file is not None:
            cmd += ["--env-file", str(self.env_file)]
        return cmd

    def compose(
        self,
        *args: str,
        input_text: str | None = None,
        capture: bool = True,
        timeout: float | None = 120,
    ) -> CommandResult:
        """Run a ``docker compose`` subcommand against this deployment."""
        return run_command(
            self._base() + list(args),
            input_text=input_text,
            capture_output=capture,
            timeout=timeout,
        )

    def ps(self) -> list[ServiceStatus]:
        """Return the status of every service (running or not)."""
        result = self.compose("ps", "--all", "--format", "json")
        statuses: list[ServiceStatus] = []
        if not result.success:
            return statuses
        for line in result.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                parsed: Any = json.loads(line)
            except json.JSONDecodeError:
                continue
            entries = parsed if isinstance(parsed, list) else [parsed]
            for entry in entries:
                statuses.append(
                    ServiceStatus(
                        name=str(entry.get("Service") or entry.get("Name") or ""),
                        state=str(entry.get("State") or ""),
                        health=str(entry.get("Health") or ""),
                    )
                )
        return statuses

    def service_status(self, service: str) -> ServiceStatus | None:
        """Return the status of a single service, or None if absent."""
        for status in self.ps():
            if status.name == service:
                return status
        return None

    def exec(
        self,
        service: str,
        args: list[str],
        input_text: str | None = None,
        capture: bool = True,
        timeout: float | None = 120,
    ) -> CommandResult:
        """Run a command inside a running service container."""
        return self.compose(
            "exec",
            "-T",
            service,
            *args,
            input_text=input_text,
            capture=capture,
            timeout=timeout,
        )

    def cp_in(self, service: str, local: Path, container_path: str) -> CommandResult:
        """Copy a local file into a service container."""
        return self.compose("cp", str(local), f"{service}:{container_path}")

    def cp_out(self, service: str, container_path: str, local: Path) -> CommandResult:
        """Copy a file out of a service container to the local filesystem."""
        return self.compose("cp", f"{service}:{container_path}", str(local))

    def up(self, services: list[str] | None = None) -> CommandResult:
        """Start (creating if needed) all or some services, detached."""
        return self.compose("up", "-d", *(services or []), timeout=300)

    def start(self, services: list[str]) -> CommandResult:
        """Start existing service containers."""
        return self.compose("start", *services)

    def stop(self, services: list[str]) -> CommandResult:
        """Stop service containers without removing them."""
        return self.compose("stop", *services)

    def down(self) -> CommandResult:
        """Stop and remove the whole deployment."""
        return self.compose("down")

    def logs(self, service: str, follow: bool = False, tail: int = 50) -> CommandResult:
        """Show (optionally follow) logs for a service."""
        args = ["logs", "--tail", str(tail)]
        if follow:
            args.append("--follow")
        args.append(service)
        return self.compose(*args, capture=not follow, timeout=None if follow else 60)


def git_sync(repo: str, branch: str, target_dir: Path) -> CommandResult:
    """Clone or hard-reset a manifest git repository into ``target_dir``."""
    if (target_dir / ".git").exists():
        run_command(["git", "-C", str(target_dir), "fetch", "origin"], check=False)
        return run_command(
            ["git", "-C", str(target_dir), "reset", "--hard", f"origin/{branch}"],
            check=False,
        )
    target_dir.mkdir(parents=True, exist_ok=True)
    return run_command(["git", "clone", "-b", branch, repo, str(target_dir)], check=False)
