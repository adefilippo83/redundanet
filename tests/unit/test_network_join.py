"""Unit tests for the network-join helpers (env generation, manifest lookup)."""

from pathlib import Path

import pytest
import typer
import yaml

from redundanet.cli.network import (
    _find_node_in_manifest,
    _generate_env_file,
    _load_manifest_dict,
    _merge_env,
    _profiles_for_roles,
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


class TestRejoinPreservesLocalConfig:
    """A re-join must not clobber a working node's local .env or strand its key."""

    def test_operator_keys_survive_rejoin(self, tmp_path: Path):
        env_path = tmp_path / ".env"
        env_path.write_text(
            "NODE_NAME=old-name\n"
            "SFTP_ENABLED=true\n"
            "SFTP_BIND=0.0.0.0\n"
            "LOG_LEVEL=DEBUG\n"
            "GPG_KEY_ID=EXISTINGKEYFINGERPRINT0000000000000000AA\n"
        )
        node = {"name": "new-name", "vpn_ip": "10.100.0.10"}  # manifest carries no key
        _generate_env_file(node, {}, "repo", "main", tmp_path)

        env = parse_env(env_path)
        # Manifest-derived value updated...
        assert env["NODE_NAME"] == "new-name"
        # ...operator-set keys preserved (this is what wiped SFTP before)...
        assert env["SFTP_ENABLED"] == "true"
        assert env["SFTP_BIND"] == "0.0.0.0"
        assert env["LOG_LEVEL"] == "DEBUG"
        # ...and an existing key id is never blanked by a keyless manifest.
        assert env["GPG_KEY_ID"] == "EXISTINGKEYFINGERPRINT0000000000000000AA"

    def test_manifest_key_overrides_existing(self, tmp_path: Path):
        env_path = tmp_path / ".env"
        env_path.write_text("GPG_KEY_ID=OLD\n")
        node = {"name": "n", "gpg_key_id": "NEWFINGERPRINT111111111111111111111111AA"}
        _generate_env_file(node, {}, "repo", "main", tmp_path)
        assert parse_env(env_path)["GPG_KEY_ID"] == "NEWFINGERPRINT111111111111111111111111AA"

    def test_merge_appends_unknown_manifest_keys(self):
        merged = _merge_env("NODE_NAME=a\n", {"NODE_NAME": "b", "VPN_IP": "10.0.0.1"})
        lines = [line for line in merged.splitlines() if "=" in line and not line.startswith("#")]
        assert "NODE_NAME=b" in lines
        assert "VPN_IP=10.0.0.1" in lines


class TestProfilesForRoles:
    def test_maps_roles_to_compose_profiles(self):
        assert _profiles_for_roles(["tahoe_storage"]) == ["storage"]
        assert _profiles_for_roles(["tahoe_introducer"]) == ["introducer"]
        assert _profiles_for_roles(["tahoe_storage", "tahoe_client"]) == ["storage", "client"]

    def test_ignores_non_service_roles_and_dedupes(self):
        # tinc_vpn has no profile (it always runs); duplicates collapse.
        assert _profiles_for_roles(["tinc_vpn", "tahoe_storage", "tahoe_storage"]) == ["storage"]
        assert _profiles_for_roles([]) == []


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

    def test_broken_yaml_exits_cleanly_not_traceback(self, tmp_path: Path):
        """A bad hand-edit on the manifest repo must produce a clean CLI error
        (a fresh Pi hit a raw ScannerError traceback mid-join)."""
        path = tmp_path / "manifest.yaml"
        path.write_text("nodes:\n  - name: broken\n   bad: indent: here\n")
        with pytest.raises(typer.Exit):
            _load_manifest_dict(path)
        with pytest.raises(typer.Exit):
            _find_node_in_manifest(path, "any")


class TestComposeProjectName:
    def test_seeded_on_fresh_env(self, tmp_path: Path):
        _generate_env_file({"name": "n"}, {}, "repo", "main", tmp_path)
        assert parse_env(tmp_path / ".env")["COMPOSE_PROJECT_NAME"] == "redundanet"

    def test_appended_when_missing_on_rejoin(self, tmp_path: Path):
        (tmp_path / ".env").write_text("NODE_NAME=old\nSFTP_ENABLED=true\n")
        _generate_env_file({"name": "n"}, {}, "repo", "main", tmp_path)
        env = parse_env(tmp_path / ".env")
        assert env["COMPOSE_PROJECT_NAME"] == "redundanet"
        assert env["SFTP_ENABLED"] == "true"

    def test_custom_value_preserved(self, tmp_path: Path):
        (tmp_path / ".env").write_text("COMPOSE_PROJECT_NAME=customproj\n")
        _generate_env_file({"name": "n"}, {}, "repo", "main", tmp_path)
        assert parse_env(tmp_path / ".env")["COMPOSE_PROJECT_NAME"] == "customproj"
