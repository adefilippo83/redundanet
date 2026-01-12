.PHONY: help install dev lint format test test-unit test-integration test-e2e coverage type-check pre-commit clean build docker-build docker-up docker-down docs

# Default target
help:
	@echo "RedundaNet Development Commands"
	@echo "================================"
	@echo ""
	@echo "Setup:"
	@echo "  install          Install production dependencies"
	@echo "  dev              Install all dependencies including dev tools"
	@echo ""
	@echo "Code Quality:"
	@echo "  lint             Run linter (ruff)"
	@echo "  format           Format code (ruff format)"
	@echo "  type-check       Run type checker (mypy)"
	@echo "  pre-commit       Run all pre-commit hooks"
	@echo ""
	@echo "Testing:"
	@echo "  test             Run all tests"
	@echo "  test-unit        Run unit tests only"
	@echo "  test-integration Run integration tests only"
	@echo "  test-e2e         Run end-to-end tests only"
	@echo "  coverage         Run tests with coverage report"
	@echo ""
	@echo "Docker:"
	@echo "  docker-build     Build all Docker containers"
	@echo "  docker-up        Start all containers"
	@echo "  docker-down      Stop all containers"
	@echo ""
	@echo "Other:"
	@echo "  docs             Build documentation"
	@echo "  clean            Remove build artifacts"
	@echo "  build            Build package for distribution"

# Installation
install:
	poetry install --only main

dev:
	poetry install
	poetry run pre-commit install

# Code Quality
lint:
	poetry run ruff check src tests

format:
	poetry run ruff format src tests
	poetry run ruff check --fix src tests

type-check:
	poetry run mypy src

pre-commit:
	poetry run pre-commit run --all-files

# Testing
test:
	poetry run pytest

test-unit:
	poetry run pytest tests/unit -v

test-integration:
	poetry run pytest tests/integration -v -m integration

test-e2e:
	poetry run pytest tests/e2e -v -m e2e

coverage:
	poetry run pytest --cov=redundanet --cov-report=html --cov-report=term-missing
	@echo "Coverage report generated in htmlcov/"

# Docker
docker-build:
	docker compose -f docker/docker-compose.yml build

docker-up:
	docker compose -f docker/docker-compose.yml up -d

docker-down:
	docker compose -f docker/docker-compose.yml down

docker-logs:
	docker compose -f docker/docker-compose.yml logs -f

# Documentation
docs:
	poetry run mkdocs build

docs-serve:
	poetry run mkdocs serve

# Cleanup
clean:
	rm -rf build dist .eggs *.egg-info
	rm -rf .pytest_cache .mypy_cache .ruff_cache
	rm -rf htmlcov .coverage coverage.xml
	find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete

# Build
build: clean
	poetry build

# Quick check before commit
check: format lint type-check test-unit
	@echo "All checks passed!"
