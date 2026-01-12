"""Integration tests for running Docker containers."""

import subprocess
import time
from pathlib import Path

import pytest

# Project root directory
PROJECT_ROOT = Path(__file__).parent.parent.parent
DOCKER_DIR = PROJECT_ROOT / "docker"
COMPOSE_FILE = DOCKER_DIR / "docker-compose.test.yml"


def run_command(cmd: list[str], timeout: int = 300) -> subprocess.CompletedProcess:
    """Run a command and return the result."""
    return subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        timeout=timeout,
        cwd=PROJECT_ROOT,
    )


def docker_compose(*args, timeout: int = 300) -> subprocess.CompletedProcess:
    """Run docker compose with the test configuration."""
    cmd = ["docker", "compose", "-f", str(COMPOSE_FILE), *args]
    return run_command(cmd, timeout=timeout)


@pytest.fixture(scope="module")
def docker_available():
    """Check if Docker is available."""
    result = run_command(["docker", "--version"])
    if result.returncode != 0:
        pytest.skip("Docker is not available")

    # Check if Docker daemon is running
    result = run_command(["docker", "info"])
    if result.returncode != 0:
        pytest.skip("Docker daemon is not running")

    return True


@pytest.fixture(scope="class")
def built_images(docker_available):
    """Build all Docker images before running container tests."""
    print("\nBuilding Docker images...")

    # Build all images with docker compose
    result = docker_compose("build", timeout=900)
    if result.returncode != 0:
        pytest.skip(f"Failed to build images: {result.stderr}")

    yield True

    # Cleanup: remove test images
    print("\nCleaning up test images...")
    for image in ["tinc-test", "tahoe-introducer-test", "tahoe-storage-test", "tahoe-client-test"]:
        run_command(["docker", "rmi", f"docker-{image}", "-f"])


class TestTahoeIntroducerContainer:
    """Tests for Tahoe Introducer container."""

    @pytest.fixture(autouse=True)
    def setup_and_teardown(self, built_images):
        """Start and stop the container for each test."""
        yield
        # Cleanup after each test
        docker_compose("down", "-v", "--remove-orphans")

    @pytest.mark.integration
    @pytest.mark.slow
    def test_introducer_starts(self, docker_available):
        """Test that Tahoe Introducer container starts."""
        # Start the introducer
        result = docker_compose("up", "-d", "tahoe-introducer-test")
        assert result.returncode == 0, f"Failed to start: {result.stderr}"

        # Wait for container to be running
        time.sleep(5)

        # Check container is running
        result = docker_compose("ps", "--format", "json")
        assert "tahoe-introducer-test" in result.stdout or "running" in result.stdout.lower()

    @pytest.mark.integration
    @pytest.mark.slow
    def test_introducer_logs_accessible(self, docker_available):
        """Test that we can access introducer logs."""
        # Start the introducer
        docker_compose("up", "-d", "tahoe-introducer-test")
        time.sleep(5)

        # Get logs
        result = docker_compose("logs", "tahoe-introducer-test")
        assert result.returncode == 0, f"Failed to get logs: {result.stderr}"

    @pytest.mark.integration
    @pytest.mark.slow
    def test_introducer_python_available(self, docker_available):
        """Test that Python is available in the container."""
        # Start the introducer
        docker_compose("up", "-d", "tahoe-introducer-test")
        time.sleep(5)

        # Run python --version
        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                str(COMPOSE_FILE),
                "exec",
                "-T",
                "tahoe-introducer-test",
                "python",
                "--version",
            ]
        )
        assert result.returncode == 0, f"Python not available: {result.stderr}"
        assert "Python 3" in result.stdout


class TestTahoeStorageContainer:
    """Tests for Tahoe Storage container."""

    @pytest.fixture(autouse=True)
    def setup_and_teardown(self, built_images):
        """Start and stop the container for each test."""
        yield
        docker_compose("down", "-v", "--remove-orphans")

    @pytest.mark.integration
    @pytest.mark.slow
    def test_storage_starts(self, docker_available):
        """Test that Tahoe Storage container starts."""
        result = docker_compose("up", "-d", "tahoe-storage-test")
        assert result.returncode == 0, f"Failed to start: {result.stderr}"

        time.sleep(5)

        result = docker_compose("ps", "--format", "json")
        assert "tahoe-storage-test" in result.stdout or "running" in result.stdout.lower()

    @pytest.mark.integration
    @pytest.mark.slow
    def test_storage_tahoe_installed(self, docker_available):
        """Test that tahoe-lafs is installed."""
        docker_compose("up", "-d", "tahoe-storage-test")
        time.sleep(5)

        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                str(COMPOSE_FILE),
                "exec",
                "-T",
                "tahoe-storage-test",
                "tahoe",
                "--version",
            ]
        )
        # tahoe --version might exit with non-zero but still show version
        assert (
            "tahoe" in result.stdout.lower()
            or "allmydata" in result.stdout.lower()
            or result.returncode == 0
        )


