"""Tests for the init -> sync flow: init persists config, sync finds the manifest."""

from pathlib import Path

from typer.testing import CliRunner

import redundanet.cli.main as main_mod
from redundanet.cli.main import app
from redundanet.core.config import load_settings
from redundanet.utils.process import CommandResult

runner = CliRunner()

_MANIFEST = """\
network:
  name: test-net
  version: "1.0.0"
  domain: test.local
  vpn_network: 10.100.0.0/16
nodes:
  - name: node-1
    internal_ip: 10.100.0.10
    gpg_key_id: ABCD1234
"""


def test_init_persists_config_that_load_settings_reads(tmp_path, monkeypatch):
    cfg = tmp_path / "cfg"
    monkeypatch.setenv("REDUNDANET_CONFIG_DIR", str(cfg))
    monkeypatch.setenv("REDUNDANET_DATA_DIR", str(tmp_path / "data"))
    monkeypatch.delenv("REDUNDANET_MANIFEST_REPO", raising=False)
    monkeypatch.delenv("REDUNDANET_NODE_NAME", raising=False)

    result = runner.invoke(
        app,
        [
            "init",
            "--name",
            "node-abc",
            "--network",
            "redundanet",
            "--storage",
            "1TB",
            "--manifest-repo",
            "https://example.com/r.git",
        ],
    )
    assert result.exit_code == 0, result.output
    assert (cfg / ".env").exists()

    # A fresh settings load (as `sync` does) must now see the persisted values.
    settings = load_settings()
    assert settings.manifest_repo == "https://example.com/r.git"
    assert settings.node_name == "node-abc"


def test_init_falls_back_to_user_dirs_without_root(tmp_path, monkeypatch):
    """On hosts where /etc and /var/lib are not writable (macOS, non-sudo
    Linux), init must fall back to per-user dirs instead of crashing."""
    read_only = tmp_path / "ro"
    read_only.mkdir()
    read_only.chmod(0o500)
    monkeypatch.setenv("REDUNDANET_CONFIG_DIR", str(read_only / "etc" / "redundanet"))
    monkeypatch.setenv("REDUNDANET_DATA_DIR", str(read_only / "var" / "redundanet"))
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.delenv("REDUNDANET_MANIFEST_REPO", raising=False)
    monkeypatch.delenv("REDUNDANET_NODE_NAME", raising=False)

    try:
        result = runner.invoke(
            app,
            [
                "init",
                "--name",
                "node-user",
                "--network",
                "redundanet",
                "--storage",
                "1TB",
                "--manifest-repo",
                "https://example.com/r.git",
            ],
        )
        assert result.exit_code == 0, result.output
        assert "per-user directories" in result.output

        env_file = tmp_path / "home" / ".config" / "redundanet" / ".env"
        assert env_file.exists()
        content = env_file.read_text()
        assert "REDUNDANET_NODE_NAME=node-user" in content
        # The fallback data dir is recorded so later commands agree on it.
        assert "REDUNDANET_DATA_DIR=" in content

        # A fresh settings load (system config absent) must pick up the
        # per-user config transparently.
        monkeypatch.setenv("REDUNDANET_CONFIG_DIR", str(tmp_path / "nonexistent"))
        monkeypatch.delenv("REDUNDANET_DATA_DIR", raising=False)
        settings = load_settings()
        assert settings.node_name == "node-user"
        assert settings.data_dir == tmp_path / "home" / ".local" / "share" / "redundanet"
        assert (settings.data_dir / "manifest").is_dir()
    finally:
        read_only.chmod(0o700)  # let pytest clean the tmp dir


def test_sync_copies_manifest_out_of_repo_subdir(tmp_path, monkeypatch):
    data = tmp_path / "data"
    monkeypatch.setenv("REDUNDANET_CONFIG_DIR", str(tmp_path / "cfg"))
    monkeypatch.setenv("REDUNDANET_DATA_DIR", str(data))
    monkeypatch.setenv("REDUNDANET_MANIFEST_REPO", "https://example.com/r.git")

    def fake_git_sync(repo: str, branch: str, target: Path) -> CommandResult:
        # Simulate cloning a repo whose manifest lives under manifests/.
        manifests = Path(target) / "manifests"
        manifests.mkdir(parents=True, exist_ok=True)
        (manifests / "manifest.yaml").write_text(_MANIFEST)
        return CommandResult(0, "", "", "git")

    monkeypatch.setattr(main_mod, "git_sync", fake_git_sync)

    result = runner.invoke(app, ["sync"])
    assert result.exit_code == 0, result.output
    # The manifest must land where the rest of the CLI looks for it.
    assert (data / "manifest" / "manifest.yaml").exists()
    assert "Nodes in manifest" in result.output
