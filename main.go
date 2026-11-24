// Command plugyard simulates EV chargers for testing a CSMS.
package main

import (
	_ "embed"
	"os"

	"github.com/Amraa1/plugyard/internal/cli"
)

// The guide for AI agents is kept at the root of the repository, where
// tools that read llms.txt look for it, and built into the program, so that
// "plugyard agents" prints the one that matches the version.
//
//go:embed llms.txt
var agentGuide string

func main() {
	cli.AgentGuide = agentGuide
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