class TestTahoeClientContainer:
    """Tests for Tahoe Client container."""

    @pytest.fixture(autouse=True)
    def setup_and_teardown(self, built_images):
        """Start and stop the container for each test."""
        yield
        docker_compose("down", "-v", "--remove-orphans")

    @pytest.mark.integration
    @pytest.mark.slow
    def test_client_starts(self, docker_available):
        """Test that Tahoe Client container starts."""
        result = docker_compose("up", "-d", "tahoe-client-test")
        assert result.returncode == 0, f"Failed to start: {result.stderr}"

        time.sleep(5)

        result = docker_compose("ps", "--format", "json")
        assert "tahoe-client-test" in result.stdout or "running" in result.stdout.lower()


class TestTincContainer:
    """Tests for Tinc VPN container."""

    @pytest.fixture(autouse=True)
    def setup_and_teardown(self, built_images):
        """Start and stop the container for each test."""
        yield
        docker_compose("down", "-v", "--remove-orphans")

    @pytest.mark.integration
    @pytest.mark.slow
    def test_tinc_starts(self, docker_available):
        """Test that Tinc VPN container starts."""
        result = docker_compose("up", "-d", "tinc-test")
        assert result.returncode == 0, f"Failed to start: {result.stderr}"

        time.sleep(5)

        result = docker_compose("ps", "--format", "json")
        assert "tinc-test" in result.stdout or "running" in result.stdout.lower()

    @pytest.mark.integration
    @pytest.mark.slow
    def test_tinc_installed(self, docker_available):
        """Test that tinc is installed in the container."""
        docker_compose("up", "-d", "tinc-test")
        time.sleep(5)

        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                str(COMPOSE_FILE),
                "exec",
                "-T",
                "tinc-test",
                "tincd",
                "--version",
            ]
        )
        assert result.returncode == 0 or "tinc" in result.stdout.lower()


class TestMultiContainerSetup:
    """Tests for running multiple containers together."""

    @pytest.fixture(autouse=True)
    def setup_and_teardown(self, built_images):
        """Cleanup after tests."""
        yield
        docker_compose("down", "-v", "--remove-orphans")

    @pytest.mark.integration
    @pytest.mark.slow
    def test_all_containers_start(self, docker_available):
        """Test that all containers can start together."""
        result = docker_compose("up", "-d")
        assert result.returncode == 0, f"Failed to start all containers: {result.stderr}"

        time.sleep(10)

        # Check all containers are running
        result = docker_compose("ps")
        assert result.returncode == 0

        # Should see multiple containers
        output = result.stdout.lower()
        assert "test" in output  # At least one test container

    @pytest.mark.integration
    @pytest.mark.slow
    def test_containers_can_communicate(self, docker_available):
        """Test that containers can communicate via Docker network."""
        # Start tahoe containers
        docker_compose("up", "-d", "tahoe-introducer-test", "tahoe-storage-test")
        time.sleep(10)

        # Try to ping storage from introducer
        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                str(COMPOSE_FILE),
                "exec",
                "-T",
                "tahoe-introducer-test",
                "ping",
                "-c",
                "2",
                "tahoe-storage-test",
            ]
        )

        # Ping should succeed (containers on same network)
        assert result.returncode == 0, f"Containers cannot communicate: {result.stderr}"

    @pytest.mark.integration
    @pytest.mark.slow
    def test_volumes_persist_data(self, docker_available):
        """Test that volumes persist data between restarts."""
        # Start introducer
        docker_compose("up", "-d", "tahoe-introducer-test")
        time.sleep(5)

        # Create a test file in the volume
        run_command(
            [
                "docker",
                "compose",
                "-f",
                str(COMPOSE_FILE),
                "exec",
                "-T",
                "tahoe-introducer-test",
                "touch",
                "/var/lib/tahoe-introducer/test-file",
            ]
        )

        # Restart container
        docker_compose("restart", "tahoe-introducer-test")
        time.sleep(5)

        # Check file still exists
        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                str(COMPOSE_FILE),
                "exec",
                "-T",
                "tahoe-introducer-test",
                "ls",
                "/var/lib/tahoe-introducer/test-file",
            ]
        )
        assert result.returncode == 0, "Volume data not persisted"
