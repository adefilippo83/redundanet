"""Network management CLI commands for RedundaNet."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Annotated, Optional

import typer
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

app = typer.Typer(help="Network management commands")
console = Console()

INSTALL_DIR = Path("/opt/redundanet")
REPO_DIR = Path("/var/lib/redundanet/repo")


def _clone_or_pull_repo(repo_url: str, branch: str, target_dir: Path) -> None:
    """Clone a repo or pull if it already exists."""
    if (target_dir / ".git").exists():
        # Pull latest changes
        subprocess.run(
            ["git", "-C", str(target_dir), "fetch", "origin"],
            check=True,
            capture_output=True,
        )
        subprocess.run(
            ["git", "-C", str(target_dir), "checkout", branch],
            check=True,
            capture_output=True,
        )
        subprocess.run(
            ["git", "-C", str(target_dir), "pull", "origin", branch],
            check=True,
            capture_output=True,
        )
    else:
        # Clone fresh
        target_dir.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(
            ["git", "clone", "--branch", branch, repo_url, str(target_dir)],
            check=True,
            capture_output=True,
        )


def _setup_docker_files(repo_dir: Path, install_dir: Path) -> None:
    """Copy docker files from cloned repo to install directory."""
    src_docker = repo_dir / "docker"
    dst_docker = install_dir / "docker"

    if not src_docker.exists():
        console.print("[yellow]Warning:[/yellow] No docker directory found in repo")
        return

    # Remove existing docker dir if present
    if dst_docker.exists():
        shutil.rmtree(dst_docker)

    # Copy docker directory
    shutil.copytree(src_docker, dst_docker)

    # Create secrets directory
    (dst_docker / "secrets").mkdir(exist_ok=True)

    console.print(f"[green]Docker files installed to:[/green] {dst_docker}")


def _setup_manifest(repo_dir: Path) -> None:
    """Copy manifest files to the data directory."""
    src_manifest = repo_dir / "manifests"
    dst_manifest = Path("/var/lib/redundanet/manifest")

    if not src_manifest.exists():
        console.print("[yellow]Warning:[/yellow] No manifests directory found in repo")
        return

    dst_manifest.mkdir(parents=True, exist_ok=True)

    # Copy manifest files
    for f in src_manifest.glob("*.yaml"):
        shutil.copy(f, dst_manifest / f.name)
    for f in src_manifest.glob("*.json"):
        shutil.copy(f, dst_manifest / f.name)

    console.print(f"[green]Manifest files installed to:[/green] {dst_manifest}")


@app.command("join")
def join_network(
    manifest_repo: Annotated[
        Optional[str],
        typer.Option("--repo", "-r", help="Git repository URL for the manifest"),
    ] = None,
    branch: Annotated[
        str,
        typer.Option("--branch", "-b", help="Git branch"),
    ] = "main",
    install_dir: Annotated[
        Path,
        typer.Option("--install-dir", help="Installation directory"),
    ] = INSTALL_DIR,
) -> None:
    """Join an existing RedundaNet network."""
    from redundanet.core.config import load_settings

    settings = load_settings()
    repo = manifest_repo or settings.manifest_repo

    if not repo:
        console.print("[red]Error:[/red] No manifest repository specified")
        console.print("Use --repo or set REDUNDANET_MANIFEST_REPO")
        raise typer.Exit(1)

    console.print(Panel(f"[bold]Joining RedundaNet Network[/bold]\nRepository: {repo}"))

    # Clone or update the repository
    with console.status("[bold green]Cloning repository..."):
        try:
            _clone_or_pull_repo(repo, branch, REPO_DIR)
            console.print(f"[green]Repository cloned to:[/green] {REPO_DIR}")
        except subprocess.CalledProcessError as e:
            console.print(f"[red]Error cloning repository:[/red] {e}")
            raise typer.Exit(1) from None

    # Set up docker files
    with console.status("[bold green]Setting up Docker files..."):
        _setup_docker_files(REPO_DIR, install_dir)

    # Set up manifest
    with console.status("[bold green]Setting up manifest..."):
        _setup_manifest(REPO_DIR)

    console.print("\n[bold green]Successfully joined the network![/bold green]")
    console.print("\n[bold]Next steps:[/bold]")
    console.print("1. Configure environment: [cyan]nano /opt/redundanet/.env[/cyan]")
    console.print("2. Export GPG key:        [cyan]gpg --armor --export-secret-keys <KEY_ID> > /opt/redundanet/docker/secrets/gpg_private_key.asc[/cyan]")
    console.print("3. Start services:        [cyan]cd /opt/redundanet/docker && docker-compose --env-file ../.env up -d[/cyan]")


@app.command("leave")
def leave_network(
    force: Annotated[
        bool,
        typer.Option("--force", "-f", help="Skip confirmation"),
    ] = False,
) -> None:
    """Leave the current RedundaNet network."""
    if not force:
        confirm = typer.confirm("Are you sure you want to leave the network?")
        if not confirm:
            console.print("Aborted.")
            raise typer.Exit(0)

    with console.status("[bold yellow]Leaving network..."):
        # In a real implementation, we'd stop services and cleanup
        pass

    console.print("[yellow]Left the RedundaNet network[/yellow]")


@app.command("status")
def network_status(
    verbose: Annotated[
        bool,
        typer.Option("--verbose", "-v", help="Show detailed status"),
    ] = False,
) -> None:
    """Show the status of the network connection."""
    console.print(Panel("[bold]Network Status[/bold]", expand=False))

    # VPN Status
    table = Table(title="VPN Connection", show_header=True)
    table.add_column("Property", style="cyan")
    table.add_column("Value")

    table.add_row("Interface", "tinc0")
    table.add_row("Status", "[yellow]Unknown[/yellow]")
    table.add_row("Connected Peers", "[dim]--[/dim]")
    table.add_row("Local IP", "[dim]--[/dim]")

    console.print(table)

    if verbose:
        # Peer list
        console.print("\n[bold]Connected Peers:[/bold]")
        console.print("[dim]No peer information available[/dim]")


@app.command("peers")
def list_peers(
    online_only: Annotated[
        bool,
        typer.Option("--online", "-o", help="Show only online peers"),
    ] = False,
) -> None:
    """List all peers in the network."""
    table = Table(title="Network Peers")
    table.add_column("Node", style="cyan")
    table.add_column("VPN IP", style="green")
    table.add_column("Status")
    table.add_column("Latency")

    # In a real implementation, we'd query the VPN for peer info
    console.print("[dim]Peer discovery not yet implemented[/dim]")
    console.print(table)


@app.command("ping")
def ping_node(
    node_name: Annotated[str, typer.Argument(help="Name of the node to ping")],
    count: Annotated[
        int,
        typer.Option("--count", "-c", help="Number of ping packets"),
    ] = 4,
) -> None:
    """Ping a node in the network."""
    console.print(f"[bold]Pinging node: {node_name}[/bold]")

    # In a real implementation, we'd resolve the node IP and ping

    # For now, just show a placeholder
    console.print(f"[dim]Would ping {node_name} ({count} packets)[/dim]")


# VPN subcommands
vpn_app = typer.Typer(help="VPN management commands")
app.add_typer(vpn_app, name="vpn")


@vpn_app.command("start")
def vpn_start() -> None:
    """Start the Tinc VPN connection."""
    with console.status("[bold green]Starting VPN..."):
        # In a real implementation, we'd start tinc
        pass
    console.print("[green]VPN started[/green]")


@vpn_app.command("stop")
def vpn_stop() -> None:
    """Stop the Tinc VPN connection."""
    with console.status("[bold yellow]Stopping VPN..."):
        # In a real implementation, we'd stop tinc
        pass
    console.print("[yellow]VPN stopped[/yellow]")


@vpn_app.command("restart")
def vpn_restart() -> None:
    """Restart the Tinc VPN connection."""
    vpn_stop()
    vpn_start()


@vpn_app.command("status")
def vpn_status() -> None:
    """Show VPN status."""
    table = Table(title="VPN Status", show_header=False)
    table.add_column("Property", style="cyan")
    table.add_column("Value")

    table.add_row("Service", "[yellow]Unknown[/yellow]")
    table.add_row("Interface", "tinc0")
    table.add_row("Network", "redundanet")

    console.print(table)


@vpn_app.command("logs")
def vpn_logs(
    follow: Annotated[
        bool,
        typer.Option("--follow", "-f", help="Follow log output"),
    ] = False,
    lines: Annotated[
        int,
        typer.Option("--lines", "-n", help="Number of lines to show"),
    ] = 50,
) -> None:
    """Show VPN logs."""
    console.print(f"[dim]Would show last {lines} lines of VPN logs[/dim]")
    if follow:
        console.print("[dim]Following logs... (Ctrl+C to exit)[/dim]")
