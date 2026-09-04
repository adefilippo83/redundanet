"""Unit tests for the `redundanet update` command."""

from __future__ import annotations

import pytest
from typer.testing import CliRunner

from redundanet.cli.main import app
from redundanet.core.deployment import ServiceStatus

runner = CliRunner()


class FakeDeployment:
    """Deployment stand-in for the update command."""

    def __init__(
        self,
        changed,
        running=("tinc", "tahoe-storage", "tahoe-client"),
        health_results=None,
    ):
        self._changed = list(changed)  # services pending_image_changes should report
        self._running = list(running)
        # Sequence of wait_healthy() return values; the last value repeats.
        self._health = list(health_results) if health_results is not None else [True]
        self.pulled = False
        self.recreated: list[str] | None = None
        self.rolled_back: tuple[dict, list[str]] | None = None
        self.recreate_calls = 0
        # No compose file / env file in tests: the real _refresh_compose_file
        # then no-ops (no repo clone), leaving image-only behavior unchanged.
        self.compose_file = None
        self.env_file = None

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
        self.recreate_calls += 1
        return CommandResult(0, "", "", "")

    def current_images(self, services):
        return {s: (f"ref:{s}", f"id:{s}") for s in services}

    def wait_healthy(self, services, timeout=120):
        value = self._health[0]
        if len(self._health) > 1:
            self._health.pop(0)
        return value

    def rollback(self, images, services):
        from redundanet.utils.process import CommandResult

        self.rolled_back = (images, services)
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
        assert dep.rolled_back is None  # healthy -> no rollback

    def test_unhealthy_rolls_back_and_recovers(self, patch_deployment):
        # Unhealthy after the new images, healthy again after rollback.
        dep = patch_deployment["install"](
            FakeDeployment(changed=["tinc"], health_results=[False, True])
        )
        result = runner.invoke(app, ["update", "--yes"])
        # A bad update is a failure even though we recovered -> non-zero exit.
        assert result.exit_code == 1
        assert "did not become healthy" in result.output
        assert "Rolling back" in result.output
        assert "healthy again" in result.output
        # Rolled back to the pre-update image of the changed service, all services.
        images, services = dep.rolled_back
        assert images == {"tinc": ("ref:tinc", "id:tinc")}
        assert services == ["tinc", "tahoe-storage", "tahoe-client"]

    def test_unhealthy_no_rollback_flag_leaves_new_images(self, patch_deployment):
        dep = patch_deployment["install"](FakeDeployment(changed=["tinc"], health_results=[False]))
        result = runner.invoke(app, ["update", "--yes", "--no-rollback"])
        assert result.exit_code == 1
        assert "Left on the new images" in result.output
        assert dep.rolled_back is None

    def test_unhealthy_rollback_does_not_recover(self, patch_deployment):
        dep = patch_deployment["install"](
            FakeDeployment(changed=["tinc"], health_results=[False, False])
        )
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 1
        assert "Rollback did not restore health" in result.output
        assert dep.rolled_back is not None

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


class TestUpdateComposeRefresh:
    """The compose-file refresh added to `redundanet update`."""

    def test_compose_change_alone_triggers_recreate(self, patch_deployment, monkeypatch):
        # No image changed, but the compose file did: still recreate (tinc first).
        dep = patch_deployment["install"](FakeDeployment(changed=[]))
        monkeypatch.setattr(
            "redundanet.cli.main._refresh_compose_file", lambda *_a, **_k: (True, b"old-bytes")
        )
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 0
        assert "Compose file update applied" in result.output
        assert dep.recreated == ["tinc", "tahoe-storage", "tahoe-client"]
        assert dep.rolled_back is None

    def test_no_image_no_compose_change_is_up_to_date(self, patch_deployment, monkeypatch):
        dep = patch_deployment["install"](FakeDeployment(changed=[]))
        monkeypatch.setattr(
            "redundanet.cli.main._refresh_compose_file", lambda *_a, **_k: (False, None)
        )
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 0
        assert "Already up to date" in result.output
        assert dep.recreated is None

    def test_no_compose_refresh_flag_skips_refresh(self, patch_deployment, monkeypatch):
        dep = patch_deployment["install"](FakeDeployment(changed=[]))
        called = {"n": 0}

        def spy(*a, **k):
            called["n"] += 1
            return (True, None)

        monkeypatch.setattr("redundanet.cli.main._refresh_compose_file", spy)
        result = runner.invoke(app, ["update", "--yes", "--no-compose-refresh"])
        assert result.exit_code == 0
        assert called["n"] == 0  # refresh not attempted
        assert "Already up to date" in result.output
        assert dep.recreated is None

    def test_check_reports_available_compose_update(self, patch_deployment, monkeypatch):
        dep = patch_deployment["install"](FakeDeployment(changed=[], running=["tinc"]))
        monkeypatch.setattr(
            "redundanet.cli.main._refresh_compose_file", lambda *_a, **_k: (True, None)
        )
        result = runner.invoke(app, ["update", "--check"])
        assert result.exit_code == 0
        assert "Compose file update available" in result.output
        assert "not recreating" in result.output
        assert dep.recreated is None

    def test_bad_compose_change_rolls_back_compose_and_recovers(
        self, patch_deployment, monkeypatch, tmp_path
    ):
        # Compose changed (no image change); node unhealthy after, healthy after rollback.
        compose = tmp_path / "docker-compose.yml"
        compose.write_bytes(b"new-broken-bytes")
        dep = patch_deployment["install"](FakeDeployment(changed=[], health_results=[False, True]))
        dep.compose_file = compose
        monkeypatch.setattr(
            "redundanet.cli.main._refresh_compose_file", lambda *_a, **_k: (True, b"old-good-bytes")
        )
        result = runner.invoke(app, ["update", "--yes"])
        assert result.exit_code == 1
        assert "Restored the previous compose file" in result.output
        assert "healthy again" in result.output
        # The previous compose bytes were written back before the rollback recreate.
        assert compose.read_bytes() == b"old-good-bytes"
        # Rollback recreate ran even though there were no image changes to retag.
        assert dep.rolled_back == ({}, ["tinc", "tahoe-storage", "tahoe-client"])
