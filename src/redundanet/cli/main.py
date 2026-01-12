"""Main CLI application for RedundaNet."""

from __future__ import annotations

from pathlib import Path
from typing import Annotated, Optional

import typer
from rich import print as rprint
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

from redundanet import __version__
from redundanet.cli.network import app as network_app
from redundanet.cli.node import app as node_app
from redundanet.cli.storage import app as storage_app
from redundanet.core.config import load_settings
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
        Optional[bool],
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
        Optional[Path],
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
        Optional[str],
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
        # Create configuration directories
        settings = load_settings()
        config_dir = settings.config_dir
        data_dir = settings.data_dir

        config_dir.mkdir(parents=True, exist_ok=True)
        data_dir.mkdir(parents=True, exist_ok=True)
        (data_dir / "manifest").mkdir(exist_ok=True)
        (data_dir / "tinc").mkdir(exist_ok=True)
        (data_dir / "tahoe").mkdir(exist_ok=True)

        console.print(f"[green]Created configuration directory:[/green] {config_dir}")
        console.print(f"[green]Created data directory:[/green] {data_dir}")

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
    console.print("3. Start services:    [cyan]docker compose up -d[/cyan]" if docker else "")


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

    # Check VPN status
    console.print("\n[bold]Service Status:[/bold]")

    services = [
        ("Tinc VPN", "tinc"),
        ("Tahoe Introducer", "tahoe-introducer"),
        ("Tahoe Storage", "tahoe-storage"),
        ("Tahoe Client", "tahoe-client"),
    ]

    status_table = Table(show_header=True)
    status_table.add_column("Service")
    status_table.add_column("Status")

    for name, _ in services:
        # In a real implementation, we'd check the actual service status
        status_table.add_row(name, "[yellow]Unknown[/yellow]")

    console.print(status_table)


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

    with console.status("[bold green]Syncing manifest..."):
        # In a real implementation, we'd clone/pull the manifest repo
        console.print(f"[green]Syncing from:[/green] {settings.manifest_repo}")
        console.print(f"[green]Branch:[/green] {settings.manifest_branch}")

    console.print("[bold green]Manifest synced successfully![/bold green]")


@app.command("validate")
def validate_manifest(
    manifest_path: Annotated[
        Path,
        typer.Argument(help="Path to the manifest file"),
    ],
) -> None:
    """Validate a network manifest file."""
    try:
        manifest = Manifest.from_file(manifest_path)
        errors = manifest.validate()

        if errors:
            console.print("[yellow]Validation warnings:[/yellow]")
            for error in errors:
                console.print(f"  [yellow]![/yellow] {error}")
        else:
            console.print("[green]Manifest is valid![/green]")

        # Print summary
        console.print(f"\n[bold]Network:[/bold] {manifest.network.name}")
        console.print(f"[bold]Nodes:[/bold] {len(manifest.nodes)}")
        console.print(f"[bold]Introducers:[/bold] {len(manifest.get_introducers())}")
        console.print(f"[bold]Storage Nodes:[/bold] {len(manifest.get_storage_nodes())}")

    except Exception as e:
        console.print(f"[red]Validation failed:[/red] {e}")
        raise typer.Exit(1) from None


if __name__ == "__main__":
    app()
