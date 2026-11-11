package bundlegen_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

func TestPackProducesDeterministicLoadableArchive(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	outDir := t.TempDir()
	first := filepath.Join(outDir, "first.tar.gz")
	second := filepath.Join(outDir, "second.tar.gz")
	if err := bundlegen.Pack(bundleDir, first); err != nil {
		t.Fatalf("Pack first archive: %v", err)
	}
	if err := bundlegen.Pack(bundleDir, second); err != nil {
		t.Fatalf("Pack second archive: %v", err)
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first archive: %v", err)
	}
	secondBytes, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second archive: %v", err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("Pack output is not byte-reproducible")
	}

	b, err := bundle.Load(first)
	if err != nil {
		t.Fatalf("Load packed archive: %v", err)
	}
	if _, ok := b.Contract("2026-05-17"); !ok {
		t.Fatal(`Contract("2026-05-17") not found in packed archive`)
	}
	if err := bundlegen.Verify(first); err != nil {
		t.Fatalf("Verify packed archive: %v", err)
	}
}

func TestPackRefusesOutputInsideCanonicalBundle(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := bundlegen.Pack(bundleDir, filepath.Join(bundleDir, "bundle.tar.gz")); err == nil {
		t.Fatal("Pack output inside canonical bundle: want error")
	}
}

func TestPackRefusesOutputViaSymlinkInsideCanonicalBundle(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Create a symlink from an outside directory that resolves into bundleDir.
	// A lexical path comparison sees the output as outside the bundle; only a
	// symlink-aware comparison catches that the canonical output is inside it.
	outsideDir := t.TempDir()
	linkPath := filepath.Join(outsideDir, "link")
	if err := os.Symlink(bundleDir, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := bundlegen.Pack(bundleDir, filepath.Join(linkPath, "bundle.tar.gz")); err == nil {
		t.Fatal("Pack output via symlink into canonical bundle: want error")
	}
}
