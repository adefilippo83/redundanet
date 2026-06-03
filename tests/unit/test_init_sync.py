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
