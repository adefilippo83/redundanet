"""Main CLI application for RedundaNet."""

from __future__ import annotations

import shutil
from pathlib import Path
from typing import Annotated

import typer
from rich import print as rprint
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

from redundanet import __version__
from redundanet.cli.network import app as network_app
from redundanet.cli.node import app as node_app
from redundanet.cli.storage import app as storage_app
from redundanet.core.config import AppSettings, load_settings
from redundanet.core.deployment import (
    Deployment,
    compose_files_differ,
    git_sync,
    read_env_file,
    sync_compose_files,
)
from redundanet.core.manifest import Manifest
from redundanet.utils.logging import setup_logging

# Create the main app
app = typer.Typer(
    name="redundanet",
    help="RedundaNet - Distributed encrypted storage on a mesh VPN network",
    no_args_is_help=True,
    rich_markup_mode="rich",
)

# Add subcommands
app.add_typer(node_app, name="node", help="Node management commands")
app.add_typer(network_app, name="network", help="Network management commands")
app.add_typer(storage_app, name="storage", help="Storage management commands")

console = Console()


def version_callback(value: bool) -> None:
    """Print version and exit."""
    if value:
        rprint(f"[bold blue]RedundaNet[/bold blue] version [green]{__version__}[/green]")
        raise typer.Exit()


@app.callback()
def main(
    version: Annotated[
        bool | None,
        typer.Option(
            "--version",
            "-v",
            callback=version_callback,
            is_eager=True,
            help="Show version and exit",
        ),
    ] = None,
    debug: Annotated[
        bool,
        typer.Option("--debug", "-d", help="Enable debug logging"),
    ] = False,
    config_dir: Annotated[
        Path | None,
        typer.Option("--config-dir", "-c", help="Configuration directory"),
    ] = None,
) -> None:
    """RedundaNet - Distributed encrypted storage on a mesh VPN network."""
    log_level = "DEBUG" if debug else "INFO"
    setup_logging(level=log_level)


