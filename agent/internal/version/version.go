// Package version holds what a build carries about the world it belongs
// to: the version, the release key that signs the release index, where the
// index is published, and the network the binary joins by default. Only
// the version is set when a release is built (scripts/build-agent.sh);
// everything a binary trusts is written here, in source that is reviewed
// and that the maintainer rebuilds before signing a release, so no setting
// of the build service can change it.
package version

// Version is "dev" in a plain build, which never updates itself.
var Version = "dev"

// ReleaseKey is the base32 public half of the key that signs the release
// index: the one key a device trusts for what it installs. A test pins it
// to the signer of releases/index.sig. It changes only through a rotation
// record in the index, signed by the key being retired. A fork sets its
// own, or empties it to build a binary that never updates itself.
var ReleaseKey = "ullhlnovqlnxchcvfsdcdb6lrohxsucw5na6oevl7cedin227l4a"

// ReleaseIndexURL is where the signed release index is fetched from. A fork
// that builds its own binary sets its own.
var ReleaseIndexURL = "https://raw.githubusercontent.com/adefilippo83/redundanet/main/releases/index.yaml"

// Network is the descriptor of the network this binary belongs to, as
// `redundanet network descriptor` prints it: the name, the hubs, the
// operator keys that are the trust anchor, and where the signed manifest is
// served. It is the default of every --network flag, and a device that has
// not joined a network by hand joins this one. The operator keys in it are
// trusted the same way the release key above is: by trusting the binary,
// which the signed release index vouches for. A fork sets its own, or
// empties it to make --network and `network join <descriptor>` required.
var Network = "rn3:net:pGRodWJzgaJja2V5WCALOLHwmTWbWj-e7yw_Sp8-0D0_janLDwPqi3cCmLIqmmVyZWxheXdodWIucmVkdW5kYW5ldC5jb206ODQ0M2duZXR3b3JranJlZHVuZGFuZXRpb3BlcmF0b3JzgVggguSNgfODyf2-nGcD4B1gB0NpE05_2fOk2a3J8dzP-jVqcmVwb3NpdG9yeXg-aHR0cHM6Ly9yYXcuZ2l0aHVidXNlcmNvbnRlbnQuY29tL2FkZWZpbGlwcG84My9yZWR1bmRhbmV0L21haW4"
