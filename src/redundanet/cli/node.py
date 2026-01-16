"""Node management CLI commands for RedundaNet."""

from __future__ import annotations

from pathlib import Path
from typing import Annotated, Optional

import typer
from rich.console import Console
from rich.table import Table

from redundanet.core.config import NodeRole, NodeStatus
from redundanet.core.manifest import Manifest

app = typer.Typer(help="Node management commands")
console = Console()


@app.command("list")
def list_nodes(
    manifest_path: Annotated[
        Optional[Path],
        typer.Option("--manifest", "-m", help="Path to manifest file"),
    ] = None,
    role: Annotated[
        Optional[str],
        typer.Option("--role", "-r", help="Filter by role"),
    ] = None,
    status: Annotated[
        Optional[str],
        typer.Option("--status", "-s", help="Filter by status"),
    ] = None,
) -> None:
    """List all nodes in the network."""
    if manifest_path is None:
        from redundanet.core.config import get_default_manifest_path

        manifest_path = get_default_manifest_path()

    if not manifest_path.exists():
        console.print(f"[red]Error:[/red] Manifest not found: {manifest_path}")
        raise typer.Exit(1)

    manifest = Manifest.from_file(manifest_path)
    nodes = manifest.nodes

    # Apply filters
    if role:
        nodes = [n for n in nodes if role in [r.value for r in n.roles]]
    if status:
        nodes = [n for n in nodes if n.status.value == status]

    # Create table
    table = Table(title=f"RedundaNet Nodes ({len(nodes)})")
    table.add_column("Name", style="cyan")
    table.add_column("VPN IP", style="green")
    table.add_column("Status")
    table.add_column("Roles")
    table.add_column("Region")

    for node in nodes:
        status_style = {
            "active": "[green]active[/green]",
            "pending": "[yellow]pending[/yellow]",
            "inactive": "[red]inactive[/red]",
        }.get(node.status.value, node.status.value)

        roles_str = ", ".join(r.value for r in node.roles)

        table.add_row(
            node.name,
            node.vpn_ip or node.internal_ip,
            status_style,
            roles_str or "[dim]none[/dim]",
            node.region or "[dim]unknown[/dim]",
        )

    console.print(table)


@app.command("info")
def node_info(
    node_name: Annotated[str, typer.Argument(help="Name of the node")],
    manifest_path: Annotated[
        Optional[Path],
        typer.Option("--manifest", "-m", help="Path to manifest file"),
    ] = None,
) -> None:
    """Show detailed information about a node."""
    if manifest_path is None:
        from redundanet.core.config import get_default_manifest_path

        manifest_path = get_default_manifest_path()

    if not manifest_path.exists():
        console.print(f"[red]Error:[/red] Manifest not found: {manifest_path}")
        raise typer.Exit(1)

    manifest = Manifest.from_file(manifest_path)
    node = manifest.get_node(node_name)

    if node is None:
        console.print(f"[red]Error:[/red] Node '{node_name}' not found")
        raise typer.Exit(1)

    # Display node info
    table = Table(title=f"Node: {node.name}", show_header=False, box=None)
    table.add_column("Property", style="cyan")
    table.add_column("Value")

    table.add_row("Name", node.name)
    table.add_row("Internal IP", node.internal_ip)
    table.add_row("VPN IP", node.vpn_ip or "[dim]same as internal[/dim]")
    table.add_row("Public IP", node.public_ip or "[dim]not set[/dim]")
    table.add_row("Status", node.status.value)
    table.add_row("Region", node.region or "[dim]unknown[/dim]")
    table.add_row("GPG Key ID", node.gpg_key_id or "[dim]not set[/dim]")
    table.add_row("Publicly Accessible", "Yes" if node.is_publicly_accessible else "No")
    table.add_row("Roles", ", ".join(r.value for r in node.roles) or "[dim]none[/dim]")
    table.add_row("Storage Contribution", node.storage_contribution or "[dim]none[/dim]")
    table.add_row("Storage Allocation", node.storage_allocation or "[dim]none[/dim]")

    # Ports
    table.add_row("", "")
    table.add_row("[bold]Ports[/bold]", "")
    table.add_row("  Tinc", str(node.ports.tinc))
    table.add_row("  Tahoe Storage", str(node.ports.tahoe_storage))
    table.add_row("  Tahoe Client", str(node.ports.tahoe_client))
    table.add_row("  Tahoe Introducer", str(node.ports.tahoe_introducer))

    console.print(table)


