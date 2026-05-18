// Command wavefront is the edge contract-mediation proxy: it maps a versioned
// external contract onto a single evolving internal HTTP/JSON backend.
//
// This is a pre-implementation stub. v0.1 is built in sequenced chunks; the
// real entrypoint — config load, bundle load (fail-fast), and the data-plane
// plus ops listeners — lands with internal/server.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "wavefront: pre-implementation stub; not yet runnable")
	os.Exit(1)
}
