# Release index

`index.yaml` lists every RedundaNet 3 release the agent may install, and
`index.sig` is the maintainer's signature over it with the release key,
whose public half is written in `agent/internal/version` and so is in every
build. A test pins it to the signer of `index.sig`; no repository variable
or workflow input sets it. An agent installs only what
this index names, after verifying the signature against its embedded key or
a key the index's rotation chain hands over to.

To publish a release:

1. Tag `agent/vX.Y.Z`, or `agent/vX.Y.Z-alpha.N`, `-beta.N`, `-rc.N` for a
   pre-release, which sorts before the release it precedes. The `agent
   release` workflow builds the assets and opens a draft release with the
   index fragment; a plain version goes on the `stable` channel, a
   pre-release on `early`. It then checks the release as it is on GitHub
   (the tag on main, one workflow run for it, every archive matching the
   fragment by sha256 and size) and opens a pull request,
   `release/index-X.Y.Z`, that adds it to `index.yaml` as the next
   revision, unsigned. No key is in CI.
2. On the laptop with the release key, one command:

   ```
   scripts/sign.sh release X.Y.Z
   ```

   It repeats every check on its own. Then it builds the release again:
   it clones the tag, runs the tag's own `scripts/build-agent.sh` (the
   script the workflow ran) and compares the binary inside every archive
   on the release with the one built on the laptop, byte for byte. A
   difference stops it before anything is signed: the signature is for
   the tagged source, not for whatever the release holds. It then rebuilds
   the revision on the current main with `redundanet release add` (built
   from main, never from the branch), shows what it will sign and asks,
   then signs, pushes the signature to the pull request, merges it at
   exactly that commit once CI is green and publishes the draft.
   `--dry-run` stops before signing and is the way to try the comparison.
   The key file is `~/.redundanet/release.key` unless `RELEASE_KEY` names
   another.

   The build is the same on any machine because `build-agent.sh` uses no
   cgo, trimmed paths, an empty build id, the Go toolchain `go.mod` names
   and nothing from the environment; the commit is stamped into the
   binary, so the tree must be clean, which a clone is. Two cases need
   care. A release whose tag predates `build-agent.sh` cannot be compared.
   And once Windows or macOS binaries are code signed after the build,
   those differ from a local build by their signature, and the comparison
   has to be made on the unsigned binary; the script does not do that yet.
   For both, `--trust-github` skips the comparison and says so: the
   signature then vouches for whatever the build service produced.

By hand, the same steps: `scripts/build-agent.sh X.Y.Z DIR` in a clean
clone of the tag and a comparison with the archives, `redundanet release add --fragment
index-fragment.yaml --assets DIR` with the release's archives downloaded
into DIR, then `redundanet release sign --key release.key --index
releases/index.yaml`, commit `index.yaml` and `index.sig` together, publish
the draft release.

To withdraw a release, set `withdrawn: true` on it, raise the revision and
sign again. The key rotates with a record under `rotations` signed by the
key being retired.
