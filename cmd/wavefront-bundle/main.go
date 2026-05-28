// Command wavefront-bundle builds and maintains the read-only wavefront
// bundle — a directory of immutable per-version layers. It is a thin
// subcommand dispatcher; all logic lives in internal/bundlegen so the e2e
// can drive it in-process.
//
// Usage:
//
//	wavefront-bundle add --openapi <file|url> --bundle <dir>
//	wavefront-bundle remove --version <id> --bundle <dir>
//	wavefront-bundle retire --version <id> --bundle <dir>
//	wavefront-bundle verify --bundle <dir>
//	wavefront-bundle draft-shim --bundle <dir> --from <id> --to <id> [--out <file>] [--strict]
//	wavefront-bundle gen-ts-client --bundle <dir> --out <dir> [--version <id>]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run dispatches a subcommand and returns a process exit code. Diagnostics go
// to errOut. It is the testable entry point — main is a thin os.Exit wrapper.
func run(args []string, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "usage: wavefront-bundle <add|remove|retire|verify|draft-shim|gen-ts-client> [flags]")
		return 2
	}
	switch args[0] {
	case "add":
		return runAdd(args[1:], errOut)
	case "remove":
		return runRemove(args[1:], errOut)
	case "retire":
		return runRetire(args[1:], errOut)
	case "verify":
		return runVerify(args[1:], errOut)
	case "draft-shim":
		return runDraftShim(args[1:], errOut, os.Stdout)
	case "gen-ts-client":
		return runGenTSClient(args[1:], errOut, os.Stdout)
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

func runRemove(args []string, errOut io.Writer) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(errOut)
	version := fs.String("version", "", "the contract version (layer) to remove")
	bundleDir := fs.String("bundle", "", "bundle directory to remove the layer from")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *version == "" || *bundleDir == "" {
		fmt.Fprintln(errOut, "usage: wavefront-bundle remove --version <id> --bundle <dir>")
		return 2
	}
	if err := bundlegen.Remove(*bundleDir, *version); err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle remove:", err)
		return 1
	}
	return 0
}

func runRetire(args []string, errOut io.Writer) int {
	fs := flag.NewFlagSet("retire", flag.ContinueOnError)
	fs.SetOutput(errOut)
	version := fs.String("version", "", "the contract version (layer) to retire")
	bundleDir := fs.String("bundle", "", "bundle directory containing the layer")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *version == "" || *bundleDir == "" {
		fmt.Fprintln(errOut, "usage: wavefront-bundle retire --version <id> --bundle <dir>")
		return 2
	}
	if err := bundlegen.Retire(*bundleDir, *version); err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle retire:", err)
		return 1
	}
	return 0
}

func runVerify(args []string, errOut io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	bundleDir := fs.String("bundle", "", "bundle directory to verify")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *bundleDir == "" {
		fmt.Fprintln(errOut, "usage: wavefront-bundle verify --bundle <dir>")
		return 2
	}
	if err := bundlegen.Verify(*bundleDir); err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle verify:", err)
		return 1
	}
	return 0
}

func runDraftShim(args []string, errOut, stdout io.Writer) int {
	fs := flag.NewFlagSet("draft-shim", flag.ContinueOnError)
	fs.SetOutput(errOut)
	bundleDir := fs.String("bundle", "", "bundle directory containing the two layers")
	fromVersion := fs.String("from", "", "the old contract version (layer subdir) to bridge from")
	toVersion := fs.String("to", "", "the new contract version (layer subdir) to bridge to")
	outFile := fs.String("out", "", "write the drafted override to this file; default stdout")
	strict := fs.Bool("strict", false, "suppress confident case-rename matches; surface every removed/added pair as a candidate")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *bundleDir == "" || *fromVersion == "" || *toVersion == "" {
		fmt.Fprintln(errOut, "usage: wavefront-bundle draft-shim --bundle <dir> --from <version> --to <version> [--out <file>] [--strict]")
		return 2
	}
	fromBytes, err := os.ReadFile(filepath.Join(*bundleDir, *fromVersion, "openapi.json"))
	if err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle draft-shim:", err)
		return 1
	}
	toBytes, err := os.ReadFile(filepath.Join(*bundleDir, *toVersion, "openapi.json"))
	if err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle draft-shim:", err)
		return 1
	}
	var proposal *bundlegen.ShimProposal
	if *strict {
		proposal, err = bundlegen.DraftShimStrict(fromBytes, toBytes)
	} else {
		proposal, err = bundlegen.DraftShim(fromBytes, toBytes)
	}
	if err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle draft-shim:", err)
		return 1
	}
	rendered := proposal.RenderYAML(bundlegen.RenderOptions{})
	if *outFile != "" {
		if err := os.WriteFile(*outFile, []byte(rendered), 0o644); err != nil {
			fmt.Fprintln(errOut, "wavefront-bundle draft-shim:", err)
			return 1
		}
		return 0
	}
	if _, err := fmt.Fprint(stdout, rendered); err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle draft-shim:", err)
		return 1
	}
	return 0
}

// runGenTSClient parses gen-ts-client's flags and delegates to bundlegen.
// The emission step shells out to protoc-gen-es on PATH and writes one
// .ts file per .proto in the layer's descriptor set into --out. routes.ts
// and client.ts are not emitted yet — they land in the follow-up chunks.
func runGenTSClient(args []string, errOut, stdout io.Writer) int {
	fs := flag.NewFlagSet("gen-ts-client", flag.ContinueOnError)
	fs.SetOutput(errOut)
	bundleDir := fs.String("bundle", "", "bundle directory to read")
	outDir := fs.String("out", "", "output directory for the generated TS client (created if missing; must otherwise be empty)")
	version := fs.String("version", "", "pin to a specific contract version (default: the latest version in the bundle)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *bundleDir == "" || *outDir == "" {
		fmt.Fprintln(errOut, "usage: wavefront-bundle gen-ts-client --bundle <dir> --out <dir> [--version <id>]")
		return 2
	}
	res, err := bundlegen.GenTSClient(*bundleDir, *outDir, *version)
	if err != nil {
		fmt.Fprintln(errOut, "wavefront-bundle gen-ts-client:", err)
		return 1
	}
	fmt.Fprintf(stdout,
		"gen-ts-client: bundle=%s version=%s out=%s\n  emitted %d message class file(s)\n",
		res.BundleDir, res.Version, res.OutDir, len(res.MessageFiles))
	return 0
}
