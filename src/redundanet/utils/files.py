"""File utility functions for RedundaNet."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml

from redundanet.utils.logging import get_logger

logger = get_logger(__name__)


def ensure_dir(path: Path | str, mode: int = 0o755) -> Path:
    """Ensure a directory exists, creating it if necessary.

    Args:
        path: Directory path
        mode: Permission mode for the directory

    Returns:
        The Path object for the directory
    """
    path = Path(path)
    if not path.exists():
        path.mkdir(parents=True, mode=mode)
        logger.debug("Created directory", path=str(path))
    return path


def read_yaml(path: Path | str) -> dict[str, Any]:
    """Read and parse a YAML file.

    Args:
        path: Path to the YAML file

    Returns:
        Parsed YAML content as dictionary

    Raises:
        FileNotFoundError: If file doesn't exist
        yaml.YAMLError: If YAML parsing fails
    """
    path = Path(path)
    if not path.exists():
        raise FileNotFoundError(f"YAML file not found: {path}")

    with path.open() as f:
        data = yaml.safe_load(f)

    return data if isinstance(data, dict) else {}


def write_yaml(path: Path | str, data: dict[str, Any], mode: int = 0o644) -> None:
    """Write data to a YAML file.

    Args:
        path: Path to write to
        data: Dictionary to serialize as YAML
        mode: File permission mode
    """
    path = Path(path)
    ensure_dir(path.parent)

    with path.open("w") as f:
        yaml.dump(data, f, default_flow_style=False, sort_keys=False)

    path.chmod(mode)
    logger.debug("Wrote YAML file", path=str(path))


def read_file(path: Path | str) -> str:
    """Read a text file.

    Args:
        path: Path to the file

    Returns:
        File contents as string
    """
    path = Path(path)
    return path.read_text()


def write_file(
    path: Path | str,
    content: str,
    mode: int = 0o644,
    executable: bool = False,
) -> Path:
    """Write content to a text file.

    Args:
        path: Path to write to
        content: Content to write
        mode: File permission mode
        executable: If True, make the file executable

    Returns:
        Path to the written file
    """
    path = Path(path)
    ensure_dir(path.parent)

    path.write_text(content)

    if executable:
        mode = mode | 0o111

    path.chmod(mode)
    logger.debug("Wrote file", path=str(path))
    return path
