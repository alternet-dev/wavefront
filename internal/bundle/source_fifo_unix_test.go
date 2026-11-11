//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package bundle

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLoadRejectsFIFOArchiveWithoutBlocking(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := unix.Mkfifo(archive, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	_, err := Load(archive)
	var readErr *ReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("Load FIFO error = %v, want ReadError", err)
	}
}
