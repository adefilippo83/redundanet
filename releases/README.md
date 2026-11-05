# Release index

`index.yaml` lists every RedundaNet 3 release the agent may install, and
`index.sig` is the maintainer's signature over it with the release key,
whose public half is embedded in every build. An agent installs only what
this index names, after verifying the signature against its embedded key or
a key the index's rotation chain hands over to.

To publish a release:

1. Tag `agent/vX.Y.Z`, or `agent/vX.Y.Z-alpha.N`, `-beta.N`, `-rc.N` for a
   pre-release, which sorts before the release it precedes; the `agent
   release` workflow builds the assets, opens a draft release and prints the
   index fragment for them.
2. Paste the fragment into `index.yaml`, raise `revision` by one, set the
   channel (`early` first, `stable` once the early members are fine).
3. Sign offline: `redundanet release sign --key release.key --index releases/index.yaml`.
4. Commit `index.yaml` and `index.sig` together, publish the draft release.

To withdraw a release, set `withdrawn: true` on it, raise the revision and
sign again. The key rotates with a record under `rotations` signed by the
key being retired.
