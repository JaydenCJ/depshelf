// Package cli implements the depshelf command line: serve, import, list,
// verify, version. Main is in-process testable — it takes argv and the two
// output streams and returns the exit code.
package cli

import (
	"fmt"
	"io"

	"github.com/JaydenCJ/depshelf/internal/version"
)

// Exit codes, stable across releases:
//
//	0 success
//	1 verification failure (corrupt artifacts found)
//	2 usage error
//	3 runtime error
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitRuntime = 3
)

const usageText = `depshelf %s — read-through npm + PyPI mirror with a plain-file store

Usage:
  depshelf serve  [flags]              start the mirror HTTP server
  depshelf import npm|pypi [flags] F   add a local tarball/wheel to the store
  depshelf list   [flags]              show what the store holds
  depshelf verify [flags]              re-hash every stored artifact
  depshelf version                     print the version

Run 'depshelf <command> -h' for command flags.
Exit codes: 0 ok, 1 verification failure, 2 usage error, 3 runtime error.
`

// Main dispatches subcommands and returns the process exit code.
func Main(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintf(stderr, usageText, version.Version)
		return exitUsage
	}
	switch argv[0] {
	case "serve":
		return runServe(argv[1:], stdout, stderr)
	case "import":
		return runImport(argv[1:], stdout, stderr)
	case "list":
		return runList(argv[1:], stdout, stderr)
	case "verify":
		return runVerify(argv[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "depshelf %s\n", version.Version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprintf(stdout, usageText, version.Version)
		return exitOK
	default:
		fmt.Fprintf(stderr, "depshelf: unknown command %q\n\n", argv[0])
		fmt.Fprintf(stderr, usageText, version.Version)
		return exitUsage
	}
}

// runtimeErr prints a runtime failure uniformly and returns its exit code.
func runtimeErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "depshelf: %v\n", err)
	return exitRuntime
}
