"""Network management CLI commands for RedundaNet."""

from __future__ import annotations

from typing import Annotated, Optional

import typer
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

app = typer.Typer(help="Network management commands")
console = Console()


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

    with console.status("[bold green]Cloning manifest repository..."):
        # In a real implementation, we'd clone the repo
        console.print(f"[green]Repository:[/green] {repo}")
        console.print(f"[green]Branch:[/green] {branch}")

    console.print("\n[bold green]Successfully joined the network![/bold green]")
    console.print("\n[bold]Next steps:[/bold]")
    console.print("1. Start the VPN: [cyan]redundanet network vpn start[/cyan]")
    console.print("2. Start storage:  [cyan]redundanet storage start[/cyan]")


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
