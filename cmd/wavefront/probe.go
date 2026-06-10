package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/probe"
)

// probeTimeout bounds the self-probe's whole GET. The image HEALTHCHECK allots
// 3s per attempt; staying under it means the probe reports its own failure
// rather than being killed mid-flight.
const probeTimeout = 2 * time.Second

// runProbe implements `wavefront probe --ready`: GET the ops listener's /ready
// in-process and map the outcome to the healthcheck exit-code contract —
// 200 → 0, anything else (non-200, refused, timeout) → 1, usage error → 2.
// The ops address resolves from the same WAVEFRONT_METRICS_ADDR the server
// binds, so a non-default port is probed without restating it.
func runProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	ready := fs.Bool("ready", false, "probe the ops listener's /ready endpoint")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		return 2
	}
	if !*ready || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: wavefront probe --ready")
		return 2
	}
	if err := probe.Ready(config.OpsAddr(), probeTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		return 1
	}
	return 0
}
