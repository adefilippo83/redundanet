# Release index

`index.yaml` lists every RedundaNet 3 release the agent may install, and
`index.sig` is the maintainer's signature over it with the release key,
whose public half is embedded in every build. An agent installs only what
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

   It repeats every check on its own, rebuilds the revision on the current
   main with `redundanet release add` (built from main, never from the
   branch), shows what it will sign and asks, then signs, pushes the
   signature to the pull request, merges it once CI is green and publishes
   the draft. `--dry-run` stops before signing. The key file is
   `~/.redundanet/release.key` unless `RELEASE_KEY` names another.

By hand, the same steps: `redundanet release add --fragment
index-fragment.yaml --assets DIR` with the release's archives downloaded
into DIR, then `redundanet release sign --key release.key --index
releases/index.yaml`, commit `index.yaml` and `index.sig` together, publish
the draft release.

To withdraw a release, set `withdrawn: true` on it, raise the revision and
sign again. The key rotates with a record under `rotations` signed by the
key being retired.
