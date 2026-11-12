# RedundaNet Pull Request

## What changes
<!-- What the change does, and why. For a membership change, name the member and the revision. -->

## Type of change
- [ ] Agent (`agent/`): bug fix, feature or refactor
- [ ] Manifest (`manifests/`): join, leave, device, encoding or parameter change
- [ ] Release index (`releases/`)
- [ ] Documentation or website (`docs/`, `README.md`, `COMMUNITY.md`, `PRIVACY.md`)
- [ ] CI or workflows (`.github/`)

## Checks
- [ ] `agent/` changed: `cd agent && go test -race ./...` passes, and `go build ./cmd/redundanet` builds
- [ ] `.github/scripts/` changed: `poetry run pytest` and `poetry run ruff check .github/scripts` pass
- [ ] `manifests/` changed: `redundanet validate --manifest manifests/manifest.yaml` passes, `revision` is raised by one, and `manifest.sig` verifies against the file (the operator signs before merge; CI shows the check red until then)
- [ ] `releases/` changed: `revision` is raised by one and `index.sig` is signed together with `index.yaml`
- [ ] Docs updated when behaviour changed; docs describe what the code does
- [ ] No em dashes in user-facing prose
- [ ] Tested on a real device or node where the change touches the live network (say which)

## Related issues

Closes #
