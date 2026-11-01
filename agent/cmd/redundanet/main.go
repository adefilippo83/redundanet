// Command redundanet is the RedundaNet 3 binary: agent, hub and CLI in one
// executable (RFC 0001). Phase 1 is being built; what exists today is the
// identity commands.
package main

import (
	"fmt"
	"os"
)

type command struct {
	name, summary string
	run           func(args []string) error
}

var commands = []command{
	{"identity", "member and device identity: init, show, device-key, certify, accept-certificate", runIdentity},
	{"operator", "the offline operator key: init", runOperator},
	{"manifest", "sign a merged manifest, or verify a signature: sign, verify", runManifest},
	{"validate", "check a manifest and print the allocations", runValidate},
	{"network", "the network descriptor: descriptor, join, status", runNetwork},
	{"agent", "run the agent: join the mesh through the hubs of a signed manifest", runAgent},
	{"hub", "run the hub mode: rendezvous and relay for a signed manifest", runHub},
	{"version", "print the version", func([]string) error { fmt.Println(versionString()); return nil }},
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
		usage()
		os.Exit(2)
	}
	for _, c := range commands {
		if c.name == os.Args[1] {
			if err := c.run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "redundanet:", err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "redundanet: unknown command %q\n\n", os.Args[1])
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: redundanet <command> [arguments]")
	fmt.Fprintln(os.Stderr)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.summary)
	}
}
