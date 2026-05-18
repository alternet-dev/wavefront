// Command wavefront-bundlegen scrapes a consumer's OpenAPI document and emits
// the read-only wavefront bundle (descriptors.binpb + openapi.json +
// versions.yaml). It is a thin flag parser; all logic lives in
// internal/bundlegen so the e2e can call it in-process.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

func main() {
	openapi := flag.String("openapi", "", "source OpenAPI JSON: a file path or an http(s):// URL")
	out := flag.String("out", "", "output bundle directory")
	flag.Parse()

	if *openapi == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: wavefront-bundlegen --openapi <file|url> --out <bundle-dir>")
		os.Exit(2)
	}
	if err := bundlegen.Generate(*openapi, *out); err != nil {
		fmt.Fprintln(os.Stderr, "bundlegen:", err)
		os.Exit(1)
	}
}