@app.command()
def init(
    node_name: Annotated[
        str,
        typer.Option("--name", "-n", prompt="Enter node name", help="Name for this node"),
    ],
    network_name: Annotated[
        str,
        typer.Option(
            "--network",
            prompt="Enter network name",
            help="Name of the network to join or create",
        ),
    ] = "redundanet",
    storage_contribution: Annotated[
        str,
        typer.Option(
            "--storage",
            prompt="Storage contribution (e.g., 1TB)",
            help="Amount of storage to contribute",
        ),
    ] = "1TB",
    manifest_repo: Annotated[
        str | None,
        typer.Option(
            "--manifest-repo",
            prompt="Manifest repository URL (or press Enter to skip)",
            help="Git repository URL for the network manifest",
        ),
    ] = None,
    docker: Annotated[
        bool,
        typer.Option("--docker", help="Initialize for Docker deployment"),
    ] = True,
) -> None:
    """Initialize a new RedundaNet node with interactive setup."""
    console.print(
        Panel(
            "[bold blue]Welcome to RedundaNet![/bold blue]\n\n"
            "This wizard will help you set up a new node on the distributed storage network.",
            title="RedundaNet Setup",
        )
    )

    with console.status("[bold green]Initializing node..."):
        # Create configuration directories. The defaults (/etc/redundanet,
        # /var/lib/redundanet) need root; on a workstation fall back to
        # per-user directories instead of crashing. The persisted .env then
        # records the data dir so every later command agrees on the location.
        settings = load_settings()
        config_dir = settings.config_dir
        data_dir = settings.data_dir

        fell_back = False
        try:
            config_dir.mkdir(parents=True, exist_ok=True)
            data_dir.mkdir(parents=True, exist_ok=True)
        except OSError:
            from redundanet.core.config import user_config_dir, user_data_dir

            config_dir = user_config_dir()
            data_dir = user_data_dir()
            config_dir.mkdir(parents=True, exist_ok=True)
            data_dir.mkdir(parents=True, exist_ok=True)
            fell_back = True
        for sub in ("manifest", "tinc", "tahoe"):
            (data_dir / sub).mkdir(exist_ok=True)

        # Persist the configuration so later commands (sync, status, network)
        # can read it via load_settings() without re-passing flags.
        config_env = config_dir / ".env"
        env_lines = [f"REDUNDANET_NODE_NAME={node_name}"]
        if manifest_repo:
            env_lines.append(f"REDUNDANET_MANIFEST_REPO={manifest_repo}")
        if fell_back:
            env_lines.append(f"REDUNDANET_DATA_DIR={data_dir}")
        try:
            config_env.write_text("\n".join(env_lines) + "\n")
            config_saved = True
        except OSError:
            config_saved = False

        if fell_back:
            console.print(
                f"[yellow]Note:[/yellow] {settings.config_dir} is not writable "
                "(no root); using per-user directories instead."
            )
        console.print(f"[green]Created configuration directory:[/green] {config_dir}")
        console.print(f"[green]Created data directory:[/green] {data_dir}")
        if config_saved:
            console.print(f"[green]Saved configuration:[/green] {config_env}")
        else:
            console.print(f"[yellow]Could not save configuration to:[/yellow] {config_env}")

    # Generate node configuration
    console.print("\n[bold]Node Configuration:[/bold]")
    table = Table(show_header=False, box=None)
    table.add_column("Property", style="cyan")
    table.add_column("Value", style="green")
    table.add_row("Node Name", node_name)
    table.add_row("Network", network_name)
    table.add_row("Storage Contribution", storage_contribution)
    table.add_row("Deployment Mode", "Docker" if docker else "Native")
    console.print(table)

    console.print("\n[bold green]Node initialized successfully![/bold green]")
    console.print("\n[bold]Next steps:[/bold]")
    console.print("1. Generate GPG keys: [cyan]redundanet node keys generate[/cyan]")
    console.print("2. Join the network:  [cyan]redundanet network join[/cyan]")
    # 'join' derives this node's compose profile from its manifest roles and
    # prints the exact 'docker compose ... up -d' to run — so we don't print a
    # role-agnostic (and here, wrong) command at init time.
    if docker:
        console.print("3. Start services:    the 'join' command prints the exact start command")


@app.command()
def status(
    verbose: Annotated[
        bool,
        typer.Option("--verbose", "-v", help="Show detailed status"),
    ] = False,
) -> None:
    """Show the current status of the local node and network."""
    console.print(Panel("[bold]RedundaNet Status[/bold]", expand=False))

    # Node status table
    table = Table(title="Local Node", show_header=True)
    table.add_column("Property", style="cyan")
    table.add_column("Value")

    settings = load_settings()

    table.add_row("Node Name", settings.node_name or "[dim]Not configured[/dim]")
    table.add_row("Config Dir", str(settings.config_dir))
    table.add_row("Data Dir", str(settings.data_dir))
    table.add_row("Debug Mode", "Yes" if settings.debug else "No")

    console.print(table)

    # Service status from the docker-compose deployment
    console.print("\n[bold]Service Status:[/bold]")
    deployment = Deployment(settings)

    if not deployment.is_configured():
        console.print(
            "[dim]No deployment found "
            "(run 'redundanet network join' or set REDUNDANET_COMPOSE_FILE).[/dim]"
        )
        return

    statuses = {s.name: s for s in deployment.ps()}
    display = [
        ("Tinc VPN", settings.tinc_service),
        ("Tahoe Introducer", settings.introducer_service),
        ("Tahoe Storage", settings.storage_service),
        ("Tahoe Client", settings.client_service),
    ]

    status_table = Table(show_header=True)
    status_table.add_column("Service")
    status_table.add_column("State")
    status_table.add_column("Health")

    for label, svc in display:
        svc_status = statuses.get(svc)
        if svc_status is None:
            status_table.add_row(label, "[dim]not created[/dim]", "")
            continue
        state = (
            f"[green]{svc_status.state}[/green]"
            if svc_status.state == "running"
            else f"[yellow]{svc_status.state}[/yellow]"
        )
        health = svc_status.health or "—"
        health_disp = f"[green]{health}[/green]" if health == "healthy" else f"[dim]{health}[/dim]"
        status_table.add_row(label, state, health_disp)

    console.print(status_table)

    # VPN interface (best-effort, only when tinc is running)
    tinc_status = statuses.get(settings.tinc_service)
    if tinc_status is not None and tinc_status.state == "running":
        result = deployment.exec(
            settings.tinc_service, ["ip", "-o", "-4", "addr", "show", "redundanet"]
        )
        if result.success and result.stdout.strip():
            ip = next(
                (p.split("/")[0] for p in result.stdout.split() if "/" in p and p[0].isdigit()),
                "",
            )
            console.print(
                f"\n[bold]VPN:[/bold] interface [cyan]redundanet[/cyan] up, "
                f"IP [green]{ip or 'unknown'}[/green]"
            )
        else:
            console.print("\n[bold]VPN:[/bold] [yellow]interface not up yet[/yellow]")


