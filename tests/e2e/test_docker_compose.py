"""End-to-end tests for Docker Compose setup."""

import subprocess
from pathlib import Path

import pytest

DOCKER_DIR = Path(__file__).parent.parent.parent / "docker"


def docker_available():
    """Check if Docker is available and running."""
    try:
        result = subprocess.run(
            ["docker", "info"],
            capture_output=True,
            timeout=10,
        )
        return result.returncode == 0
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return False


@pytest.mark.e2e
@pytest.mark.skipif(not docker_available(), reason="Docker is not available")
class TestDockerCompose:
    """E2E tests for Docker Compose configuration."""

    def test_compose_config_valid(self):
        """Test that docker-compose.yml is valid."""
        result = subprocess.run(
            ["docker", "compose", "-f", str(DOCKER_DIR / "docker-compose.yml"), "config"],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0, f"Invalid compose config: {result.stderr}"

    def test_compose_dev_config_valid(self):
        """Test that docker-compose.dev.yml is valid."""
        result = subprocess.run(
            [
                "docker",
                "compose",
                "-f",
                str(DOCKER_DIR / "docker-compose.yml"),
                "-f",
                str(DOCKER_DIR / "docker-compose.dev.yml"),
                "config",
            ],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0, f"Invalid dev compose config: {result.stderr}"

    def test_build_images(self):
        """Test building Docker images."""
        result = subprocess.run(
            [
                "docker",
                "compose",
                "-f",
                str(DOCKER_DIR / "docker-compose.yml"),
                "build",
            ],
            capture_output=True,
            text=True,
            timeout=600,  # 10 minute timeout
        )
        assert result.returncode == 0, f"Build failed: {result.stderr}"

    def test_services_start(self):
        """Test starting services."""
        # Start only tinc for initial test
        try:
            subprocess.run(
                [
                    "docker",
                    "compose",
                    "-f",
                    str(DOCKER_DIR / "docker-compose.yml"),
                    "up",
                    "-d",
                    "tinc",
                ],
                capture_output=True,
                text=True,
                timeout=120,
                check=True,
            )

            # Check service is running
            result = subprocess.run(
                [
                    "docker",
                    "compose",
                    "-f",
                    str(DOCKER_DIR / "docker-compose.yml"),
                    "ps",
                    "--format",
                    "json",
                ],
                capture_output=True,
                text=True,
            )
            assert "tinc" in result.stdout

        finally:
            # Cleanup
            subprocess.run(
                [
                    "docker",
                    "compose",
                    "-f",
                    str(DOCKER_DIR / "docker-compose.yml"),
                    "down",
                    "-v",
                ],
                capture_output=True,
            )
