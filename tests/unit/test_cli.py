"""Unit tests for the CLI commands (real assertions against real behavior)."""

from pathlib import Path

from typer.testing import CliRunner

from redundanet import __version__
from redundanet.cli.main import app

runner = CliRunner()


class TestMainCLI:
    def test_help_lists_subcommands(self):
        result = runner.invoke(app, ["--help"])
        assert result.exit_code == 0
        for command in ("init", "status", "sync", "validate", "node", "network", "storage"):
            assert command in result.output

    def test_version_flag(self):
        result = runner.invoke(app, ["--version"])
        assert result.exit_code == 0
        assert __version__ in result.output


class TestValidateCommand:
    def test_valid_manifest_passes(self, sample_manifest_file: Path):
        result = runner.invoke(app, ["validate", str(sample_manifest_file)])
        assert result.exit_code == 0
        assert "test-network" in result.output
        assert "Nodes:" in result.output

    def test_semantic_problems_are_reported_as_warnings(self, sample_manifest_file: Path):
        # 2 storage nodes < shares_happy=7 -> the validate() warning must surface.
        result = runner.invoke(app, ["validate", str(sample_manifest_file)])
        assert "Not enough storage nodes" in result.output

    def test_schema_invalid_manifest_fails(self, tmp_path: Path):
        bad = tmp_path / "bad.yaml"
        bad.write_text("network:\n  name: broken\nnodes: []\n")
        result = runner.invoke(app, ["validate", str(bad)])
        assert result.exit_code == 1
        assert "Validation failed" in result.output

    def test_missing_file_fails(self, tmp_path: Path):
        result = runner.invoke(app, ["validate", str(tmp_path / "nope.yaml")])
        assert result.exit_code == 1


class TestNodeCommands:
    def test_node_list_shows_manifest_nodes(self, sample_manifest_file: Path):
        result = runner.invoke(app, ["node", "list", "--manifest", str(sample_manifest_file)])
        assert result.exit_code == 0
        assert "node1" in result.output
        assert "node2" in result.output

    def test_node_list_role_filter(self, sample_manifest_file: Path):
        result = runner.invoke(
            app,
            [
                "node",
                "list",
                "--manifest",
                str(sample_manifest_file),
                "--role",
                "tahoe_introducer",
            ],
        )
        assert result.exit_code == 0
        assert "node1" in result.output
        assert "node2" not in result.output

    def test_node_list_missing_manifest_fails(self, tmp_path: Path):
        result = runner.invoke(app, ["node", "list", "--manifest", str(tmp_path / "no.yaml")])
        assert result.exit_code == 1

    def test_node_info_shows_details(self, sample_manifest_file: Path):
        result = runner.invoke(
            app, ["node", "info", "node1", "--manifest", str(sample_manifest_file)]
        )
        assert result.exit_code == 0
        assert "10.100.0.1" in result.output
        assert "tahoe_introducer" in result.output

    def test_node_info_unknown_node_fails(self, sample_manifest_file: Path):
        result = runner.invoke(
            app, ["node", "info", "ghost", "--manifest", str(sample_manifest_file)]
        )
        assert result.exit_code == 1
        assert "not found" in result.output

    def test_node_add_appends_to_manifest(self, sample_manifest_file: Path):
        result = runner.invoke(
            app,
            [
                "node",
                "add",
                "--name",
                "node3",
                "--ip",
                "10.100.0.3",
                "--role",
                "tahoe_storage",
                "--manifest",
                str(sample_manifest_file),
            ],
        )
        assert result.exit_code == 0, result.output
        listing = runner.invoke(app, ["node", "list", "--manifest", str(sample_manifest_file)])
        assert "node3" in listing.output

    def test_node_add_duplicate_name_fails(self, sample_manifest_file: Path):
        result = runner.invoke(
            app,
            [
                "node",
                "add",
                "--name",
                "node1",
                "--ip",
                "10.100.0.99",
                "--manifest",
                str(sample_manifest_file),
            ],
        )
        assert result.exit_code == 1
        assert "already exists" in result.output

    def test_node_remove(self, sample_manifest_file: Path):
        result = runner.invoke(
            app,
            ["node", "remove", "node2", "--force", "--manifest", str(sample_manifest_file)],
        )
        assert result.exit_code == 0
        listing = runner.invoke(app, ["node", "list", "--manifest", str(sample_manifest_file)])
        assert "node2" not in listing.output


class TestSubcommandHelp:
    def test_network_help(self):
        result = runner.invoke(app, ["network", "--help"])
        assert result.exit_code == 0
        for command in ("join", "leave", "peers", "vpn"):
            assert command in result.output

    def test_storage_help(self):
        result = runner.invoke(app, ["storage", "--help"])
        assert result.exit_code == 0
        for command in ("upload", "download", "status"):
            assert command in result.output
