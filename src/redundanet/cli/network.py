"""Network management CLI commands for RedundaNet."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Annotated, Any, Optional

import typer
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

from redundanet.core.config import AppSettings, get_default_manifest_path, load_settings
from redundanet.core.deployment import Deployment, DeploymentError
from redundanet.core.manifest import Manifest

app = typer.Typer(help="Network management commands")
console = Console()

INSTALL_DIR = Path("/opt/redundanet")
REPO_DIR = Path("/var/lib/redundanet/repo")


def _deployment() -> tuple[Deployment, AppSettings]:
    """Return a ready-to-use Deployment, or exit with a friendly error."""
    settings = load_settings()
    deployment = Deployment(settings)
    try:
        deployment.require()
    except DeploymentError as e:
        console.print(f"[red]Error:[/red] {e}")
        raise typer.Exit(1) from None
    return deployment, settings


def _load_manifest(settings: AppSettings) -> Manifest | None:
    """Load the synced manifest, or None if it is missing/unparseable."""
    path = get_default_manifest_path(settings)
    if not path.exists():
        return None
    try:
        return Manifest.from_file(path)
    except Exception:  # - treat any load failure as "no manifest"
        return None


def _print_peer_table(
    deployment: Deployment,
    settings: AppSettings,
    manifest: Manifest,
    online_only: bool = False,
) -> None:
    """Render a table of peers with VPN reachability."""
    table = Table(title="Network Peers")
    table.add_column("Node", style="cyan")
    table.add_column("VPN IP", style="green")
    table.add_column("Status")

    for node in manifest.nodes:
        if node.name == settings.node_name:
            continue
        target = node.vpn_ip or node.internal_ip
        ping = deployment.exec(settings.tinc_service, ["ping", "-c", "1", "-W", "1", target])
        online = ping.success
        if online_only and not online:
            continue
        status = "[green]online[/green]" if online else "[red]offline[/red]"
        table.add_row(node.name, target, status)

    console.print(table)


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
    secrets_dir = dst_docker / "secrets"

    if not src_docker.exists():
        console.print("[yellow]Warning:[/yellow] No docker directory found in repo")
        return

    # Backup secrets if they exist (e.g., GPG key generated before join)
    secrets_backup = None
    if secrets_dir.exists():
        secrets_backup = install_dir / "secrets_backup"
        if secrets_backup.exists():
            shutil.rmtree(secrets_backup)
        shutil.move(str(secrets_dir), str(secrets_backup))

    # Remove existing docker dir if present
    if dst_docker.exists():
        shutil.rmtree(dst_docker)

    # Copy docker directory
    shutil.copytree(src_docker, dst_docker)

    # Restore secrets if they were backed up
    if secrets_backup and secrets_backup.exists():
        if secrets_dir.exists():
            shutil.rmtree(secrets_dir)
        shutil.move(str(secrets_backup), str(secrets_dir))
    else:
        # Create empty secrets directory
        secrets_dir.mkdir(exist_ok=True)

    console.print(f"[green]Docker files installed to:[/green] {dst_docker}")


def _setup_manifest(repo_dir: Path) -> Path | None:
    """Copy manifest files to the data directory. Returns path to main manifest."""
    src_manifest = repo_dir / "manifests"
    dst_manifest = Path("/var/lib/redundanet/manifest")

    if not src_manifest.exists():
        console.print("[yellow]Warning:[/yellow] No manifests directory found in repo")
        return None

    dst_manifest.mkdir(parents=True, exist_ok=True)

    manifest_file = None
    # Copy manifest files
    for f in src_manifest.glob("*.yaml"):
        shutil.copy(f, dst_manifest / f.name)
        if f.name == "manifest.yaml":
            manifest_file = dst_manifest / f.name
    for f in src_manifest.glob("*.json"):
        shutil.copy(f, dst_manifest / f.name)

    console.print(f"[green]Manifest files installed to:[/green] {dst_manifest}")
    return manifest_file


def _find_node_in_manifest(manifest_path: Path, node_name: str) -> dict[str, Any] | None:
    """Find a node by name in the manifest."""
    import yaml

    with manifest_path.open() as f:
        manifest = yaml.safe_load(f)

    nodes: list[dict[str, Any]] = manifest.get("nodes", [])
    for node in nodes:
        if node.get("name") == node_name:
            return node
    return None


def _list_nodes_in_manifest(manifest_path: Path) -> list[dict[str, Any]]:
    """List all nodes in the manifest."""
    import yaml

    with manifest_path.open() as f:
        manifest = yaml.safe_load(f)

    result: list[dict[str, Any]] = manifest.get("nodes", [])
    return result


def _generate_env_file(node: dict[str, Any], repo_url: str, branch: str, install_dir: Path) -> None:
    """Generate .env file from node configuration."""
    secrets_path = install_dir / "docker" / "secrets" / "gpg_private_key.asc"
    env_content = f"""# RedundaNet Node Configuration
