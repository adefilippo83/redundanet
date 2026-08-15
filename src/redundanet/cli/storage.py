"""Storage management CLI commands for RedundaNet.

These commands drive the running Tahoe-LAFS client container of the
docker-compose deployment (see :class:`redundanet.core.deployment.Deployment`).
"""

from __future__ import annotations

from pathlib import Path
from typing import Annotated

import typer
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

from redundanet.core.config import AppSettings, load_settings
from redundanet.core.deployment import Deployment, DeploymentError
from redundanet.utils.process import CommandResult

app = typer.Typer(help="Storage management commands")
console = Console()

# Bulk transfers (upload/download) ride the erasure-coding + mesh path and can
# far outlast the 120s default used for control commands; give them an hour by
# default, overridable with --timeout.
DATA_TIMEOUT = 3600


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


def _report_check(result: CommandResult) -> None:
    """Render the output of a ``tahoe check``/``--repair`` run, then exit on failure."""
    if result.success:
        console.print(result.stdout.rstrip() or "[green]Healthy[/green]")
        return
    combined = result.stdout + result.stderr
    if "Method Not Allowed" in combined or "405" in combined:
        console.print(
            "[yellow]Health check is unavailable on this node[/yellow] "
            "(the Tahoe web API returned 405 for the check request)."
        )
    else:
        detail = (result.stderr.strip() or result.stdout.strip())[:300]
        console.print(f"[red]Check failed:[/red] {detail}")
    raise typer.Exit(1)


@app.command("status")
def storage_status(
    verbose: Annotated[
        bool,
        typer.Option("--verbose", "-v", help="Show detailed status"),
    ] = False,
) -> None:
    """Show storage status and statistics."""
    deployment, settings = _deployment()
    console.print(Panel("[bold]Storage Status[/bold]", expand=False))

    statuses = {s.name: s for s in deployment.ps()}
    table = Table(title="Services", show_header=True)
    table.add_column("Service", style="cyan")
    table.add_column("State")
    table.add_column("Health")
    for label, svc in (
        ("Introducer", settings.introducer_service),
        ("Storage", settings.storage_service),
        ("Client", settings.client_service),
    ):
        s = statuses.get(svc)
        if s is None:
            table.add_row(label, "[dim]not created[/dim]", "")
        else:
            state = (
                f"[green]{s.state}[/green]"
                if s.state == "running"
                else f"[yellow]{s.state}[/yellow]"
            )
            table.add_row(label, state, s.health or "—")
    console.print(table)

    # Introducer FURL presence (read from the introducer container)
    furl_path = f"{settings.introducer_node_dir}/private/introducer.furl"
    furl_result = deployment.exec(settings.introducer_service, ["cat", furl_path])
    furl = furl_result.stdout.strip() if furl_result.success else ""
    furl_state = "[green]set[/green]" if furl else "[yellow]not available[/yellow]"
    console.print(f"\n[bold]Introducer FURL:[/bold] {furl_state}")
    if verbose and furl:
        console.print(f"[dim]{furl}[/dim]")


