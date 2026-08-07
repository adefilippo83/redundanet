"""Unit tests for the network-join helpers (env generation, manifest lookup)."""

from pathlib import Path

import yaml

from redundanet.cli.network import (
    _find_node_in_manifest,
    _generate_env_file,
    _load_manifest_dict,
)


def write_manifest(tmp_path: Path, data: dict) -> Path:
    path = tmp_path / "manifest.yaml"
    path.write_text(yaml.dump(data))
    return path


def parse_env(env_path: Path) -> dict[str, str]:
    values = {}
    for line in env_path.read_text().splitlines():
        if "=" in line and not line.startswith("#"):
            key, _, value = line.partition("=")
            values[key] = value
    return values


class TestGenerateEnvFile:
    def test_env_includes_manifest_tahoe_settings(self, tmp_path: Path):
        node = {
            "name": "node-a",
            "vpn_ip": "10.100.0.10",
            "gpg_key_id": "1234567890ABCDEF1234567890ABCDEF12345678",
            "ports": {"tinc": 656},
        }
        network = {
            "tahoe": {
                "shares_needed": 2,
                "shares_happy": 3,
                "shares_total": 4,
                "reserved_space": "5G",
            }
        }
        _generate_env_file(node, network, "https://example.com/r.git", "main", tmp_path)

        env = parse_env(tmp_path / ".env")
        assert env["NODE_NAME"] == "node-a"
        assert env["VPN_IP"] == "10.100.0.10"
        assert env["TINC_PORT"] == "656"
        # The manifest's encoding parameters must reach the containers — not
        # the compose defaults.
        assert env["SHARES_NEEDED"] == "2"
        assert env["SHARES_HAPPY"] == "3"
        assert env["SHARES_TOTAL"] == "4"
        assert env["RESERVED_SPACE"] == "5G"
        assert env["MANIFEST_REPO"] == "https://example.com/r.git"

    def test_defaults_when_manifest_omits_tahoe(self, tmp_path: Path):
        node = {"name": "node-a", "vpn_ip": "10.100.0.10"}
        _generate_env_file(node, {}, "repo", "main", tmp_path)

        env = parse_env(tmp_path / ".env")
        assert env["TINC_PORT"] == "655"
        assert env["SHARES_NEEDED"] == "3"
        assert env["SHARES_HAPPY"] == "7"
        assert env["SHARES_TOTAL"] == "10"
        assert env["RESERVED_SPACE"] == "1G"
        assert env["PUBLIC_IP"] == "auto"


class TestManifestLookup:
    def test_find_node(self, tmp_path: Path):
        path = write_manifest(
            tmp_path,
            {"nodes": [{"name": "node-a"}, {"name": "node-b", "vpn_ip": "10.100.0.11"}]},
        )
        assert _find_node_in_manifest(path, "node-b") == {
            "name": "node-b",
            "vpn_ip": "10.100.0.11",
        }
        assert _find_node_in_manifest(path, "ghost") is None

    def test_load_manifest_dict_tolerates_non_mapping(self, tmp_path: Path):
        path = tmp_path / "manifest.yaml"
        path.write_text("- a\n- b\n")
        assert _load_manifest_dict(path) == {}
