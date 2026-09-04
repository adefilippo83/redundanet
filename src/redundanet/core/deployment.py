"""Drive the RedundaNet Docker Compose deployment from the CLI.

The CLI runs on the host and manages the containerized stack (tinc + tahoe
services) that ``redundanet network join`` sets up. This module is a thin,
typed wrapper around ``docker compose`` built on :func:`run_command`.
"""

from __future__ import annotations

import json
import time
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

    def _override_files(self) -> list[Path]:
        """Compose override files next to the main compose file.

        Passing any ``-f`` disables Docker's automatic loading of the
        override, so we must add it back explicitly — otherwise a CLI-driven
        recreate (e.g. `redundanet update`) silently drops the override's
        settings, such as a storage node's bind-mount to its data disk, and
        falls back to the empty named volume.
        """
        if self.compose_file is None:
            return []
        parent = self.compose_file.parent
        names = (
            "docker-compose.override.yml",
            "docker-compose.override.yaml",
            "compose.override.yml",
            "compose.override.yaml",
        )
        return [parent / name for name in names if (parent / name).exists()]

    def _base(self) -> list[str]:
        cmd = ["docker", "compose", "-p", self.project, "-f", str(self.compose_file)]
        for override in self._override_files():
            cmd += ["-f", str(override)]
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

    def cp_in(
        self, service: str, local: Path, container_path: str, timeout: float | None = 120
    ) -> CommandResult:
        """Copy a local file into a service container."""
        return self.compose("cp", str(local), f"{service}:{container_path}", timeout=timeout)

    def cp_out(
        self, service: str, container_path: str, local: Path, timeout: float | None = 120
    ) -> CommandResult:
        """Copy a file out of a service container to the local filesystem."""
        return self.compose("cp", f"{service}:{container_path}", str(local), timeout=timeout)

    def up(self, services: list[str] | None = None) -> CommandResult:
        """Start (creating if needed) all or some services, detached."""
        return self.compose("up", "-d", *(services or []), timeout=300)

    def running_services(self) -> list[str]:
        """Names of the services whose containers currently exist (any state)."""
        return [s.name for s in self.ps() if s.name]

    def _service_containers(self) -> dict[str, str]:
        """Map each service to its container name, from `docker compose ps`.

        Deliberately NOT `docker compose images` — that returns empty on some
        compose versions (observed on v5.4), which silently made update think
        nothing changed. `ps` is reliable and gives us container names to
        inspect directly.
        """
        result = self.compose("ps", "--all", "--format", "json")
        names: dict[str, str] = {}
        for line in result.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                parsed: Any = json.loads(line)
            except json.JSONDecodeError:
                continue
            for entry in parsed if isinstance(parsed, list) else [parsed]:
                service = str(entry.get("Service") or "")
                name = str(entry.get("Name") or "")
                if service and name:
                    names[service] = name
        return names

    def _inspect(self, target: str, fmt: str) -> str:
        """`docker inspect -f <fmt> <target>`, stripped, or '' on failure."""
        result = run_command(["docker", "inspect", "-f", fmt, target], check=False)
        return result.stdout.strip() if result.success else ""

    def local_image_id(self, ref: str) -> str:
        """The image ID that ``ref`` (repository:tag) resolves to locally, or ''."""
        return self._inspect(ref, "{{.Id}}")

    def pending_image_changes(self, services: list[str]) -> list[str]:
        """Services whose running container's image differs from its tag's.

        Call after pull(): for each service compares the container's actual
        image (``docker inspect .Image``) against what the reference it was
        created from (``.Config.Image``) now resolves to. A pull updates the
        tag but not the running container, so a difference means a recreate is
        needed.
        """
        containers = self._service_containers()
        changed: list[str] = []
        for service in services:
            container = containers.get(service)
            if not container:
                continue
            current = self._inspect(container, "{{.Image}}")
            ref = self._inspect(container, "{{.Config.Image}}")
            if not ref or not current:
                continue
            latest = self.local_image_id(ref)
            if latest and current != latest:
                changed.append(service)
        return sorted(changed)

    def pull(self, services: list[str] | None = None) -> CommandResult:
        """Pull the latest images for all or some services."""
        return self.compose("pull", *(services or []), timeout=1800)

    def recreate(self, services: list[str]) -> CommandResult:
        """Force-recreate the named services from their current images.

        Naming the services explicitly auto-enables their compose profiles, and
        depends_on ordering means tinc is recreated before the tahoe services —
        so those rejoin tinc's *new* network namespace instead of the dead one
        (see docs/quickstart.md: '0 shares after an update').
        """
        return self.compose("up", "-d", "--force-recreate", *services, timeout=600)

    def current_images(self, services: list[str]) -> dict[str, tuple[str, str]]:
        """For each service, ``(image reference, current image id)`` of its
        running container. Captured *before* an update so a bad recreate can be
        rolled back to the exact image that was working.
        """
        containers = self._service_containers()
        images: dict[str, tuple[str, str]] = {}
        for service in services:
            container = containers.get(service)
            if not container:
                continue
            ref = self._inspect(container, "{{.Config.Image}}")
            image_id = self._inspect(container, "{{.Image}}")
            if ref and image_id:
                images[service] = (ref, image_id)
        return images

    def retag(self, image_id: str, ref: str) -> CommandResult:
        """Point an image reference (``repo:tag``) back at a specific image id."""
        return run_command(["docker", "tag", image_id, ref], check=False)

    @staticmethod
    def _statuses_healthy(statuses: list[ServiceStatus], services: list[str]) -> bool:
        """Whether the given services look healthy after a recreate.

        Every named service must be ``running`` and not ``unhealthy``; tinc must
        additionally be affirmatively ``healthy`` because it anchors the shared
        network namespace and the VPN the tahoe services ride on. The tahoe
        services are only required to be up (not crash-looping) — they may still
        be reconnecting to the grid, which is not a failure of the update.
        """
        by_name = {s.name: s for s in statuses}
        for service in services:
            status = by_name.get(service)
            if status is None or status.state != "running":
                return False
            if status.health == "unhealthy":
                return False
        # tinc must be affirmatively healthy when it reports a healthcheck.
        tinc = by_name.get("tinc")
        return tinc is None or not tinc.health or tinc.health == "healthy"

    def wait_healthy(
        self, services: list[str], timeout: float = 120.0, interval: float = 5.0
    ) -> bool:
        """Poll ``ps`` until the services are healthy, or ``timeout`` elapses."""
        deadline = time.monotonic() + timeout
        while True:
            if self._statuses_healthy(self.ps(), services):
                return True
            if time.monotonic() >= deadline:
                return False
            time.sleep(interval)

    def rollback(self, images: dict[str, tuple[str, str]], services: list[str]) -> CommandResult:
        """Re-point each captured image reference back to its previous id and
        force-recreate the services (tinc first) so the node returns to the
        exact images it was running before the update.
        """
        for _service, (ref, image_id) in images.items():
            self.retag(image_id, ref)
        return self.recreate(services)

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
    """Sync a manifest git repository into ``target_dir`` (init/fetch/reset).

    Deliberately avoids ``git clone``: the target may already contain
    unrelated files (e.g. the shared manifest volume, where the introducer
    FURL is published by a concurrent process), and clone refuses non-empty
    directories — a race that left a live hub unable to sync at all.
    Initializing in place and hard-resetting to the fetched branch works for
    empty, non-empty, and already-cloned directories alike; untracked files
    are left alone.
    """
    git = ["git", "-C", str(target_dir)]
    if not (target_dir / ".git").exists():
        target_dir.mkdir(parents=True, exist_ok=True)
        result = run_command([*git, "init", "-q"], check=False)
        if not result.success:
            return result
    # add-or-update the remote (idempotent across both paths)
    run_command([*git, "remote", "add", "origin", repo], check=False)
    run_command([*git, "remote", "set-url", "origin", repo], check=False)
    result = run_command([*git, "fetch", "-q", "origin", branch], check=False)
    if not result.success:
        return result
    return run_command([*git, "reset", "--hard", "FETCH_HEAD"], check=False)