@app.command("add")
def add_node(
    name: Annotated[str, typer.Option("--name", "-n", prompt=True, help="Node name")],
    internal_ip: Annotated[
        str,
        typer.Option("--ip", "-i", prompt=True, help="Internal/VPN IP address"),
    ],
    public_ip: Annotated[
        Optional[str],
        typer.Option("--public-ip", "-p", help="Public IP address"),
    ] = None,
    gpg_key_id: Annotated[
        Optional[str],
        typer.Option("--gpg-key", "-g", help="GPG key ID"),
    ] = None,
    region: Annotated[
        Optional[str],
        typer.Option("--region", "-r", help="Geographic region"),
    ] = None,
    roles: Annotated[
        Optional[list[str]],
        typer.Option("--role", help="Node roles (can be specified multiple times)"),
    ] = None,
    storage: Annotated[
        Optional[str],
        typer.Option("--storage", "-s", help="Storage contribution"),
    ] = None,
    publicly_accessible: Annotated[
        bool,
        typer.Option("--public", help="Node is publicly accessible"),
    ] = False,
    manifest_path: Annotated[
        Optional[Path],
        typer.Option("--manifest", "-m", help="Path to manifest file"),
    ] = None,
) -> None:
    """Add a new node to the manifest."""
    from redundanet.core.config import NodeConfig, PortConfig

    if manifest_path is None:
        from redundanet.core.config import get_default_manifest_path

        manifest_path = get_default_manifest_path()

    if not manifest_path.exists():
        console.print(f"[red]Error:[/red] Manifest not found: {manifest_path}")
        console.print("Create a new manifest first or specify a valid path.")
        raise typer.Exit(1)

    manifest = Manifest.from_file(manifest_path)

    # Check if node already exists
    if manifest.get_node(name):
        console.print(f"[red]Error:[/red] Node '{name}' already exists")
        raise typer.Exit(1)

    # Parse roles
    node_roles = []
    if roles:
        for role in roles:
            try:
                node_roles.append(NodeRole(role))
            except ValueError:
                valid_roles = [r.value for r in NodeRole]
                console.print(f"[red]Error:[/red] Invalid role '{role}'")
                console.print(f"Valid roles: {', '.join(valid_roles)}")
                raise typer.Exit(1) from None

    # Create node config
    node = NodeConfig(
        name=name,
        internal_ip=internal_ip,
        vpn_ip=internal_ip,
        public_ip=public_ip,
        gpg_key_id=gpg_key_id,
        region=region,
        status=NodeStatus.PENDING,
        roles=node_roles,
        ports=PortConfig(),
        storage_contribution=storage,
        is_publicly_accessible=publicly_accessible,
    )

    # Add to manifest
    manifest.add_node(node)
    manifest.save(manifest_path)

    console.print(f"[green]Added node '{name}' to manifest[/green]")
    console.print(f"Manifest saved to: {manifest_path}")


@app.command("remove")
def remove_node(
    node_name: Annotated[str, typer.Argument(help="Name of the node to remove")],
    manifest_path: Annotated[
        Optional[Path],
        typer.Option("--manifest", "-m", help="Path to manifest file"),
    ] = None,
    force: Annotated[
        bool,
        typer.Option("--force", "-f", help="Skip confirmation"),
    ] = False,
) -> None:
    """Remove a node from the manifest."""
    if manifest_path is None:
        from redundanet.core.config import get_default_manifest_path

        manifest_path = get_default_manifest_path()

    if not manifest_path.exists():
        console.print(f"[red]Error:[/red] Manifest not found: {manifest_path}")
        raise typer.Exit(1)

    manifest = Manifest.from_file(manifest_path)

    if not manifest.get_node(node_name):
        console.print(f"[red]Error:[/red] Node '{node_name}' not found")
        raise typer.Exit(1)

    if not force:
        confirm = typer.confirm(f"Are you sure you want to remove node '{node_name}'?")
        if not confirm:
            console.print("Aborted.")
            raise typer.Exit(0)

    manifest.remove_node(node_name)
    manifest.save(manifest_path)

    console.print(f"[green]Removed node '{node_name}' from manifest[/green]")


