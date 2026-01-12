"""File utility functions for RedundaNet."""

from __future__ import annotations

import shutil
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


def safe_copy(src: Path | str, dst: Path | str, mode: int | None = None) -> Path:
    """Safely copy a file, preserving permissions.

    Args:
        src: Source file path
        dst: Destination file path
        mode: Optional permission mode to set on destination

    Returns:
        Path to the destination file
    """
    src = Path(src)
    dst = Path(dst)

    ensure_dir(dst.parent)
    shutil.copy2(src, dst)

    if mode is not None:
        dst.chmod(mode)

    logger.debug("Copied file", src=str(src), dst=str(dst))
    return dst


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


def remove_path(path: Path | str, ignore_errors: bool = False) -> bool:
    """Remove a file or directory.

    Args:
        path: Path to remove
        ignore_errors: If True, don't raise errors

    Returns:
        True if removal succeeded, False otherwise
    """
    path = Path(path)
    try:
        if path.is_file():
            path.unlink()
        elif path.is_dir():
            shutil.rmtree(path)
        else:
            return False
        logger.debug("Removed path", path=str(path))
        return True
    except (OSError, PermissionError) as e:
        if ignore_errors:
            logger.warning("Failed to remove path", path=str(path), error=str(e))
            return False
        raise
