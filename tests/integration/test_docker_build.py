"""Integration tests for Docker image building."""

import subprocess
from pathlib import Path

import pytest

# Project root directory
PROJECT_ROOT = Path(__file__).parent.parent.parent
DOCKER_DIR = PROJECT_ROOT / "docker"


def run_command(cmd: list[str], timeout: int = 300) -> subprocess.CompletedProcess:
    """Run a command and return the result."""
    return subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        timeout=timeout,
        cwd=PROJECT_ROOT,
    )


@pytest.fixture(scope="module")
def docker_available():
    """Check if Docker is available."""
    result = run_command(["docker", "--version"])
    if result.returncode != 0:
        pytest.skip("Docker is not available")
    return True


class TestDockerBuild:
    """Tests for building Docker images."""

    @pytest.mark.integration
    def test_build_tinc_image(self, docker_available):
        """Test building the Tinc VPN image."""
        result = run_command(
            [
                "docker",
                "build",
                "-f",
                "docker/Dockerfile.tinc",
                "-t",
                "redundanet-tinc:test",
                ".",
            ],
            timeout=600,
        )

        assert result.returncode == 0, f"Build failed: {result.stderr}"

    @pytest.mark.integration
    def test_build_tahoe_introducer_image(self, docker_available):
        """Test building the Tahoe Introducer image."""
        result = run_command(
            [
                "docker",
                "build",
                "-f",
                "docker/Dockerfile.tahoe-introducer",
                "-t",
                "redundanet-tahoe-introducer:test",
                ".",
            ],
            timeout=600,
        )

        assert result.returncode == 0, f"Build failed: {result.stderr}"

    @pytest.mark.integration
    def test_build_tahoe_storage_image(self, docker_available):
        """Test building the Tahoe Storage image."""
        result = run_command(
            [
                "docker",
                "build",
                "-f",
                "docker/Dockerfile.tahoe-storage",
                "-t",
                "redundanet-tahoe-storage:test",
                ".",
            ],
            timeout=600,
        )

        assert result.returncode == 0, f"Build failed: {result.stderr}"

    @pytest.mark.integration
    def test_build_tahoe_client_image(self, docker_available):
        """Test building the Tahoe Client image."""
        result = run_command(
            [
                "docker",
                "build",
                "-f",
                "docker/Dockerfile.tahoe-client",
                "-t",
                "redundanet-tahoe-client:test",
                ".",
            ],
            timeout=600,
        )

        assert result.returncode == 0, f"Build failed: {result.stderr}"


class TestDockerComposeConfig:
    """Tests for Docker Compose configuration."""

    @pytest.mark.integration
    def test_compose_config_valid(self, docker_available):
        """Test that docker-compose.yml is valid."""
        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                "docker/docker-compose.yml",
                "config",
            ]
        )
        assert result.returncode == 0, f"Invalid compose config: {result.stderr}"

    @pytest.mark.integration
    def test_compose_test_config_valid(self, docker_available):
        """Test that docker-compose.test.yml is valid."""
        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                "docker/docker-compose.test.yml",
                "config",
            ]
        )
        assert result.returncode == 0, f"Invalid test compose config: {result.stderr}"

    @pytest.mark.integration
    def test_compose_dev_config_valid(self, docker_available):
        """Test that docker-compose.dev.yml overlay is valid."""
        result = run_command(
            [
                "docker",
                "compose",
                "-f",
                "docker/docker-compose.yml",
                "-f",
                "docker/docker-compose.dev.yml",
                "config",
            ]
        )
        assert result.returncode == 0, f"Invalid dev compose config: {result.stderr}"
