"""Network management CLI commands for RedundaNet."""

from __future__ import annotations

import shutil
from pathlib import Path
from typing import Annotated, Any

import typer
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

from redundanet.core.config import AppSettings, get_default_manifest_path, load_settings
from redundanet.core.deployment import Deployment, DeploymentError, git_sync
from redundanet.core.manifest import Manifest

app = typer.Typer(help="Network management commands")
console = Console()

INSTALL_DIR = Path("/opt/redundanet")
REPO_DIR = Path("/var/lib/redundanet/repo")

# Manifest role -> the docker-compose profile that runs that role's service.
_ROLE_PROFILES = {
    "tahoe_introducer": "introducer",
    "tahoe_storage": "storage",
    "tahoe_client": "client",
}


def _profiles_for_roles(roles: list[str]) -> list[str]:
    """Compose profiles for a node's manifest roles, in a stable de-duped order."""
    profiles: list[str] = []
    for role in roles:
        profile = _ROLE_PROFILES.get(role)
        if profile and profile not in profiles:
            profiles.append(profile)
    return profiles


def _merge_env(existing: str | None, updates: dict[str, str]) -> str:
    """Merge manifest-derived values into an existing ``.env``.

    Keys already present are updated in place; unknown operator-set keys (e.g.
    ``SFTP_ENABLED``, a tuned ``LOG_LEVEL``), comments, and ordering are kept
    verbatim; manifest keys not yet present are appended. This makes a re-join
    idempotent instead of clobbering a working node's local configuration.
    """
    header = (
        "# RedundaNet Node Configuration\n"
        "# Manifest-derived values managed by 'redundanet network join';\n"
        "# other keys are preserved across re-joins.\n\n"
    )
    if not existing or not existing.strip():
        body = "\n".join(f"{key}={value}" for key, value in updates.items())
        return header + body + "\n"

    remaining = dict(updates)
    out: list[str] = []
    for raw in existing.splitlines():
        stripped = raw.strip()
        if not stripped or stripped.startswith("#") or "=" not in stripped:
            out.append(raw)  # comment / blank / non-assignment: keep verbatim
            continue
        key = stripped.split("=", 1)[0].strip()
        if key in remaining:
            out.append(f"{key}={remaining.pop(key)}")  # update in place
        else:
            out.append(raw)  # operator/unknown key: preserve
    if remaining:
        if out and out[-1].strip():
            out.append("")
        out.extend(f"{key}={value}" for key, value in remaining.items())
    return "\n".join(out) + "\n"


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


def _load_manifest_dict(manifest_path: Path) -> dict[str, Any]:
    """Load the raw manifest mapping.

    A manifest that is not valid YAML (e.g. a bad hand-edit landed on the
    repo) must produce a clean, actionable error — not a raw traceback (a
    fresh node hit exactly that during a join).
    """
    import yaml

    try:
        with manifest_path.open() as f:
            data = yaml.safe_load(f)
    except yaml.YAMLError as e:
        console.print(f"[red]Error:[/red] the synced manifest is not valid YAML: {e}")
        console.print(
            "This usually means a broken commit landed on the manifest repository. "
            "Fix (or wait for a fix of) the manifest on the repo, then re-run "
            "[cyan]redundanet network join[/cyan]."
        )
        raise typer.Exit(1) from None
    return data if isinstance(data, dict) else {}


def _find_node_in_manifest(manifest_path: Path, node_name: str) -> dict[str, Any] | None:
    """Find a node by name in the manifest."""
    nodes: list[dict[str, Any]] = _load_manifest_dict(manifest_path).get("nodes", [])
    for node in nodes:
        if node.get("name") == node_name:
            return node
    return None


def _list_nodes_in_manifest(manifest_path: Path) -> list[dict[str, Any]]:
    """List all nodes in the manifest."""
    result: list[dict[str, Any]] = _load_manifest_dict(manifest_path).get("nodes", [])
    return result


