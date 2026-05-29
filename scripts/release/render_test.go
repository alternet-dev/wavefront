// Package release_test pins the output of render-homebrew-formula.sh against
// a checked-in fixture. The script is the source of truth for the formula
// shape; when it changes, regenerate the fixture and commit both. The test
// is fully offline: per-arch shas are injected via WAVEFRONT_SHA_OVERRIDE_*
// env vars instead of curl'ing the GitHub Release.
package release_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderHomebrewFormulaMatchesGolden(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping render-homebrew-formula.sh golden test")
	}

	script, err := filepath.Abs("render-homebrew-formula.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("stat script: %v", err)
	}

	cmd := exec.Command("bash", script, "v0.5.0", "alternet-dev/wavefront")
	cmd.Env = append(os.Environ(),
		"WAVEFRONT_SHA_OVERRIDE_AARCH64_APPLE_DARWIN="+strings.Repeat("a", 64),
		"WAVEFRONT_SHA_OVERRIDE_X86_64_UNKNOWN_LINUX_GNU="+strings.Repeat("c", 64),
		"WAVEFRONT_SHA_OVERRIDE_AARCH64_UNKNOWN_LINUX_GNU="+strings.Repeat("d", 64),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run script: %v; stderr=%s", err, stderr.String())
	}

	goldenPath := filepath.Join("testdata", "expected-formula.rb")
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file %s: %v", goldenPath, err)
	}

	got := stdout.Bytes()
	if !bytes.Equal(got, golden) {
		t.Fatalf(
			"render-homebrew-formula.sh output does not match %s.\n"+
				"--- got ---\n%s\n--- want ---\n%s\n",
			goldenPath, got, golden,
		)
	}
}

func TestRenderHomebrewFormulaRequiresArgs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping render-homebrew-formula.sh arg-validation test")
	}

	script, err := filepath.Abs("render-homebrew-formula.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	// Zero args -> script should exit non-zero with the `set -u` /
	// `${1:?...}` error for the missing ref-name.
	cmd := exec.Command("bash", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		t.Fatalf("expected non-zero exit with no args; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "ref name required") {
		t.Fatalf("expected stderr to mention 'ref name required'; got: %s", stderr.String())
	}
}
