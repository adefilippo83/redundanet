#!/usr/bin/env python3
"""Validate the RedundaNet network manifest in a pull request.

Nodes are identified by a GPG key (published to public keyservers) and reuse
that key as their Tinc transport key, so there is no committed ``tinc/hosts/``
layout to check anymore. This validates ``manifests/manifest.yaml`` (or a path
passed as the first argument) against the network model:

  * required network fields and a valid VPN CIDR
  * every node has a name, an IP, and a valid GPG key id (its identity)
  * IPs are valid, inside the VPN network, and not shared across nodes
  * roles and status use known values
  * node names are unique

Exits non-zero (and lists the problems) if the manifest is invalid.
"""

from __future__ import annotations

import ipaddress
import sys

import yaml

VALID_ROLES = {"tinc_vpn", "tahoe_introducer", "tahoe_storage", "tahoe_client"}
VALID_STATUS = {"active", "pending", "inactive"}


def _is_gpg_key_id(value: str) -> bool:
    v = value.replace(" ", "").upper()
    return len(v) in (8, 16, 40) and all(c in "0123456789ABCDEF" for c in v)


def validate(manifest_path: str) -> list[str]:
    errors: list[str] = []

    try:
        with open(manifest_path) as f:
            manifest = yaml.safe_load(f)
    except FileNotFoundError:
        return [f"Manifest not found: {manifest_path}"]
    except yaml.YAMLError as e:
        return [f"Failed to parse {manifest_path}: {e}"]

    if not isinstance(manifest, dict):
        return [f"{manifest_path}: top-level document must be a mapping"]

    # --- network section ---
    network = manifest.get("network")
    if not isinstance(network, dict):
        errors.append("Missing or invalid 'network' section")
        network = {}
    for field in ("name", "version", "domain", "vpn_network"):
        if not network.get(field):
            errors.append(f"network.{field} is required")

    vpn_network = None
    if network.get("vpn_network"):
        try:
            vpn_network = ipaddress.ip_network(network["vpn_network"])
        except ValueError as e:
            errors.append(f"Invalid network.vpn_network: {e}")

    # --- nodes ---
    nodes = manifest.get("nodes")
    if not isinstance(nodes, list):
        errors.append("'nodes' must be a list")
        nodes = []

    names: dict[str, int] = {}
    ip_owners: dict[str, set[str]] = {}

    for idx, node in enumerate(nodes):
        where = node.get("name") if isinstance(node, dict) else f"#{idx}"
        if not isinstance(node, dict):
            errors.append(f"Node {where} is not a mapping")
            continue

        name = node.get("name")
        if not name:
            errors.append(f"Node {where} is missing 'name'")
        else:
            names[name] = names.get(name, 0) + 1

        # GPG key id is the node identity (used as the Tinc key, fetched from
        # keyservers), so every node must declare a valid one.
        gpg_key_id = node.get("gpg_key_id")
        if not gpg_key_id:
            errors.append(f"Node {name or where} is missing 'gpg_key_id'")
        elif not _is_gpg_key_id(str(gpg_key_id)):
            errors.append(
                f"Node {name or where}: invalid gpg_key_id '{gpg_key_id}' "
                "(expected an 8/16/40-character hex key id)"
            )

        # IPs: must be valid, inside the VPN network, and unique across nodes.
        node_ips: set[str] = set()
        if not node.get("internal_ip"):
            errors.append(f"Node {name or where} is missing 'internal_ip'")
        for field in ("internal_ip", "vpn_ip"):
            value = node.get(field)
            if not value:
                continue
            try:
                addr = ipaddress.ip_address(value)
            except ValueError:
                errors.append(f"Node {name or where}: invalid {field} '{value}'")
                continue
            node_ips.add(str(addr))
            if vpn_network is not None and addr not in vpn_network and field == "vpn_ip":
                errors.append(
                    f"Node {name or where}: vpn_ip {value} is outside {vpn_network}"
                )
        for ip in node_ips:
            ip_owners.setdefault(ip, set()).add(name or where)

        # roles / status
        for role in node.get("roles", []) or []:
            if role not in VALID_ROLES:
                errors.append(
                    f"Node {name or where}: invalid role '{role}' "
                    f"(valid: {', '.join(sorted(VALID_ROLES))})"
                )
        status = node.get("status")
        if status and status not in VALID_STATUS:
            errors.append(
                f"Node {name or where}: invalid status '{status}' "
                f"(valid: {', '.join(sorted(VALID_STATUS))})"
            )

    for name, count in names.items():
        if count > 1:
            errors.append(f"Duplicate node name: {name} ({count} times)")
    for ip, owners in ip_owners.items():
        if len(owners) > 1:
            errors.append(f"IP {ip} is used by multiple nodes: {', '.join(sorted(owners))}")

    return errors


def main() -> None:
    manifest_path = sys.argv[1] if len(sys.argv) > 1 else "manifests/manifest.yaml"
    errors = validate(manifest_path)
    if errors:
        print(f"❌ Manifest validation failed for {manifest_path}:")
        for error in errors:
            print(f"  - {error}")
        sys.exit(1)
    print(f"✅ {manifest_path} is valid")
    sys.exit(0)


if __name__ == "__main__":
    main()
