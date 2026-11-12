package bundlegen_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

// packMaxDepth mirrors the packer's internal maxPackDepth constant. A failure
// here means the public contract — the deepest tree Pack will accept — has
// changed and both this and the loader must be re-audited.
const packMaxDepth = 32

func TestPackRefusesTreeOneLevelDeeperThanLimit(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Normal bundle files live at depth 2 (bundle/layer/file). Build an extra
	// chain of empty subdirectories inside the single existing layer so the
	// deepest directory ends up one level past the packer's declared limit.
	layerDir := filepath.Join(bundleDir, "2026-05-17")
	addNestedDirs(t, layerDir, packMaxDepth) // layer depth 1 + packMaxDepth = packMaxDepth+1 at the leaf

	outDir := t.TempDir()
	err := bundlegen.Pack(bundleDir, filepath.Join(outDir, "bundle.tar.gz"))
	if err == nil {
		t.Fatalf("Pack over-deep tree: want error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", packMaxDepth)) {
		t.Fatalf("Pack over-deep tree: error %q does not name the limit %d", err.Error(), packMaxDepth)
	}
}

func TestPackAtMaxDepthLoadsPackedArchive(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The deepest directory ends up exactly at the packer's limit: layer
	// depth 1 + (packMaxDepth-1) nested subdirectories = packMaxDepth.
	layerDir := filepath.Join(bundleDir, "2026-05-17")
	addNestedDirs(t, layerDir, packMaxDepth-1)

	outDir := t.TempDir()
	archive := filepath.Join(outDir, "bundle.tar.gz")
	if err := bundlegen.Pack(bundleDir, archive); err != nil {
		t.Fatalf("Pack at max depth: %v", err)
	}
	b, err := bundle.Load(archive)
	if err != nil {
		t.Fatalf("Load packed archive at max depth: %v", err)
	}
	if _, ok := b.Contract("2026-05-17"); !ok {
		t.Fatal(`Contract("2026-05-17") not found in packed archive`)
	}
}

// addNestedDirs creates a chain of n single-child empty directories rooted at
// parent, so parent/d/d/.../d exists with n segments beyond parent.
func addNestedDirs(t *testing.T, parent string, n int) {
	t.Helper()
	cur := parent
	for i := 0; i < n; i++ {
		cur = filepath.Join(cur, "d")
		if err := os.Mkdir(cur, 0o755); err != nil {
			t.Fatalf("create nested dir level %d: %v", i+1, err)
		}
	}
}