@app.command("keys")
def manage_keys(
    action: Annotated[
        str,
        typer.Argument(help="Action: generate, export, import, list, publish, fetch"),
    ],
    node_name: Annotated[
        Optional[str],
        typer.Option("--name", "-n", help="Node name"),
    ] = None,
    email: Annotated[
        Optional[str],
        typer.Option("--email", "-e", help="Email address for the GPG key"),
    ] = None,
    key_id: Annotated[
        Optional[str],
        typer.Option("--key-id", "-k", help="Key ID for export/import operations"),
    ] = None,
    output_file: Annotated[
        Optional[Path],
        typer.Option("--output", "-o", help="Output file for export"),
    ] = None,
    input_file: Annotated[
        Optional[Path],
        typer.Option("--input", "-i", help="Input file for import"),
    ] = None,
) -> None:
    """Manage GPG keys for node authentication."""
    from redundanet.auth.gpg import GPGManager
    from redundanet.core.config import load_settings
    from redundanet.core.exceptions import GPGError

    settings = load_settings()
    node_name = node_name or settings.node_name

    if action == "generate":
        if not node_name:
            console.print("[red]Error:[/red] No node name specified")
            console.print("Use --name or set REDUNDANET_NODE_NAME")
            raise typer.Exit(1)

        # Prompt for email if not provided
        if not email:
            email = typer.prompt("Enter email address for the GPG key")

        console.print(f"[bold]Generating GPG key for node: {node_name}[/bold]")
        console.print("[yellow]Note:[/yellow] This will create a new GPG keypair.")

        try:
            gpg = GPGManager(node_name=node_name)
            with console.status("[bold green]Generating GPG key (this may take a moment)..."):
                key_info = gpg.generate_key(
                    name=f"RedundaNet Node {node_name}",
                    email=email,
                )

            console.print("\n[bold green]GPG key generated successfully![/bold green]")
            console.print(f"  Key ID:      [cyan]{key_info.key_id}[/cyan]")
            console.print(f"  Fingerprint: [dim]{key_info.fingerprint}[/dim]")
            console.print(f"  User ID:     {key_info.user_id}")

            console.print("\n[bold]Next steps:[/bold]")
            console.print(
                f"1. Publish your key to keyservers: [cyan]redundanet node keys publish --key-id {key_info.key_id}[/cyan]"
            )
            console.print(
                "2. Submit your application at: [cyan]https://redundanet.com/join.html[/cyan]"
            )
            console.print(f"3. Use this Key ID in your application: [cyan]{key_info.key_id}[/cyan]")

        except GPGError as e:
            console.print(f"[red]Error generating key:[/red] {e}")
            raise typer.Exit(1) from None

    elif action == "export":
        if not key_id:
            console.print("[red]Error:[/red] --key-id is required for export")
            raise typer.Exit(1)

        console.print(f"[bold]Exporting public key: {key_id}[/bold]")

        try:
            gpg = GPGManager()
            public_key = gpg.export_public_key(key_id)

            if output_file:
                output_file.write_text(public_key)
                console.print(f"[green]Public key exported to:[/green] {output_file}")
            else:
                console.print("\n[bold]Public Key:[/bold]")
                console.print(public_key)

        except GPGError as e:
            console.print(f"[red]Error exporting key:[/red] {e}")
            raise typer.Exit(1) from None

    elif action == "import":
        if not input_file:
            console.print("[red]Error:[/red] --input is required for import")
            console.print("Provide a path to an ASCII-armored public key file")
            raise typer.Exit(1)

        if not input_file.exists():
            console.print(f"[red]Error:[/red] File not found: {input_file}")
            raise typer.Exit(1)

        console.print(f"[bold]Importing public key from: {input_file}[/bold]")

        try:
            gpg = GPGManager()
            key_data = input_file.read_text()
            key_info = gpg.import_key(key_data)

            console.print("[green]Key imported successfully![/green]")
            console.print(f"  Key ID:      [cyan]{key_info.key_id}[/cyan]")
            console.print(f"  Fingerprint: [dim]{key_info.fingerprint}[/dim]")
            console.print(f"  User ID:     {key_info.user_id}")

        except GPGError as e:
            console.print(f"[red]Error importing key:[/red] {e}")
            raise typer.Exit(1) from None

    elif action == "list":
        console.print("[bold]GPG Keys in Keyring:[/bold]")

        try:
            gpg = GPGManager()
            keys = gpg.list_keys()

            if not keys:
                console.print("[dim]No keys found in keyring[/dim]")
                return

            table = Table()
            table.add_column("Key ID", style="cyan")
            table.add_column("User ID")
            table.add_column("Created")
            table.add_column("Expires")

            for key in keys:
                table.add_row(
                    key.key_id,
                    key.user_id,
                    key.created or "[dim]unknown[/dim]",
                    key.expires or "[dim]never[/dim]",
                )

            console.print(table)

        except GPGError as e:
            console.print(f"[red]Error listing keys:[/red] {e}")
            raise typer.Exit(1) from None

    elif action == "publish":
        if not key_id:
            console.print("[red]Error:[/red] --key-id is required for publish")
            console.print("Use 'redundanet node keys list' to see your keys")
            raise typer.Exit(1)

        console.print(f"[bold]Publishing key to keyservers: {key_id}[/bold]")

        try:
            from redundanet.auth.keyserver import KeyServerClient
            from redundanet.core.exceptions import KeyServerError

            gpg = GPGManager()
            keyserver_client = KeyServerClient(gpg)

            with console.status("[bold green]Uploading to keyservers..."):
                success = keyserver_client.upload_key(key_id)

            if success:
                console.print("[green]Key published successfully![/green]")
                console.print("\nYour key is now available on public keyservers.")
                console.print("Note: It may take a few minutes for the key to propagate.")
                console.print("\n[bold]Keyservers used:[/bold]")
                for server in keyserver_client.keyservers:
                    console.print(f"  - {server}")
            else:
                console.print("[yellow]Warning:[/yellow] Failed to upload to any keyserver")
                console.print("You may need to manually upload your key at:")
                console.print("  - https://keys.openpgp.org/upload")
                console.print("  - https://keyserver.ubuntu.com/")

        except (GPGError, KeyServerError) as e:
            console.print(f"[red]Error publishing key:[/red] {e}")
            raise typer.Exit(1) from None

    elif action == "fetch":
        if not key_id:
            console.print("[red]Error:[/red] --key-id is required for fetch")
            raise typer.Exit(1)

        console.print(f"[bold]Fetching key from keyservers: {key_id}[/bold]")

        try:
            from redundanet.auth.keyserver import KeyServerClient

            gpg = GPGManager()
            keyserver_client = KeyServerClient(gpg)

            with console.status("[bold green]Searching keyservers..."):
                success = keyserver_client.import_key_from_server(key_id)

            if success:
                console.print("[green]Key fetched and imported successfully![/green]")

                # Show key info
                fetched_key = gpg.get_key(key_id)
                if fetched_key:
                    console.print(f"  Key ID:  [cyan]{fetched_key.key_id}[/cyan]")
                    console.print(f"  User ID: {fetched_key.user_id}")
            else:
                console.print(f"[red]Error:[/red] Key {key_id} not found on any keyserver")
                raise typer.Exit(1)

        except GPGError as e:
            console.print(f"[red]Error fetching key:[/red] {e}")
            raise typer.Exit(1) from None

    else:
        console.print(f"[red]Error:[/red] Unknown action '{action}'")
        console.print("Valid actions: generate, export, import, list, publish, fetch")
        raise typer.Exit(1)
