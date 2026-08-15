"""Unit tests for the `redundanet update` command."""

from __future__ import annotations

import pytest
from typer.testing import CliRunner

from redundanet.cli.main import app
from redundanet.core.deployment import ServiceStatus

runner = CliRunner()


class FakeDeployment:
    """Deployment stand-in for the update command."""

    def __init__(self, changed, running=("tinc", "tahoe-storage", "tahoe-client")):
        self._changed = list(changed)  # services pending_image_changes should report
        self._running = list(running)
        self.pulled = False
        self.recreated: list[str] | None = None

    def require(self):
        return None

    def running_services(self):
        return self._running

    def pending_image_changes(self, services):
        return [s for s in self._changed if s in services]

    def pull(self, services=None):
        from redundanet.utils.process import CommandResult

        self.pulled = True
        return CommandResult(0, "", "", "")

    def recreate(self, services):
        from redundanet.utils.process import CommandResult

        self.recreated = services
        return CommandResult(0, "", "", "")

    def ps(self):
        return [ServiceStatus(s, "running", "healthy") for s in self._running]


@pytest.fixture
def patch_deployment(monkeypatch):
    holder = {}

    def install(dep):
        holder["dep"] = dep
        monkeypatch.setattr("redundanet.cli.main.Deployment", lambda _settings: dep)
        return dep

    holder["install"] = install
    return holder


class TestUpdate:
    def test_no_change_reports_up_to_date(self, patch_deployment):
        dep = patch_deployment["install"](FakeDeployment(changed=[]))
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 0
        assert "Already up to date" in result.output
        assert dep.recreated is None

    def test_change_triggers_recreate_tinc_first(self, patch_deployment):
        dep = patch_deployment["install"](FakeDeployment(changed=["tinc"]))
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 0
        assert "Updated images available for: tinc" in result.output
        # Even though only tinc changed, ALL running services recreate (netns).
        assert dep.recreated == ["tinc", "tahoe-storage", "tahoe-client"]

    def test_check_mode_never_recreates(self, patch_deployment):
        dep = patch_deployment["install"](FakeDeployment(changed=["tinc"], running=["tinc"]))
        result = runner.invoke(app, ["update", "--check"])
        assert result.exit_code == 0
        assert "not recreating" in result.output
        assert dep.pulled is True  # pull still happens to detect changes
        assert dep.recreated is None

    def test_no_running_services_errors(self, patch_deployment):
        patch_deployment["install"](FakeDeployment(changed=[], running=[]))
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 1
        assert "No running services" in result.output
