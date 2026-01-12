"""Structured logging configuration for RedundaNet."""

from __future__ import annotations

import contextlib
import logging
import sys
from typing import Any

import structlog
from structlog.typing import Processor


def setup_logging(
    level: str = "INFO",
    json_output: bool = False,
    log_file: str | None = None,
) -> None:
    """Configure structured logging for the application.

    Args:
        level: Log level (DEBUG, INFO, WARNING, ERROR, CRITICAL)
        json_output: If True, output logs as JSON (useful for production)
        log_file: Optional file path to write logs to
    """
    # Configure standard library logging
    logging.basicConfig(
        format="%(message)s",
        stream=sys.stderr,
        level=getattr(logging, level.upper()),
    )

    # Set up processors
    shared_processors: list[Processor] = [
        structlog.contextvars.merge_contextvars,
        structlog.stdlib.add_log_level,
        structlog.stdlib.add_logger_name,
        structlog.stdlib.PositionalArgumentsFormatter(),
        structlog.processors.TimeStamper(fmt="iso"),
        structlog.processors.StackInfoRenderer(),
        structlog.processors.UnicodeDecoder(),
    ]

    if json_output:
        # JSON output for production
        processors: list[Processor] = [
            *shared_processors,
            structlog.processors.format_exc_info,
            structlog.processors.JSONRenderer(),
        ]
    else:
        # Console output for development
        processors = [
            *shared_processors,
            structlog.dev.ConsoleRenderer(colors=True),
        ]

    structlog.configure(
        processors=processors,
        wrapper_class=structlog.stdlib.BoundLogger,
        context_class=dict,
        logger_factory=structlog.stdlib.LoggerFactory(),
        cache_logger_on_first_use=True,
    )

    # Configure file handler if specified
    if log_file:
        file_handler = logging.FileHandler(log_file)
        file_handler.setLevel(getattr(logging, level.upper()))
        logging.getLogger().addHandler(file_handler)


def get_logger(name: str | None = None) -> structlog.stdlib.BoundLogger:
    """Get a structured logger instance.

    Args:
        name: Logger name (typically __name__)

    Returns:
        A configured structlog logger
    """
    return structlog.get_logger(name)  # type: ignore[no-any-return]


def log_context(**kwargs: Any) -> contextlib.AbstractContextManager[None]:
    """Context manager to add context to all logs within the block.

    Example:
        with log_context(node_name="node1", operation="sync"):
            logger.info("Starting operation")
            # All logs here will include node_name and operation
    """
    return structlog.contextvars.bound_contextvars(**kwargs)
