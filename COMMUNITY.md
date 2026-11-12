# How the RedundaNet network is run

RedundaNet is a network of people who give each other disk. The software enforces what it can: who is a member, what a device may write, how much. The rest rests on people, and this page says who decides what, in practice, for the live network. The design that asks for this page is [RFC 0001](rfcs/0001-redundanet-3.md), "The rules that rest on trust".

## The operator

The network has one operator, who is also the maintainer of the software: Alessandro De Filippo, [@adefilippo83](https://github.com/adefilippo83) on GitHub. The operator holds the operator key offline. There is one operator key today; a second operator is added through a manifest the current key signs. How a threshold of operator keys would work is an open question of the RFC (to confirm).

The operator key signs `manifests/manifest.yaml`: the list of operators, hubs, members and storage devices, the encoding, the reserve, the grace period, the minimum version. A manifest takes effect on a device only with that signature and a revision above the last one applied. The operator cannot read anyone's data, install a binary on anyone's device, or delete anything inside the grace period: those limits are in the software, not in a promise ([docs/security.md](docs/security.md)).

A second key, the release key, also held offline by the maintainer, signs the release index. The two keys are separate on purpose: a stolen operator key cannot ship a binary.

## What members agree to

- A member stores only their own data, encrypted on their own devices; storage hosts hold ciphertext they cannot read and are not responsible for its content.
- A member who contributes disk keeps the node running and reachable; the status page shows when it is not, and shares on a server dead for 72 hours are repaired elsewhere at the network's cost.
- A member uses their allocation and no more; servers enforce it with headroom, and the status page shows usage.
- Member names and keys are public in the repository's history forever ([PRIVACY.md](PRIVACY.md)).

## Joining and leaving

A new member fills the form at <https://redundanet.com/join.html>, which opens a GitHub issue; a workflow validates it and opens a pull request; the operator reviews, signs and merges ([docs/join.md](docs/join.md)). A former RedundaNet 2 member attests with the GPG key of one of their nodes ([docs/migration.md](docs/migration.md)). Who is admitted is the operator's decision, made in public on the pull request.

A member leaves by telling the operator. They are marked `leaving` in the manifest; servers keep their shares for the grace period, 30 days today, then drop them.

Removal of a member against their will: who may propose it, on what grounds, and who decides (to confirm). Until written down here, it is the operator's decision, in public on a pull request, with the grace period as the delay.

A member whose devices have all gone silent: after how long they are marked `leaving` (to confirm; the RFC suggests 90 days without any device seen and no answer to the operator).

## Legal demands

A storage host served a legal demand can hand over what it holds: ciphertext, the owner member keys of the shares, sizes and times. It holds nothing else, so it can hand over nothing else. The hub can hand over the same plus the IP addresses it has seen, which it logs. The repository is public.

## Releases

A release is a tag `agent/vX.Y.Z` on `main`, built by the repository's workflow on GitHub's runners. The workflow checks it and prepares the next revision of `releases/index.yaml` as a pull request, unsigned; the maintainer checks it again and signs it offline with the release key, with `scripts/sign.sh`. Every agent installs only what the signed index names, after verifying hash and size, with probation and rollback. The policy and the steps are in [releases/README.md](releases/README.md); the code signing status is in [README.md](README.md). Every change, AI-assisted ones included, is reviewed by the maintainer before it reaches `main`.

## The hub

One hub, `hub-hetzner`, run by the maintainer on a rented server, holds no key but its own and no share. With it down there are no new connections, no repair scheduling and no status page; existing direct connections continue. Its runbook is [agent/deploy/hub/README.md](agent/deploy/hub/README.md).

## Running your own network

The binary carries no reference to this network: which network a device joins comes from the descriptor it is given. The RFC promises (F23) one guide that takes a new community from an empty repository to a running hub and a first member. This section is the stub of that guide; the full version is not written yet.

What a fork needs: a GitHub repository with Actions, a server for the hub, an operator key, and either this project's release chain or its own release key and build.

1. Make the operator key offline: `redundanet operator init --key operator.key`. Put its public half under `operators:` in your own `manifests/manifest.yaml`, with `schema: 3`, `revision: 1`, a `network.name` of your own and an `encoding`.
2. Run the hub on a server with a public UDP and TCP port, following [agent/deploy/hub/README.md](agent/deploy/hub/README.md): make its identity, list its device key and `relay` under `hubs:`, open the port.
3. Validate and sign: `redundanet validate --manifest manifests/manifest.yaml`, then `redundanet manifest sign --key operator.key --manifest manifests/manifest.yaml`. Commit the manifest and `manifest.sig` to your repository.
4. Print the descriptor your members join with: `redundanet network descriptor --manifest manifests/manifest.yaml --repository https://raw.githubusercontent.com/OWNER/REPO/main`.
5. Releases: either let your members' agents follow this project's signed index, which is what a plain build does, or make your own release key with `redundanet release keygen`, embed its public half in your builds and sign your own index ([releases/README.md](releases/README.md)).
6. Copy this page, fill in the parts that are yours, and keep it beside your manifest.

The join form, the workflow that turns an issue into a pull request, and the status page publishing are in this repository under `docs/`, `.github/` and `agent/deploy/hub/`; adapting them is part of the guide still to write (to confirm).

## Contact

The maintainer is reached through the repository's issues at <https://github.com/adefilippo83/redundanet/issues>. For a problem on your device, attach a `redundanet diagnostics` bundle; it holds no path, URL or address. Questions about the privacy policy go the same way.
