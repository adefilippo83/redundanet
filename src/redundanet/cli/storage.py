"""Storage management CLI commands for RedundaNet.

These commands drive the running Tahoe-LAFS client container of the
docker-compose deployment (see :class:`redundanet.core.deployment.Deployment`).
"""

from __future__ import annotations

from pathlib import Path
from typing import Annotated, Optional

import typer
from rich.console import Console
from rich.panel import Panel
from rich.table import Table

from redundanet.core.config import AppSettings, load_settings
from redundanet.core.deployment import Deployment, DeploymentError
from redundanet.utils.process import CommandResult

app = typer.Typer(help="Storage management commands")
console = Console()


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
        Optional[str],
        typer.Argument(
            help="Optional directory destination like 'home:report.pdf' "
            "(an alias from 'storage mkdir'). Omit for an unlinked capability.",
        ),
    ] = None,
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
    container_path = f"/tmp/{source.name}"  # nosec B108
    node_dir = str(settings.client_node_dir)

    with console.status(f"[bold green]Uploading {source.name}..."):
        copy = deployment.cp_in(settings.client_service, source, container_path)
        if not copy.success:
            console.print(f"[red]Failed to copy file into client:[/red] {copy.stderr.strip()}")
            raise typer.Exit(1)
        put_args = ["tahoe", "-d", node_dir, "put", container_path]
        if dest:
            put_args.append(dest)
        result = deployment.exec(settings.client_service, put_args)
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
    console.print(cap)


@app.command("download")
def download_file(
    cap: Annotated[
        str,
        typer.Argument(help="Capability (URI:...) or directory path like 'home:report.pdf'"),
    ],
    destination: Annotated[
        Optional[Path],
        typer.Argument(help="Local destination path"),
    ] = None,
) -> None:
    """Download a file from the storage grid by capability or alias path."""
    deployment, settings = _deployment()
    dest = destination or Path("downloaded.out")
    node_dir = str(settings.client_node_dir)
    # Staging path inside the ephemeral, single-tenant client container (see above).
    container_path = "/tmp/redundanet-download"  # nosec B108

    with console.status(f"[bold green]Downloading to {dest}..."):
        result = deployment.exec(
            settings.client_service, ["tahoe", "-d", node_dir, "get", cap, container_path]
        )
        if not result.success:
            console.print(
                f"[red]Download failed:[/red] {result.stderr.strip() or result.stdout.strip()}"
            )
            raise typer.Exit(1)
        copy = deployment.cp_out(settings.client_service, container_path, dest)
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
        console.print(f"[dim]{result.stdout.strip()}[/dim]")


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
    console.print(result.stdout.rstrip() or "[dim]No aliases configured[/dim]")


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
    console.print(result.stdout.rstrip() or "[dim](empty)[/dim]")


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