@app.command("start")
def storage_start() -> None:
    """Start the storage and client services."""
    deployment, settings = _deployment()
    with console.status("[bold green]Starting storage services..."):
        result = deployment.up([settings.storage_service, settings.client_service])
    if not result.success:
        console.print(f"[red]Failed to start services:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)
    console.print("[green]Started storage and client services[/green]")


@app.command("stop")
def storage_stop() -> None:
    """Stop the storage and client services."""
    deployment, settings = _deployment()
    with console.status("[bold yellow]Stopping storage services..."):
        result = deployment.stop([settings.storage_service, settings.client_service])
    if not result.success:
        console.print(f"[red]Failed to stop services:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)
    console.print("[yellow]Stopped storage and client services[/yellow]")


@app.command("upload")
def upload_file(
    source: Annotated[Path, typer.Argument(help="File to upload")],
    dest: Annotated[
        str | None,
        typer.Argument(
            help="Optional directory destination like 'home:report.pdf' "
            "(an alias from 'storage mkdir'). Omit for an unlinked capability.",
        ),
    ] = None,
    timeout: Annotated[
        int,
        typer.Option(
            "--timeout",
            help="Seconds allowed for the copy and the erasure-coded upload. "
            "Raise for large files: the default suits data transfer, not the 120s "
            "control-command default.",
        ),
    ] = DATA_TIMEOUT,
) -> None:
    """Upload a file to the grid.

    With no destination the file's capability (``URI:...``) is printed. With a
    destination of the form ``alias:name`` the file is linked into that directory
    so it can be listed with ``storage ls alias:``.
    """
    if not source.exists() or not source.is_file():
        console.print(f"[red]Error:[/red] File not found: {source}")
        raise typer.Exit(1)

    deployment, settings = _deployment()
    # Staging path inside the ephemeral, single-tenant client container (created
    # and removed by us); not a host temp file, so B108's symlink risk doesn't apply.
    container_path = f"/tmp/{source.name}"  # noqa: S108  # nosec B108
    node_dir = str(settings.client_node_dir)

    with console.status(f"[bold green]Uploading {source.name}..."):
        copy = deployment.cp_in(settings.client_service, source, container_path, timeout=timeout)
        if not copy.success:
            console.print(f"[red]Failed to copy file into client:[/red] {copy.stderr.strip()}")
            raise typer.Exit(1)
        put_args = ["tahoe", "-d", node_dir, "put", container_path]
        if dest:
            put_args.append(dest)
        result = deployment.exec(settings.client_service, put_args, timeout=timeout)
        deployment.exec(settings.client_service, ["rm", "-f", container_path])

    if not result.success:
        console.print(f"[red]Upload failed:[/red] {result.stderr.strip() or result.stdout.strip()}")
        raise typer.Exit(1)

    if dest:
        console.print(f"[green]Uploaded[/green] {source.name} -> [cyan]{dest}[/cyan]")
        return

    cap = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else ""
    if not cap:
        console.print("[red]Upload failed:[/red] no capability returned")
        raise typer.Exit(1)
    console.print(f"[green]Uploaded[/green] {source.name}")
    # soft_wrap: a capability must come out as ONE line — rich would otherwise
    # hard-wrap it at the terminal width, truncating what scripts capture.
    console.print(cap, soft_wrap=True)


@app.command("download")
def download_file(
    cap: Annotated[
        str,
        typer.Argument(help="Capability (URI:...) or directory path like 'home:report.pdf'"),
    ],
    destination: Annotated[
        Path | None,
        typer.Argument(help="Local destination path"),
    ] = None,
    timeout: Annotated[
        int,
        typer.Option(
            "--timeout",
            help="Seconds allowed for the erasure-coded fetch and the copy out. "
            "Raise for large files (the 120s control default is too short).",
        ),
    ] = DATA_TIMEOUT,
) -> None:
    """Download a file from the storage grid by capability or alias path."""
    deployment, settings = _deployment()
    dest = destination or Path("downloaded.out")
    node_dir = str(settings.client_node_dir)
    # Staging path inside the ephemeral, single-tenant client container (see above).
    container_path = "/tmp/redundanet-download"  # noqa: S108  # nosec B108

    with console.status(f"[bold green]Downloading to {dest}..."):
        result = deployment.exec(
            settings.client_service,
            ["tahoe", "-d", node_dir, "get", cap, container_path],
            timeout=timeout,
        )
        if not result.success:
            console.print(
                f"[red]Download failed:[/red] {result.stderr.strip() or result.stdout.strip()}"
            )
            raise typer.Exit(1)
        copy = deployment.cp_out(settings.client_service, container_path, dest, timeout=timeout)
        deployment.exec(settings.client_service, ["rm", "-f", container_path])

    if not copy.success:
        console.print(f"[red]Failed to copy file out of client:[/red] {copy.stderr.strip()}")
        raise typer.Exit(1)
    console.print(f"[green]Downloaded[/green] -> {dest}")


@app.command("mkdir")
def make_directory(
    alias: Annotated[str, typer.Argument(help="Alias name for the new directory, e.g. 'home'")],
) -> None:
    """Create a directory on the grid and give it an alias.

    The alias becomes a browsable namespace: upload into it with
    ``storage upload <file> home:<name>`` and list it with ``storage ls home:``.
    """
    deployment, settings = _deployment()
    node_dir = str(settings.client_node_dir)
    result = deployment.exec(
        settings.client_service, ["tahoe", "-d", node_dir, "create-alias", alias]
    )
    if not result.success:
        combined = result.stdout + result.stderr
        if "already" in combined.lower():
            console.print(f"[yellow]Alias '{alias}' already exists.[/yellow]")
        else:
            console.print(f"[red]Failed to create directory:[/red] {combined.strip()[:200]}")
        raise typer.Exit(1)
    console.print(f"[green]Created directory[/green] [cyan]{alias}:[/cyan]")
    if result.stdout.strip():
        console.print(f"[dim]{result.stdout.strip()}[/dim]", soft_wrap=True)


@app.command("aliases")
def list_aliases() -> None:
    """List the directory aliases configured on this node."""
    deployment, settings = _deployment()
    node_dir = str(settings.client_node_dir)
    result = deployment.exec(settings.client_service, ["tahoe", "-d", node_dir, "list-aliases"])
    if not result.success:
        console.print(
            f"[red]Failed to list aliases:[/red] {result.stderr.strip() or result.stdout.strip()}"
        )
        raise typer.Exit(1)
    console.print(result.stdout.rstrip() or "[dim]No aliases configured[/dim]", soft_wrap=True)


@app.command("ls")
def list_files(
    target: Annotated[
        str,
        typer.Argument(help="Directory capability or alias to list, e.g. 'home:'"),
    ],
    long: Annotated[
        bool,
        typer.Option("--long", "-l", help="Show detailed listing"),
    ] = False,
) -> None:
    """List the contents of a directory capability or alias."""
    deployment, settings = _deployment()
    node_dir = str(settings.client_node_dir)
    args = ["tahoe", "-d", node_dir, "ls"]
    if long:
        args.append("--long")
    args.append(target)

    result = deployment.exec(settings.client_service, args)
    if not result.success:
        console.print(f"[red]List failed:[/red] {result.stderr.strip() or result.stdout.strip()}")
        raise typer.Exit(1)
    console.print(result.stdout.rstrip() or "[dim](empty)[/dim]", soft_wrap=True)


@app.command("info")
def file_info(
    cap: Annotated[str, typer.Argument(help="Capability to inspect")],
) -> None:
    """Check the health of a file or directory capability."""
    deployment, settings = _deployment()
    node_dir = str(settings.client_node_dir)
    result = deployment.exec(settings.client_service, ["tahoe", "-d", node_dir, "check", cap])
    _report_check(result)


@app.command("repair")
def repair_file(
    cap: Annotated[str, typer.Argument(help="Capability to check/repair")],
    check_only: Annotated[
        bool,
        typer.Option("--check", "-c", help="Only check, don't repair"),
    ] = False,
) -> None:
    """Check and repair the redundancy of a capability."""
    deployment, settings = _deployment()
    node_dir = str(settings.client_node_dir)
    args = ["tahoe", "-d", node_dir, "check"]
    if not check_only:
        args.append("--repair")
    args.append(cap)

    action = "Checking" if check_only else "Repairing"
    with console.status(f"[bold green]{action} {cap[:24]}..."):
        result = deployment.exec(settings.client_service, args)
    _report_check(result)


@app.command("renew")
def renew_leases(
    target: Annotated[
        str | None,
        typer.Argument(
            help="Alias (e.g. 'home:') or capability to renew. Omit to renew every alias."
        ),
    ] = None,
) -> None:
    """Renew storage leases so shares are not garbage-collected.

    Storage nodes expire shares whose lease is older than the network's lease
    duration (default 90 days). The client container renews all aliases
    automatically once a week; use this command for manual renewal or for bare
    capabilities that are not linked into an alias.
    """
    deployment, settings = _deployment()
    node_dir = str(settings.client_node_dir)

    if target:
        targets = [target]
    else:
        listing = deployment.exec(
            settings.client_service, ["tahoe", "-d", node_dir, "list-aliases"]
        )
        if not listing.success:
            console.print(
                f"[red]Failed to list aliases:[/red] "
                f"{listing.stderr.strip() or listing.stdout.strip()}"
            )
            raise typer.Exit(1)
        targets = [
            line.split(":", 1)[0].strip() + ":"
            for line in listing.stdout.splitlines()
            if ":" in line
        ]
        if not targets:
            console.print("[dim]No aliases configured; nothing to renew.[/dim]")
            return

    failures = 0
    for item in targets:
        with console.status(f"[bold green]Renewing leases for {item}..."):
            result = deployment.exec(
                settings.client_service,
                ["tahoe", "-d", node_dir, "deep-check", "--add-lease", item],
                timeout=600,
            )
        if result.success:
            console.print(f"[green]Renewed[/green] {item}")
        else:
            failures += 1
            detail = (result.stderr.strip() or result.stdout.strip())[:200]
            console.print(f"[red]Failed to renew {item}:[/red] {detail}")

    if failures:
        raise typer.Exit(1)


SFTP_ACCOUNTS = "/var/lib/tahoe-client/private/sftp_accounts"


def parse_pubkey(pubkey: str) -> str:
    """Return the 'ssh-rsa <blob>' pair from a public key line, or raise."""
    parts = pubkey.split()
    if len(parts) < 2 or not parts[0].startswith(("ssh-", "ecdsa-")):
        raise ValueError("not a valid SSH public key (expected 'ssh-rsa AAAA...')")
    return f"{parts[0]} {parts[1]}"


def _sftp_root_cap(deployment: Deployment, settings: AppSettings) -> str:
    """The DIR2 capability of the dedicated 'sftp' alias, creating it if needed."""
    node_dir = str(settings.client_node_dir)
    listing = deployment.exec(settings.client_service, ["tahoe", "-d", node_dir, "list-aliases"])
    for line in listing.stdout.splitlines() if listing.success else []:
        name, _, cap = line.partition(":")
        if name.strip() == "sftp" and cap.strip().startswith("URI:"):
            return cap.strip()
    created = deployment.exec(
        settings.client_service, ["tahoe", "-d", node_dir, "create-alias", "sftp"]
    )
    if not created.success:
        raise RuntimeError(created.stderr.strip() or created.stdout.strip())
    listing = deployment.exec(settings.client_service, ["tahoe", "-d", node_dir, "list-aliases"])
    for line in listing.stdout.splitlines():
        name, _, cap = line.partition(":")
        if name.strip() == "sftp":
            return cap.strip()
    raise RuntimeError("could not resolve the 'sftp' alias after creating it")


sftp_app = typer.Typer(help="SFTP frontend: mount the grid as a filesystem")
app.add_typer(sftp_app, name="sftp")


@sftp_app.command("adduser")
def sftp_adduser(
    user: Annotated[str, typer.Option("--user", "-u", help="SFTP username")],
    pubkey_file: Annotated[
        Path, typer.Argument(help="Path to the user's SSH public key (e.g. ~/.ssh/id_ed25519.pub)")
    ],
) -> None:
    """Grant an SSH public key SFTP access to the shared 'sftp' directory.

    Requires SFTP to be enabled on the node (SFTP_ENABLED=true in the .env, then
    recreate the stack). The user then connects with:
        sftp -P <port> <user>@<node-lan-ip>
    """
    if not pubkey_file.exists():
        console.print(f"[red]Error:[/red] public key file not found: {pubkey_file}")
        raise typer.Exit(1)
    try:
        keypair = parse_pubkey(pubkey_file.read_text())
    except ValueError as e:
        console.print(f"[red]Error:[/red] {e}")
        raise typer.Exit(1) from None

    deployment, settings = _deployment()
    # Confirm SFTP is actually enabled (the accounts file only exists then).
    check = deployment.exec(settings.client_service, ["test", "-f", SFTP_ACCOUNTS])
    if not check.success:
        console.print(
            "[red]SFTP is not enabled on this node.[/red]\n"
            "Enable it first: set [cyan]SFTP_ENABLED=true[/cyan] (and "
            "[cyan]SFTP_BIND=0.0.0.0[/cyan] for LAN access) in "
            "/opt/redundanet/.env, then [cyan]redundanet update[/cyan] "
            "(or recreate the stack)."
        )
        raise typer.Exit(1)

    try:
        rootcap = _sftp_root_cap(deployment, settings)
    except RuntimeError as e:
        console.print(f"[red]Error preparing the sftp directory:[/red] {e}")
        raise typer.Exit(1) from None

    # Remove any existing line for this user, then append the new one.
    line = f"{user} {keypair} {rootcap}"
    script = (
        f"touch {SFTP_ACCOUNTS}; "
        f"grep -v '^{user} ' {SFTP_ACCOUNTS} > {SFTP_ACCOUNTS}.tmp || true; "
        f"mv {SFTP_ACCOUNTS}.tmp {SFTP_ACCOUNTS}; "
        f"printf '%s\\n' {line!r} >> {SFTP_ACCOUNTS}"
    )
    result = deployment.exec(settings.client_service, ["sh", "-c", script])
    if not result.success:
        console.print(f"[red]Failed to write account:[/red] {result.stderr.strip()}")
        raise typer.Exit(1)

    with console.status("[bold green]Restarting client to load the account..."):
        deployment.compose("restart", settings.client_service)
    console.print(
        f"[green]SFTP access granted to [cyan]{user}[/cyan][/green] on the 'sftp:' directory."
    )
    console.print(
        "\nConnect (from a machine on the node's LAN, default port 8022):\n"
        f"  [cyan]sftp -P 8022 {user}@<node-lan-ip>[/cyan]\n"
        f"  or mount it:  [cyan]sshfs -p 8022 {user}@<node-lan-ip>:/ /mnt/grid[/cyan]"
    )


@sftp_app.command("listusers")
def sftp_listusers() -> None:
    """List the SFTP usernames configured on this node."""
    deployment, settings = _deployment()
    result = deployment.exec(settings.client_service, ["cat", SFTP_ACCOUNTS])
    if not result.success:
        console.print("[yellow]SFTP is not enabled (no accounts file).[/yellow]")
        raise typer.Exit(1)
    users = [
        line.split()[0]
        for line in result.stdout.splitlines()
        if line.strip() and not line.startswith("#")
    ]
    console.print("[bold]SFTP users:[/bold] " + (", ".join(users) if users else "[dim]none[/dim]"))


@sftp_app.command("removeuser")
def sftp_removeuser(
    user: Annotated[str, typer.Option("--user", "-u", help="SFTP username to remove")],
) -> None:
    """Remove an SFTP user's access."""
    deployment, settings = _deployment()
    script = (
        f"grep -v '^{user} ' {SFTP_ACCOUNTS} > {SFTP_ACCOUNTS}.tmp && "
        f"mv {SFTP_ACCOUNTS}.tmp {SFTP_ACCOUNTS}"
    )
    result = deployment.exec(settings.client_service, ["sh", "-c", script])
    if not result.success:
        console.print(f"[red]Failed:[/red] {result.stderr.strip() or 'SFTP not enabled?'}")
        raise typer.Exit(1)
    deployment.compose("restart", settings.client_service)
    console.print(f"[green]Removed SFTP access for [cyan]{user}[/cyan].[/green]")


@app.command("mount")
def mount_storage(
    mountpoint: Annotated[
        Path,
        typer.Argument(help="Directory to mount Tahoe filesystem"),
    ] = Path("/mnt/redundanet"),
) -> None:
    """(Unavailable) Mount the Tahoe-LAFS filesystem.

    Native FUSE mounting was removed in Tahoe-LAFS 1.20, so this is not
    supported in the current release.
    """
    console.print(
        Panel(
            "[yellow]FUSE mounting is not available[/yellow] with Tahoe-LAFS 1.20.\n\n"
            "Use [cyan]redundanet storage download <cap> <path>[/cyan] to retrieve files, "
            "or [cyan]redundanet storage upload <path>[/cyan] to store them.",
            title="Not supported",
        )
    )
    raise typer.Exit(1)


@app.command("unmount")
def unmount_storage(
    mountpoint: Annotated[
        Path,
        typer.Argument(help="Mountpoint to unmount"),
    ] = Path("/mnt/redundanet"),
) -> None:
    """(Unavailable) Unmount the Tahoe-LAFS filesystem."""
    console.print(
        "[yellow]Nothing to unmount:[/yellow] FUSE mounting is not available with Tahoe-LAFS 1.20."
    )
    raise typer.Exit(1)
