// Command depshelf is a read-through npm + PyPI mirror for airgapped or
// flaky-network development. See README.md for the full story.
package main

import (
	"os"

	"github.com/JaydenCJ/depshelf/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
