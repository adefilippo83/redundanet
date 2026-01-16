"""Process management utilities for RedundaNet."""

from __future__ import annotations

import asyncio
import shlex
import subprocess
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

from redundanet.utils.logging import get_logger

logger = get_logger(__name__)


@dataclass
class CommandResult:
    """Result of a command execution."""

    returncode: int
    stdout: str
    stderr: str
    command: str

    @property
    def success(self) -> bool:
        """Check if the command succeeded."""
        return self.returncode == 0

    def check(self) -> None:
        """Raise exception if command failed."""
        if not self.success:
            raise subprocess.CalledProcessError(
                self.returncode,
                self.command,
                self.stdout,
                self.stderr,
            )


def run_command(
    command: str | Sequence[str],
    *,
    cwd: Path | str | None = None,
    env: dict[str, str] | None = None,
    timeout: float | None = None,
    capture_output: bool = True,
    check: bool = False,
    input_text: str | None = None,
) -> CommandResult:
    """Run a shell command and return the result.

    Args:
        command: Command to run (string or list of arguments)
        cwd: Working directory
        env: Environment variables
        timeout: Timeout in seconds
        capture_output: Whether to capture stdout/stderr
        check: If True, raise exception on non-zero exit
        input_text: Text to send to stdin

    Returns:
        CommandResult with return code, stdout, stderr
    """
    if isinstance(command, str):
        cmd_str = command
        args = shlex.split(command)
    else:
        cmd_str = " ".join(command)
        args = list(command)

    logger.debug("Running command", command=cmd_str, cwd=str(cwd) if cwd else None)

    try:
        result = subprocess.run(
            args,
            cwd=cwd,
            env=env,
            timeout=timeout,
            capture_output=capture_output,
            text=True,
            input=input_text,
        )

        cmd_result = CommandResult(
            returncode=result.returncode,
            stdout=result.stdout if capture_output else "",
            stderr=result.stderr if capture_output else "",
            command=cmd_str,
        )

        if check:
            cmd_result.check()

        return cmd_result

    except subprocess.TimeoutExpired:
        logger.error("Command timed out", command=cmd_str, timeout=timeout)
        return CommandResult(
            returncode=-1,
            stdout="",
            stderr=f"Command timed out after {timeout}s",
            command=cmd_str,
        )
    except FileNotFoundError:
        logger.error("Command not found", command=args[0])
        return CommandResult(
            returncode=-1,
            stdout="",
            stderr=f"Command not found: {args[0]}",
            command=cmd_str,
        )


async def run_command_async(
    command: str | Sequence[str],
    *,
    cwd: Path | str | None = None,
    env: dict[str, str] | None = None,
    timeout: float | None = None,
    input_text: str | None = None,
) -> CommandResult:
    """Run a shell command asynchronously.

    Args:
        command: Command to run (string or list of arguments)
        cwd: Working directory
        env: Environment variables
        timeout: Timeout in seconds
        input_text: Text to send to stdin

    Returns:
        CommandResult with return code, stdout, stderr
    """
    if isinstance(command, str):
        cmd_str = command
        # Use shell=True for string commands
        process = await asyncio.create_subprocess_shell(
            command,
            cwd=cwd,
            env=env,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            stdin=asyncio.subprocess.PIPE if input_text else None,
        )
    else:
        cmd_str = " ".join(command)
        process = await asyncio.create_subprocess_exec(
            *command,
            cwd=cwd,
            env=env,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            stdin=asyncio.subprocess.PIPE if input_text else None,
        )

    logger.debug("Running async command", command=cmd_str)

    try:
        stdin_bytes = input_text.encode() if input_text else None
        stdout, stderr = await asyncio.wait_for(
            process.communicate(input=stdin_bytes),
            timeout=timeout,
        )

        return CommandResult(
            returncode=process.returncode or 0,
            stdout=stdout.decode() if stdout else "",
            stderr=stderr.decode() if stderr else "",
            command=cmd_str,
        )

    except TimeoutError:
        process.kill()
        await process.wait()
        logger.error("Async command timed out", command=cmd_str, timeout=timeout)
        return CommandResult(
            returncode=-1,
            stdout="",
            stderr=f"Command timed out after {timeout}s",
            command=cmd_str,
        )


def is_command_available(command: str) -> bool:
    """Check if a command is available in PATH.

    Args:
        command: Command name to check

    Returns:
        True if command is available
    """
    result = run_command(f"which {command}", check=False)
    return result.success


def get_pid_of(process_name: str) -> list[int]:
    """Get PIDs of processes matching a name.

    Args:
        process_name: Process name to search for

    Returns:
        List of PIDs
    """
    result = run_command(f"pgrep -f {process_name}", check=False)
    if result.success and result.stdout.strip():
        return [int(pid) for pid in result.stdout.strip().split("\n")]
    return []


def kill_process(pid: int, force: bool = False) -> bool:
    """Kill a process by PID.

    Args:
        pid: Process ID
        force: If True, use SIGKILL instead of SIGTERM

    Returns:
        True if successful
    """
    signal = "-9" if force else "-15"
    result = run_command(f"kill {signal} {pid}", check=False)
    return result.success
