# RedundaNet documentation

RedundaNet 3 is the current product. One page per topic; every page stands on its own.

- [install.md](install.md): download, verify and install the agent on macOS, Linux, a Linux storage node, a Raspberry Pi 5 and Windows.
- [join.md](join.md): become a member: identity, the join form, what happens next, more devices.
- [migration.md](migration.md): for former RedundaNet 2 members: the agent on each node, the join request, native backup sets, the restore test, `migrate-node`, what the end of RedundaNet 2 means.
- [backups.md](backups.md): backup sets, schedule, excludes, skipped files, restores, restore tests, cleanup and the 30-day window.
- [configuration.md](configuration.md): the v3 manifest keys, `agent.env`, `backup.yaml`, data directories, channels, the local API.
- [operators.md](operators.md): editing and signing the manifest, revocations, encoding transitions, grace and minimum version, the hub runbooks.
- [security.md](security.md): the threat model in short: keys and phrase, what the hub sees, device revocation, the local page, phrase loss, a lost device.
- [architecture.md](architecture.md): what runs today: agent, hub, storage servers, manifest, release index, and the package map.
- [cli.md](cli.md): every command of the `redundanet` binary with its flags.
- [faq.md](faq.md): short answers to the questions members ask.
- [../COMMUNITY.md](../COMMUNITY.md): how the network is governed and how to run your own.
- [../PRIVACY.md](../PRIVACY.md): what the software sends, to whom, and what stays on your device.
- [../releases/README.md](../releases/README.md): the signed release index and how a release is published.
- [../rfcs/0001-redundanet-3.md](../rfcs/0001-redundanet-3.md): the design.

The website at [https://redundanet.com](https://redundanet.com) is the hand-written HTML in this folder, served by GitHub Pages.
