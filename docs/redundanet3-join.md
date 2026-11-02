# Joining RedundaNet 3 as an existing member

RedundaNet 3 (RFC 0001) runs beside the RedundaNet 2 stack during the
migration. Nothing changes for your Tahoe node. What you add is an identity
of your own, made on your laptop, and the agent on your node. The public
status of the new network is at <https://redundanet-hub.fly.dev/v3/>.

Four steps. Two are on your laptop, one is a pull request, one is on the
node. Only the laptop ever holds your phrase.

## 1. Your identity, on your laptop

Download the agent for your laptop from the
[releases page](https://github.com/adefilippo83/redundanet/releases)
(`agent/v3.0.0-...`, the `darwin`, `windows` or `linux` archive) and check
its sha256 against `releases/index.yaml` on main. Then:

```bash
redundanet identity init --network redundanet
```

It shows a twelve-word phrase once. Write it down and keep it like a
password: it is your member key, and every device you ever certify comes
from it. It also prints your member key (52 characters), a display code,
and this laptop's device key. Say `--passphrase` to seal the identity file.

## 2. The attestation that you are you

Your RedundaNet 2 nodes are listed with GPG keys. Sign your new member key
with one of them, which is what ties the new identity to the person the
network already trusts:

```bash
printf '%s' <your member key> | gpg --armor --detach-sign -u <fingerprint of one of your nodes' keys>
```

If that key lives on the node, run the command there over SSH. CI verifies
the signature against the fingerprint the manifest lists for your nodes, so
it has to be one of those keys, published on a keyserver with its user IDs
(keyserver.ubuntu.com keeps them; keys.openpgp.org strips them unless the
e-mail is verified, and a key without them cannot be imported).

## 3. The pull request

Add yourself to `members:` in `manifests/manifest.yaml`, beside the
RedundaNet 2 entries, which stay as they are:

```yaml
members:
  - name: <your member name, the same as `member:` on your nodes>
    key: <your member key>
    contribution_gb: <the sum of your nodes' storage_contribution, in GB>
    joined: <today, YYYY-MM-DD>
    status: active
    legacy_pgp:
      fingerprint: <the 40-character fingerprint you signed with>
      signature: |
        -----BEGIN PGP SIGNATURE-----
        ...
        -----END PGP SIGNATURE-----
```

A member who contributes disk also lists each node under `storage:`, which
needs the node's device key from step 4, so most members do step 4's
`prepare` first and put both in one pull request:

```yaml
storage:
  - member: <your member name>
    device: <the device key the installer printed>
    name: <node name>
```

Raise `revision` by one. The operator reviews, signs the manifest on your
branch (`manifest.sig` must match the file, which is why CI shows the
signature check red until then), and merges. Every agent applies the new
revision within five minutes.

## 4. The agent on your node

Follow [agent/deploy/node/README.md](../agent/deploy/node/README.md):
`install.sh prepare` on the node prints its device key; you certify that key
on your laptop with `redundanet identity certify --storage`, copy the
certificate over, and `install.sh finish` accepts it, joins and starts the
service. From then on the node keeps itself up to date from the signed
release index, with probation and rollback, and appears on the status page
under your name with its free space.

## What you can expect

- The agent stores nothing yet. The native grid comes in phase 2 (RFC 0001,
  gate G2); your Tahoe shares stay where they are until G3.
- Your laptop appears on the page whenever you run `redundanet agent` on it,
  and the local page at `redundanet open` shows what your device sees.
- Lost the phrase? A new identity is a new member entry; the operator
  retires the old one. Lost a node? Certify the new one, the old device key
  is revoked.