def _refresh_compose_file(
    settings: AppSettings, deployment: Deployment, *, check: bool
) -> tuple[bool, bytes | None]:
    """Refresh the node's ``docker-compose.yml`` from the manifest repo clone.

    ``redundanet update`` pulls new *images*, but the compose file that maps
    settings and mounts into those images is installed once by ``network
    join`` and otherwise never refreshed, so a node silently misses compose
    changes shipped in a release (new env passthrough, new sidecars). This
    pulls the clone that ``join`` maintains and copies its compose file into
    the install directory, leaving operator-owned files (the override, secrets,
    the FUSE mount dir) untouched.

    Returns ``(changed, previous_bytes)``. ``previous_bytes`` is the
    pre-refresh content of the compose file so a failed update can restore it;
    it is ``None`` under ``--check`` (nothing is written) and when nothing
    changed.
    """
    compose_path = deployment.compose_file
    if compose_path is None:
        return (False, None)
    repo_dir = settings.data_dir / "repo"
    # Skip when the located compose file *is* the repo clone (dev/CI checkouts),
    # or when there is no clone to refresh from.
    if repo_dir in compose_path.parents or not (repo_dir / ".git").exists():
        return (False, None)

    install_docker = compose_path.parent
    repo_docker = repo_dir / "docker"

    # Pull the clone to the branch this node was joined with, when we know it.
    env_vals = read_env_file(deployment.env_file) if deployment.env_file else {}
    repo_url = env_vals.get("MANIFEST_REPO") or settings.manifest_repo
    branch = env_vals.get("MANIFEST_BRANCH") or settings.manifest_branch
    if repo_url:
        with console.status("[bold green]Checking the manifest repo for compose changes..."):
            synced = git_sync(repo_url, branch, repo_dir)
        if not synced.success:
            console.print(
                "[yellow]Could not update the repo clone; comparing against the "
                "local copy.[/yellow]"
            )

    if not (repo_docker / "docker-compose.yml").exists():
        return (False, None)

    if check:
        return (compose_files_differ(repo_docker, install_docker), None)

    previous = compose_path.read_bytes() if compose_path.exists() else None
    changed = sync_compose_files(repo_docker, install_docker)
    return (changed, previous if changed else None)


