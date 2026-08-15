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


def test_base_includes_override_when_present(tmp_path):
    """A CLI-driven recreate MUST keep docker-compose.override.yml — passing -f
    disables Docker's auto-load, and dropping it detaches a storage node's data
    disk (the bind-mount lives in the override)."""
    settings = make_settings(tmp_path)
    override = settings.compose_file.parent / "docker-compose.override.yml"
    override.write_text("services: {}\n")
    dep = Deployment(settings)
    base = dep._base()
    # Both the main file and the override are passed with -f.
    assert base.count("-f") == 2
    assert str(override) in base


def test_base_no_override_when_absent(tmp_path):
    settings = make_settings(tmp_path)
    dep = Deployment(settings)
    assert dep._base().count("-f") == 1


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


class FakeCompose:
    """Deployment.compose stand-in scripted by subcommand."""

    def __init__(self, responses: dict) -> None:
        self.responses = responses
        self.calls: list[tuple[str, ...]] = []

    def __call__(self, *args, **kwargs):
        self.calls.append(args)
        for key, result in self.responses.items():
            if args[: len(key)] == key:
                return result() if callable(result) else result
        return CommandResult(0, "", "", "")


def test_service_containers_maps_names(tmp_path):
    dep = Deployment(make_settings(tmp_path))
    payload = (
        '{"Service":"tinc","Name":"redundanet-tinc"}\n'
        '{"Service":"tahoe-storage","Name":"redundanet-tahoe-storage"}'
    )
    dep.compose = FakeCompose({("ps",): CommandResult(0, payload, "", "")})  # type: ignore[assignment]
    assert dep._service_containers() == {
        "tinc": "redundanet-tinc",
        "tahoe-storage": "redundanet-tahoe-storage",
    }


def test_pending_image_changes_detects_pulled_image(tmp_path, monkeypatch):
    """The running container's image differs from what the reference it was
    created from now resolves to (post-pull) — so the service is reported
    changed. Uses `docker inspect`, never `docker compose images` (which
    returned empty on compose v5.4 and silently broke detection)."""
    dep = Deployment(make_settings(tmp_path))
    ps = '{"Service":"tinc","Name":"c-tinc"}\n{"Service":"tahoe-storage","Name":"c-storage"}'
    dep.compose = FakeCompose({("ps",): CommandResult(0, ps, "", "")})  # type: ignore[assignment]

    # tinc container runs 'old' but its ref now resolves to 'new'; storage is
    # unchanged ('same' == 'same').
    inspects = {
        ("c-tinc", "{{.Image}}"): "old",
        ("c-tinc", "{{.Config.Image}}"): "repo/tinc:main",
        ("c-storage", "{{.Image}}"): "same",
        ("c-storage", "{{.Config.Image}}"): "repo/storage:main",
        ("repo/tinc:main", "{{.Id}}"): "new",
        ("repo/storage:main", "{{.Id}}"): "same",
    }
    monkeypatch.setattr(dep, "_inspect", lambda target, fmt: inspects[(target, fmt)])

    assert dep.pending_image_changes(["tinc", "tahoe-storage"]) == ["tinc"]


def test_recreate_forces_and_names_services(tmp_path):
    dep = Deployment(make_settings(tmp_path))
    fake = FakeCompose({})
    dep.compose = fake  # type: ignore[assignment]
    dep.recreate(["tinc", "tahoe-storage", "tahoe-client"])
    call = fake.calls[-1]
    assert call[:3] == ("up", "-d", "--force-recreate")
    # tinc named first so it is recreated before the tahoe services (netns).
    assert call[3] == "tinc"
    assert "tahoe-storage" in call and "tahoe-client" in call


def test_git_sync_initializes_in_place_never_clones(tmp_path, monkeypatch):
    """git_sync must work in a NON-EMPTY dir (the shared manifest volume can
    already hold e.g. introducer.furl) — `git clone` would refuse it."""
    target = tmp_path / "manifest"
    target.mkdir()
    (target / "introducer.furl").write_text("pb://x")  # concurrent writer won the race
    calls: list[list[str]] = []

    def fake(command, **kwargs):
        calls.append(list(command))
        return CommandResult(0, "", "", "")

    monkeypatch.setattr("redundanet.core.deployment.run_command", fake)
    result = git_sync("https://example.com/repo.git", "main", target)

    assert result.success
    assert not any("clone" in c for c in calls)
    assert [*["git", "-C", str(target)], "init", "-q"] in calls
    assert any("fetch" in c and "main" in c for c in calls)
    assert calls[-1][-2:] == ["--hard", "FETCH_HEAD"]


def test_git_sync_existing_repo_skips_init(tmp_path, monkeypatch):
    target = tmp_path / "manifest"
    (target / ".git").mkdir(parents=True)
    calls: list[list[str]] = []

    def fake(command, **kwargs):
        calls.append(list(command))
        return CommandResult(0, "", "", "")

    monkeypatch.setattr("redundanet.core.deployment.run_command", fake)
    git_sync("repo", "develop", target)
    assert not any("init" in c for c in calls)
    assert any("fetch" in c and "develop" in c for c in calls)
    assert calls[-1][-2:] == ["--hard", "FETCH_HEAD"]


def test_git_sync_real_end_to_end(tmp_path):
    """Real git: sync from a local origin into a dir pre-polluted with an
    untracked file; the file must survive and the checkout must appear."""
    origin = tmp_path / "origin"
    origin.mkdir()
    run = __import__("subprocess").run
    run(["git", "init", "-q", "-b", "main", str(origin)], check=True)
    (origin / "manifests").mkdir()
    (origin / "manifests" / "manifest.yaml").write_text("network: {}\n")
    run(["git", "-C", str(origin), "add", "-A"], check=True)
    run(
        [
            "git",
            "-C",
            str(origin),
            "-c",
            "user.email=t@t",
            "-c",
            "user.name=t",
            "commit",
            "-q",
            "-m",
            "init",
        ],
        check=True,
    )

    target = tmp_path / "manifest"
    target.mkdir()
    (target / "introducer.furl").write_text("pb://x")

    result = git_sync(str(origin), "main", target)
    assert result.success, result.stderr
    assert (target / "manifests" / "manifest.yaml").exists()
    assert (target / "introducer.furl").read_text() == "pb://x"  # untracked survives

    # Second sync (repo now exists) also works.
    assert git_sync(str(origin), "main", target).success
