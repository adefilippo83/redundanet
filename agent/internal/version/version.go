// Package version holds what a build carries about the world it belongs
// to, settable with -ldflags "-X": the version the release workflow
// stamps, the release key that signs the release index, where the index is
// published, and the network the binary joins by default.
package version

// Version is "dev" in a plain build.
var Version = "dev"

// ReleaseKey is the base32 public half of the key that signs the release
// index. Empty in a plain build, which disables self-update.
var ReleaseKey = ""

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
