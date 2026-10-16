// Command track-record keeps passes on record and wraps a command so that a
// pass already on record is reused instead of re-run.
package main

import (
	"os"

	"github.com/kasofsk/track-record/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