# Auto-generated by 'redundanet network join'

NODE_NAME={node.get('name', '')}
VPN_IP={node.get('vpn_ip', '')}
PUBLIC_IP={node.get('public_ip', 'auto')}
GPG_KEY_ID={node.get('gpg_key_id', '')}
GPG_KEY_FILE={secrets_path}
MANIFEST_REPO={repo_url}
MANIFEST_BRANCH={branch}
"""
    env_path = install_dir / ".env"
    env_path.write_text(env_content)
    console.print(f"[green]Environment file created:[/green] {env_path}")


@app.command("join")
def join_network(
    node_name: Annotated[
        Optional[str],
        typer.Option("--name", "-n", help="Name of this node (must exist in manifest)"),
    ] = None,
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
    settings = load_settings()
    repo = manifest_repo or settings.manifest_repo
    name = node_name or settings.node_name

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
        manifest_file = _setup_manifest(REPO_DIR)

    # If no node name provided, list available nodes
    if not name:
        if manifest_file and manifest_file.exists():
            nodes = _list_nodes_in_manifest(manifest_file)
            if nodes:
                console.print("\n[bold]Available nodes in manifest:[/bold]")
                table = Table(show_header=True)
                table.add_column("Name", style="cyan")
                table.add_column("VPN IP", style="green")
                table.add_column("Roles")
                for n in nodes:
                    roles = ", ".join(n.get("roles", []))
                    table.add_row(n.get("name", ""), n.get("vpn_ip", ""), roles)
                console.print(table)
                console.print(
                    "\n[yellow]Run again with --name <node-name> to configure this node[/yellow]"
                )
                raise typer.Exit(0)

        console.print("[red]Error:[/red] No node name specified")
        console.print("Use --name or set REDUNDANET_NODE_NAME")
        raise typer.Exit(1)

    # Find node in manifest and generate .env
    if manifest_file and manifest_file.exists():
        node = _find_node_in_manifest(manifest_file, name)
        if node:
            console.print(f"\n[green]Found node in manifest:[/green] {name}")
            _generate_env_file(node, repo, branch, install_dir)
        else:
            console.print(f"[yellow]Warning:[/yellow] Node '{name}' not found in manifest")
            console.print("You'll need to create the .env file manually")

    console.print("\n[bold green]Successfully joined the network![/bold green]")
    console.print("\n[bold]Next steps:[/bold]")
    console.print(
        f"Start services: [cyan]cd {REPO_DIR / 'docker'} && docker-compose --env-file {install_dir / '.env'} up -d[/cyan]"
    )


@app.command("leave")
def leave_network(
    force: Annotated[
        bool,
        typer.Option("--force", "-f", help="Skip confirmation"),
    ] = False,
) -> None:
    """Leave the network: stop and remove the local deployment."""
    deployment, _ = _deployment()

    if not force:
        confirm = typer.confirm("Stop and remove all RedundaNet containers?")
        if not confirm:
            console.print("Aborted.")
            raise typer.Exit(0)

    with console.status("[bold yellow]Leaving network (docker compose down)..."):
        result = deployment.down()

    if not result.success:
        console.print(f"[red]Failed to leave network:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)
    console.print("[yellow]Left the RedundaNet network[/yellow]")


@app.command("status")
def network_status(
    verbose: Annotated[
        bool,
        typer.Option("--verbose", "-v", help="Show detailed status"),
    ] = False,
) -> None:
    """Show the status of the VPN connection."""
    deployment, settings = _deployment()
    console.print(Panel("[bold]Network Status[/bold]", expand=False))

    tinc = deployment.service_status(settings.tinc_service)
    table = Table(title="VPN Connection", show_header=True)
    table.add_column("Property", style="cyan")
    table.add_column("Value")
    table.add_row("Interface", "redundanet")

    if tinc is None or tinc.state != "running":
        table.add_row("Service", "[yellow]not running[/yellow]")
        console.print(table)
        return

    table.add_row("Service", f"[green]{tinc.health or 'running'}[/green]")

    addr = deployment.exec(settings.tinc_service, ["ip", "-o", "-4", "addr", "show", "redundanet"])
    local_ip = ""
    if addr.success and addr.stdout.strip():
        local_ip = next(
            (p.split("/")[0] for p in addr.stdout.split() if "/" in p and p[0].isdigit()), ""
        )
    table.add_row("Local IP", local_ip or "[dim]--[/dim]")

    manifest = _load_manifest(settings)
    peer_count = max(len(manifest.nodes) - 1, 0) if manifest else 0
    table.add_row("Configured Peers", str(peer_count))
    console.print(table)

    if verbose and manifest is not None:
        console.print("\n[bold]Peers:[/bold]")
        _print_peer_table(deployment, settings, manifest)


@app.command("peers")
def list_peers(
    online_only: Annotated[
        bool,
        typer.Option("--online", "-o", help="Show only online peers"),
    ] = False,
) -> None:
    """List peers and their reachability over the VPN."""
    deployment, settings = _deployment()
    manifest = _load_manifest(settings)
    if manifest is None:
        console.print(
            "[red]Error:[/red] No manifest found. Run 'redundanet sync' or 'redundanet network join'."
        )
        raise typer.Exit(1)
    _print_peer_table(deployment, settings, manifest, online_only=online_only)


@app.command("ping")
def ping_node(
    node_name: Annotated[str, typer.Argument(help="Name of the node to ping")],
    count: Annotated[
        int,
        typer.Option("--count", "-c", help="Number of ping packets"),
    ] = 4,
) -> None:
    """Ping a node in the network over the VPN."""
    deployment, settings = _deployment()
    manifest = _load_manifest(settings)
    if manifest is None:
        console.print("[red]Error:[/red] No manifest found.")
        raise typer.Exit(1)

    node = manifest.get_node(node_name)
    if node is None:
        console.print(f"[red]Error:[/red] Node '{node_name}' not found in manifest")
        raise typer.Exit(1)

    target = node.vpn_ip or node.internal_ip
    console.print(f"[bold]Pinging {node_name} ({target})[/bold]")
    result = deployment.exec(
        settings.tinc_service, ["ping", "-c", str(count), target], capture=False
    )
    raise typer.Exit(0 if result.success else 1)


# VPN subcommands
vpn_app = typer.Typer(help="VPN management commands")
app.add_typer(vpn_app, name="vpn")


@vpn_app.command("start")
def vpn_start() -> None:
    """Start the Tinc VPN container."""
    deployment, settings = _deployment()
    with console.status("[bold green]Starting VPN..."):
        result = deployment.up([settings.tinc_service])
    if not result.success:
        console.print(f"[red]Failed to start VPN:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)
    console.print("[green]VPN started[/green]")


@vpn_app.command("stop")
def vpn_stop() -> None:
    """Stop the Tinc VPN container."""
    deployment, settings = _deployment()
    with console.status("[bold yellow]Stopping VPN..."):
        result = deployment.stop([settings.tinc_service])
    if not result.success:
        console.print(f"[red]Failed to stop VPN:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)
    console.print("[yellow]VPN stopped[/yellow]")


@vpn_app.command("restart")
def vpn_restart() -> None:
    """Restart the Tinc VPN container."""
    deployment, settings = _deployment()
    with console.status("[bold green]Restarting VPN..."):
        result = deployment.compose("restart", settings.tinc_service)
    if not result.success:
        console.print(f"[red]Failed to restart VPN:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)
    console.print("[green]VPN restarted[/green]")


@vpn_app.command("status")
def vpn_status() -> None:
    """Show VPN status."""
    deployment, settings = _deployment()
    tinc = deployment.service_status(settings.tinc_service)

    table = Table(title="VPN Status", show_header=False)
    table.add_column("Property", style="cyan")
    table.add_column("Value")
    table.add_row("Service", (tinc.health or tinc.state) if tinc is not None else "not running")
    table.add_row("Interface", "redundanet")
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
    deployment, settings = _deployment()
    result = deployment.logs(settings.tinc_service, follow=follow, tail=lines)
    if not follow:
        console.print(result.stdout.rstrip() or result.stderr.rstrip() or "[dim]no logs[/dim]")
