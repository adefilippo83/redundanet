# RedundaNet Pull Request

## What changes
<!-- What the change does, and why. For a membership change, name the member and the revision. -->

## Type of change
- [ ] Agent (`agent/`): bug fix, feature or refactor
- [ ] Manifest (`manifests/`): join, leave, device, encoding or parameter change
- [ ] Release index (`releases/`)
- [ ] Documentation or website (`docs/`, `README.md`, `COMMUNITY.md`, `PRIVACY.md`)
- [ ] Legacy Python stack (`src/`, `docker/`, `scripts/`, `tests/`)
- [ ] CI or workflows (`.github/`)

## Checks
- [ ] `agent/` changed: `cd agent && go test -race ./...` passes, and `go build ./cmd/redundanet` builds
- [ ] `src/` changed: `poetry run pytest tests/unit -q`, `poetry run ruff check src tests` and `poetry run mypy src` pass, and the patch version in `pyproject.toml` is bumped for a user-facing change
- [ ] `manifests/` changed: `redundanet validate --manifest manifests/manifest.yaml` passes, `revision` is raised by one, and `manifest.sig` verifies against the file (the operator signs before merge; CI shows the check red until then)
- [ ] `releases/` changed: `revision` is raised by one and `index.sig` is signed together with `index.yaml`
- [ ] Docs updated when behaviour changed; docs describe what the code does
- [ ] No em dashes in user-facing prose
- [ ] Tested on a real device or node where the change touches the live network (say which)

## Related issues

Closes #