def _generate_env_file(
    node: dict[str, Any],
    network: dict[str, Any],
    repo_url: str,
    branch: str,
    install_dir: Path,
) -> None:
    """Generate .env file from the node's and the network's manifest configuration.

    The tahoe encoding parameters and ports come from the manifest so the node
    runs what the network agreed on — without them the containers would fall
    back to the compose file's hardcoded defaults.
    """
    secrets_path = install_dir / "docker" / "secrets" / "gpg_private_key.asc"
    tahoe = network.get("tahoe", {}) or {}
    tinc_port = (node.get("ports", {}) or {}).get("tinc", 655)

    updates: dict[str, str] = {
        "NODE_NAME": str(node.get("name", "")),
        "VPN_IP": str(node.get("vpn_ip", "")),
        "PUBLIC_IP": str(node.get("public_ip", "auto")),
        "GPG_KEY_FILE": str(secrets_path),
        "MANIFEST_REPO": str(repo_url),
        "MANIFEST_BRANCH": str(branch),
        "TINC_PORT": str(tinc_port),
        "SHARES_NEEDED": str(tahoe.get("shares_needed", 3)),
        "SHARES_HAPPY": str(tahoe.get("shares_happy", 7)),
        "SHARES_TOTAL": str(tahoe.get("shares_total", 10)),
        "RESERVED_SPACE": str(tahoe.get("reserved_space", "1G")),
    }

    env_path = install_dir / ".env"
    existing = env_path.read_text() if env_path.exists() else None

    # Set GPG_KEY_ID from the manifest when it carries one; otherwise never blank
    # an existing value (a re-join with a keyless manifest must not strand a node
    # that already authenticates). Seed an empty value only on a fresh file.
    gpg_key_id = str(node.get("gpg_key_id", "") or "")
    if gpg_key_id:
        updates["GPG_KEY_ID"] = gpg_key_id
    elif existing is None:
        updates["GPG_KEY_ID"] = ""

    # Compose reads COMPOSE_PROJECT_NAME from the --env-file, so with this in
    # place a plain `docker compose --env-file .../.env up` lands in the same
    # project the redundanet CLI drives — no -p flag to forget. Never override
    # an operator's custom value.
    if existing is None or "COMPOSE_PROJECT_NAME" not in existing:
        updates["COMPOSE_PROJECT_NAME"] = "redundanet"

    env_path.write_text(_merge_env(existing, updates))
    verb = "updated" if existing else "created"
    console.print(f"[green]Environment file {verb}:[/green] {env_path}")


def _ensure_gpg_secret(gpg_key_id: str, install_dir: Path) -> None:
    """Make sure the containers' GPG private key file exists after a join.

    The tinc container reads /run/secrets/gpg_private_key from
    ``docker/secrets/gpg_private_key.asc``. Without this file Docker turns the
    missing bind-mount source into an empty directory and the VPN crash-loops
    — so export the key automatically when it is in the local keyring, and
    warn LOUDLY when it is not.
    """
    secrets_path = install_dir / "docker" / "secrets" / "gpg_private_key.asc"
    if secrets_path.is_file() and secrets_path.stat().st_size > 0:
        return  # already exported (and preserved across re-joins)

    if gpg_key_id:
        try:
            from redundanet.auth.gpg import GPGManager

            armored = GPGManager().export_private_key(gpg_key_id)
        except Exception:
            armored = ""
        if armored and "PRIVATE KEY BLOCK" in armored:
            secrets_path.parent.mkdir(parents=True, exist_ok=True)
            secrets_path.write_text(armored)
            secrets_path.chmod(0o600)
            console.print(
                f"[green]GPG private key exported for the containers:[/green] {secrets_path}"
            )
            return

    console.print(f"\n[bold red]WARNING: no GPG private key at {secrets_path}[/bold red]")
    console.print("[red]The VPN container cannot start without it.[/red] Export it now:")
    console.print(
        f"  [cyan]gpg --armor --export-secret-keys {gpg_key_id or '<FINGERPRINT>'} "
        f"> {secrets_path}[/cyan]"
    )
    console.print(f"  [cyan]chmod 600 {secrets_path}[/cyan]")


@app.command("join")
def join_network(
    node_name: Annotated[
        str | None,
        typer.Option("--name", "-n", help="Name of this node (must exist in manifest)"),
    ] = None,
    manifest_repo: Annotated[
        str | None,
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
        result = git_sync(repo, branch, REPO_DIR)
        if not result.success:
            detail = result.stderr.strip() or result.stdout.strip() or "unknown error"
            console.print(f"[red]Error cloning repository:[/red] {detail}")
            raise typer.Exit(1)
        console.print(f"[green]Repository cloned to:[/green] {REPO_DIR}")

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
    profiles: list[str] = []
    if manifest_file and manifest_file.exists():
        node = _find_node_in_manifest(manifest_file, name)
        if node:
            console.print(f"\n[green]Found node in manifest:[/green] {name}")
            network = _load_manifest_dict(manifest_file).get("network", {}) or {}
            _generate_env_file(node, network, repo, branch, install_dir)
            _ensure_gpg_secret(str(node.get("gpg_key_id", "") or ""), install_dir)
            profiles = _profiles_for_roles(node.get("roles", []) or [])
        else:
            console.print(f"[yellow]Warning:[/yellow] Node '{name}' not found in manifest")
            console.print("You'll need to create the .env file manually")

    console.print("\n[bold green]Successfully joined the network![/bold green]")
    console.print("\n[bold]Next steps:[/bold]")
    # Start via `docker compose` (v2) from the INSTALL dir (where join placed the
    # compose file + override), with the CLI's own project name so later
    # `redundanet` commands see the same containers, and the profile(s) for this
    # node's roles so the tahoe services actually start. Running from the dir
    # (no -f) auto-loads docker-compose.override.yml (e.g. a storage disk bind).
    compose_dir = install_dir / "docker"
    env_file = install_dir / ".env"
    profile_flags = "".join(f" --profile {profile}" for profile in profiles)
    console.print(
        f"Start services: [cyan]cd {compose_dir} && "
        f"docker compose -p redundanet --env-file {env_file}{profile_flags} up -d[/cyan]"
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
