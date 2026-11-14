//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package bundlegen_test

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

func TestPackRefusesFIFOInBundleWithoutBlocking(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	fifoPath := filepath.Join(bundleDir, "2026-05-17", "fifo")
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo unavailable: %v", err)
	}

	outDir := t.TempDir()
	if err := bundlegen.Pack(bundleDir, filepath.Join(outDir, "out.tar.gz")); err == nil {
		t.Fatal("Pack with FIFO in bundle directory: want error, got nil")
	}
}
