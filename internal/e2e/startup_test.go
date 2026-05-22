package e2e

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runWavefront invokes the wavefront binary as a subprocess with the given
// environment and returns the resulting *exec.ExitError (or nil if the
// process exited 0). Stderr is captured for diagnostics on failure.
func runWavefront(t *testing.T, env []string) (*exec.ExitError, string) {
	t.Helper()
	cmd := exec.Command("go", "run", "github.com/alternet-dev/wavefront/cmd/wavefront")
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil, stderr.String()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("invoking wavefront: %v; stderr=%s", err, stderr.String())
	}
	return exitErr, stderr.String()
}

func TestFailFastOnBadBundle(t *testing.T) {
	// Claim: an invalid / missing bundle path at boot causes wavefront to
	// refuse to start (non-zero exit). Pins the operations.md "fail-fast"
	// claim end-to-end at the binary level.
	env := append(os.Environ(),
		"WAVEFRONT_BUNDLE_PATH=/nonexistent/wavefront-bundle",
		"WAVEFRONT_UPSTREAM_BASE_URL=http://localhost:1",
	)
	exitErr, stderr := runWavefront(t, env)
	if exitErr == nil {
		t.Fatalf("expected non-zero exit; stderr=%s", stderr)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit, got 0; stderr=%s", stderr)
	}
}

func TestFailFastOnMissingRequiredConfig(t *testing.T) {
	// Claim: a missing required environment variable
	// (WAVEFRONT_BUNDLE_PATH / WAVEFRONT_UPSTREAM_BASE_URL) at boot causes
	// wavefront to refuse to start.
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "WAVEFRONT_") {
			env = append(env, e)
		}
	}
	exitErr, stderr := runWavefront(t, env)
	if exitErr == nil {
		t.Fatalf("expected non-zero exit; stderr=%s", stderr)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit, got 0; stderr=%s", stderr)
	}
}
