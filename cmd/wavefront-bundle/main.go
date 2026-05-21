// Command wavefront-bundle builds and maintains the read-only wavefront
// bundle — a directory of immutable per-version layers. It is a thin
// subcommand dispatcher; all logic lives in internal/bundlegen so the e2e
// can drive it in-process.
//
// Usage:
//
//	wavefront-bundle add --openapi <file|url> --bundle <dir>
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run dispatches a subcommand and returns a process exit code. Diagnostics go
// to errOut. It is the testable entry point — main is a thin os.Exit wrapper.
func run(args []string, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "usage: wavefront-bundle <add> [flags]")
		return 2
	}
	switch args[0] {
	case "add":
		return runAdd(args[1:], errOut)
	default:
		fmt.Fprintf(errOut, "wavefront-bundle: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runAdd(args []string, errOut io.Writer) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(errOut)
	openapi := fs.String("openapi", "", "source OpenAPI JSON: a file path or an http(s):// URL")
	bundleDir := fs.String("bundle", "", "bundle directory to add the layer into")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *openapi == "" || *bundleDir == "" {
		fmt.Fprintln(errOut, "usage: wavefront-bundle add --openapi <file|url> --bundle <dir>")
		return 2
	}
	if err := bundlegen.Add(*openapi, *bundleDir); err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle add:", err)
		return 1
	}
	return 0
}
