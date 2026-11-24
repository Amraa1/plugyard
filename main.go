// Command plugyard simulates EV chargers for testing a CSMS.
package main

import (
	"os"

	"github.com/Amraa1/plugyard/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
