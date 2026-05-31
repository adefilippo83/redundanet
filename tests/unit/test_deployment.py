"""Unit tests for the Deployment docker-compose wrapper."""

from pathlib import Path

from redundanet.core.config import AppSettings
from redundanet.core.deployment import Deployment, ServiceStatus, git_sync
from redundanet.utils.process import CommandResult


def make_settings(tmp_path: Path, **kw) -> AppSettings:
    """Build settings pointing at a throwaway compose file."""
    compose = tmp_path / "docker" / "docker-compose.yml"
    compose.parent.mkdir(parents=True, exist_ok=True)
    compose.write_text("services: {}\n")
    return AppSettings(compose_file=compose, compose_project="testproj", **kw)


class FakeRun:
    """Records calls and returns a fixed CommandResult."""

    def __init__(self, result: CommandResult) -> None:
        self.calls: list[list[str]] = []
        self.result = result

    def __call__(self, command, **kwargs):
        self.calls.append(list(command))
        return self.result


def test_base_command_has_project_and_file(tmp_path):
    settings = make_settings(tmp_path)
    dep = Deployment(settings)
    base = dep._base()
    assert base[:4] == ["docker", "compose", "-p", "testproj"]
    assert "-f" in base
    assert str(settings.compose_file) in base


def test_env_file_added_to_base(tmp_path):
    env = tmp_path / ".env"
    env.write_text("X=1\n")
    settings = make_settings(tmp_path, compose_env_file=env)
    dep = Deployment(settings)
    base = dep._base()
    assert "--env-file" in base
    assert str(env) in base


def test_compose_file_discovery_missing(tmp_path):
    # Point at a non-existent compose file; nothing else on disk -> not configured
    settings = AppSettings(compose_file=tmp_path / "nope.yml", data_dir=tmp_path)
    dep = Deployment(settings)
    assert dep.is_configured() is False


def test_ps_parses_ndjson(tmp_path, monkeypatch):
    settings = make_settings(tmp_path)
    dep = Deployment(settings)
    ndjson = (
        '{"Service":"tinc","State":"running","Health":"healthy"}\n'
        '{"Service":"tahoe-client","State":"exited","Health":""}'
    )
    fake = FakeRun(CommandResult(0, ndjson, "", "docker compose ps"))
    monkeypatch.setattr("redundanet.core.deployment.run_command", fake)

    statuses = dep.ps()
    assert ServiceStatus("tinc", "running", "healthy") in statuses
    assert any(s.name == "tahoe-client" and s.state == "exited" for s in statuses)


def test_exec_builds_command(tmp_path, monkeypatch):
    settings = make_settings(tmp_path)
    dep = Deployment(settings)
    fake = FakeRun(CommandResult(0, "", "", ""))
    monkeypatch.setattr("redundanet.core.deployment.run_command", fake)

    dep.exec("tahoe-client", ["tahoe", "-d", "/d", "put", "/f"])
    cmd = fake.calls[-1]
    assert cmd[:4] == ["docker", "compose", "-p", "testproj"]
    assert "exec" in cmd and "-T" in cmd and "tahoe-client" in cmd
    assert cmd[-5:] == ["tahoe", "-d", "/d", "put", "/f"]


def test_git_sync_clone(tmp_path, monkeypatch):
    calls: list[list[str]] = []

    def fake(command, **kwargs):
        calls.append(list(command))
        return CommandResult(0, "", "", "")

    monkeypatch.setattr("redundanet.core.deployment.run_command", fake)
    git_sync("https://example.com/repo.git", "main", tmp_path / "manifest")
    assert calls[-1][:2] == ["git", "clone"]
    assert "main" in calls[-1]


def test_git_sync_pull_existing(tmp_path, monkeypatch):
    target = tmp_path / "manifest"
    (target / ".git").mkdir(parents=True)
    calls: list[list[str]] = []

    def fake(command, **kwargs):
        calls.append(list(command))
        return CommandResult(0, "", "", "")

    monkeypatch.setattr("redundanet.core.deployment.run_command", fake)
    git_sync("repo", "develop", target)
    assert ["git", "-C", str(target), "fetch", "origin"] in calls
    assert any("reset" in c for c in calls)