def read_env_file(path: Path) -> dict[str, str]:
    """Parse a simple ``KEY=VALUE`` ``.env`` file into a dict.

    Blank lines, comments and lines without ``=`` are ignored. Values are
    taken verbatim (no quote stripping); this is only used to recover a few
    plain keys (the manifest repo/branch a node was joined with), not to
    reproduce full shell ``.env`` semantics.
    """
    values: dict[str, str] = {}
    try:
        text = path.read_text()
    except OSError:
        return values
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, val = line.partition("=")
        values[key.strip()] = val.strip()
    return values


# Compose files the node reads at runtime. Only these are refreshed from the
# manifest repo; the ``.override.`` file is operator-owned (it carries the
# storage disk bind-mount) and is never overwritten from the repo.
_COMPOSE_GLOB = "docker-compose*.yml"


def compose_files_differ(repo_docker: Path, install_docker: Path) -> bool:
    """Whether the installed runtime ``docker-compose.yml`` differs from the
    repo clone's copy (a dry-run check that writes nothing)."""
    src = repo_docker / "docker-compose.yml"
    dst = install_docker / "docker-compose.yml"
    if not src.exists():
        return False
    if not dst.exists():
        return True
    return src.read_bytes() != dst.read_bytes()


def sync_compose_files(repo_docker: Path, install_docker: Path) -> bool:
    """Copy the repo clone's compose files into the install directory.

    Only files matching ``docker-compose*.yml`` are written, and any
    ``*.override.*`` file is skipped, so operator-owned artifacts (the
    override with the storage disk bind-mount, the ``secrets/`` directory, the
    root-owned FUSE ``mount/`` directory) are left untouched. Returns whether
    the runtime ``docker-compose.yml`` content actually changed.
    """
    runtime = install_docker / "docker-compose.yml"
    before = runtime.read_bytes() if runtime.exists() else None
    install_docker.mkdir(parents=True, exist_ok=True)
    for src in sorted(repo_docker.glob(_COMPOSE_GLOB)):
        if ".override." in src.name:
            continue
        (install_docker / src.name).write_bytes(src.read_bytes())
    after = runtime.read_bytes() if runtime.exists() else None
    return before != after