@app.command()
def update(
    check: Annotated[
        bool,
        typer.Option(
            "--check", help="Only report whether new images are available; change nothing"
        ),
    ] = False,
    yes: Annotated[
        bool,
        typer.Option("--yes", "-y", help="Recreate without confirmation"),
    ] = False,
    health_timeout: Annotated[
        int,
        typer.Option(
            "--health-timeout",
            help="Seconds to wait for the node to become healthy after recreate "
            "before rolling back.",
        ),
    ] = 120,
    no_rollback: Annotated[
        bool,
        typer.Option(
            "--no-rollback",
            help="Do not roll back to the previous images if the node is "
            "unhealthy after the update (just report).",
        ),
    ] = False,
    no_compose_refresh: Annotated[
        bool,
        typer.Option(
            "--no-compose-refresh",
            help="Do not refresh docker-compose.yml from the manifest repo; update images only.",
        ),
    ] = False,
) -> None:
    """Pull the latest container images and recreate the node's services.

    Also refreshes the node's docker-compose.yml from the manifest repo (unless
    --no-compose-refresh), so compose changes shipped in a release (new setting
    passthrough, new sidecars) actually reach the node; a pull alone never
    updates the compose file. The operator's override file, secrets and mount
    directory are left untouched.

    Netns-aware: the tahoe services share the tinc container's network
    namespace, so this force-recreates all running services together (tinc
    first) — a plain restart would strand them on the old namespace.

    After recreating, the node's health is checked; if it does not recover
    within --health-timeout, the previous images (and, if it changed, the
    previous compose file) are restored (unless --no-rollback), so a bad
    :latest push or compose change cannot leave the node down.
    """
    settings = load_settings()
    deployment = Deployment(settings)
    try:
        deployment.require()
    except Exception as e:  # DeploymentError and friends
        console.print(f"[red]Error:[/red] {e}")
        raise typer.Exit(1) from None

    services = deployment.running_services()
    if not services:
        console.print("[yellow]No running services found.[/yellow] Start the node first.")
        raise typer.Exit(1)

    # Refresh the compose file before pulling, so image detection and recreate
    # both see the current file. Under --check nothing is written.
    compose_changed = False
    previous_compose: bytes | None = None
    if not no_compose_refresh:
        compose_changed, previous_compose = _refresh_compose_file(settings, deployment, check=check)

    with console.status("[bold green]Pulling latest images..."):
        pull = deployment.pull(services)
    if not pull.success:
        console.print(f"[red]Pull failed:[/red] {pull.stderr.strip() or pull.stdout.strip()}")
        raise typer.Exit(1)

    # A pull updates the local repo:tag but NOT the running container's image,
    # so compare each container's image against what its tag now resolves to.
    changed = deployment.pending_image_changes(services)
    if not changed and not compose_changed:
        console.print("[green]Already up to date.[/green] No image or compose change.")
        return

    if changed:
        console.print("[bold]Updated images available for:[/bold] " + ", ".join(changed))
    if compose_changed:
        verb = "available" if check else "applied"
        console.print(f"[bold]Compose file update {verb}[/bold] (from the manifest repo).")
    if check:
        console.print("[dim]--check: not recreating.[/dim]")
        raise typer.Exit(0)

    if not yes and not typer.confirm(
        "Recreate these services now? (brief downtime; VPN reconverges after)"
    ):
        console.print("Aborted. Run without --check to apply later.")
        raise typer.Exit(0)

    # Capture the working images BEFORE recreating, so a bad new image can be
    # rolled back to exactly what was running.
    rollback_images = deployment.current_images(changed)

    with console.status("[bold green]Recreating services (tinc first)..."):
        result = deployment.recreate(services)
    if not result.success:
        console.print(f"[red]Recreate failed:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)

    with console.status("[bold green]Verifying node health..."):
        healthy = deployment.wait_healthy(services, timeout=health_timeout)
    if healthy:
        console.print("[green]Updated.[/green] Services recreated from the new images and healthy.")
        console.print(
            "[dim]The VPN mesh takes ~a minute to reconverge before the client "
            "reconnects to the grid.[/dim]"
        )
        return

    # The node did not recover on the new images/compose.
    console.print(
        f"[red]Node did not become healthy within {health_timeout}s after the update.[/red]"
    )
    if no_rollback or (not rollback_images and previous_compose is None):
        console.print(
            "[yellow]Left on the new images (--no-rollback or nothing to roll "
            "back to).[/yellow] Investigate with [cyan]redundanet status[/cyan]."
        )
        raise typer.Exit(1)

    # Restore the previous compose file (if we changed it) before recreating, so
    # a bad compose change is rolled back as cleanly as a bad image.
    if previous_compose is not None and deployment.compose_file is not None:
        deployment.compose_file.write_bytes(previous_compose)
        console.print("[dim]Restored the previous compose file.[/dim]")

    console.print("[bold]Rolling back to the previous images...[/bold]")
    restored = deployment.rollback(rollback_images, services)
    if not restored.success:
        console.print(f"[red]Rollback recreate failed:[/red] {restored.stderr.strip()}")
        raise typer.Exit(1)

    with console.status("[bold green]Verifying health after rollback..."):
        healthy_again = deployment.wait_healthy(services, timeout=health_timeout)
    if healthy_again:
        console.print(
            "[yellow]Rolled back to the previous images; the node is healthy "
            "again.[/yellow] The new images appear broken — not applied."
        )
    else:
        console.print(
            "[red]Rollback did not restore health.[/red] Manual intervention "
            "needed: check [cyan]redundanet status[/cyan] and the container logs."
        )
    raise typer.Exit(1)


@app.command()
def sync(
    force: Annotated[
        bool,
        typer.Option("--force", "-f", help="Force sync even if up to date"),
    ] = False,
) -> None:
    """Sync the network manifest from the repository."""
    settings = load_settings()

    if not settings.manifest_repo:
        console.print("[red]Error:[/red] No manifest repository configured.")
        console.print("Set REDUNDANET_MANIFEST_REPO environment variable or run init.")
        raise typer.Exit(1)

    repo_dir = settings.data_dir / "repo"
    manifest_dir = settings.data_dir / "manifest"

    with console.status("[bold green]Syncing manifest..."):
        result = git_sync(settings.manifest_repo, settings.manifest_branch, repo_dir)

    if not result.success:
        detail = result.stderr.strip() or result.stdout.strip() or "unknown error"
        console.print(f"[red]Failed to sync manifest:[/red] {detail}")
        raise typer.Exit(1)

    # The manifest lives under the repo's manifests/ directory; copy it into the
    # manifest dir where the rest of the CLI (node list/info, network) looks.
    manifest_dir.mkdir(parents=True, exist_ok=True)
    src = repo_dir / "manifests"
    if src.is_dir():
        for f in sorted([*src.glob("*.yaml"), *src.glob("*.json")]):
            shutil.copy(f, manifest_dir / f.name)

    console.print(
        f"[green]Synced[/green] {settings.manifest_repo} "
        f"([cyan]{settings.manifest_branch}[/cyan]) -> {manifest_dir}"
    )

    manifest_file = manifest_dir / settings.manifest_filename
    if manifest_file.exists():
        try:
            manifest = Manifest.from_file(manifest_file)
            console.print(f"[bold]Nodes in manifest:[/bold] {len(manifest.nodes)}")
        except Exception as e:  # summary is best-effort
            console.print(f"[yellow]Manifest synced but could not be parsed:[/yellow] {e}")
    else:
        console.print(
            f"[yellow]Note:[/yellow] {settings.manifest_filename} not found in the "
            "repo's manifests/ directory."
        )


@app.command("validate")
def validate_manifest(
    manifest_path: Annotated[
        Path,
        typer.Argument(help="Path to the manifest file"),
    ],
) -> None:
    """Validate a network manifest file.

    Exits non-zero if the manifest has blocking errors (so CI and operators can
    gate on it); advisory warnings alone do not fail the check.
    """
    try:
        manifest = Manifest.from_file(manifest_path)
    except Exception as e:
        console.print(f"[red]Validation failed:[/red] {e}")
        raise typer.Exit(1) from None

    result = manifest.validate_detailed()

    if result.errors:
        console.print("[red]Validation errors:[/red]")
        for error in result.errors:
            console.print(f"  [red]✗[/red] {error}")
    if result.warnings:
        console.print("[yellow]Validation warnings:[/yellow]")
        for warning in result.warnings:
            console.print(f"  [yellow]![/yellow] {warning}")
    if not result.errors and not result.warnings:
        console.print("[green]Manifest is valid![/green]")

    # Print summary
    console.print(f"\n[bold]Network:[/bold] {manifest.network.name}")
    console.print(f"[bold]Nodes:[/bold] {len(manifest.nodes)}")
    console.print(f"[bold]Introducers:[/bold] {len(manifest.get_introducers())}")
    console.print(f"[bold]Storage Nodes:[/bold] {len(manifest.get_storage_nodes())}")

    if result.errors:
        raise typer.Exit(1)


if __name__ == "__main__":
    app()
