package bundletest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
)

func TestDirWritesALayerSubdirectory(t *testing.T) {
	dir := bundletest.Dir(t, "")
	// The three bundle files must NOT sit directly in dir...
	if _, err := os.Stat(filepath.Join(dir, "descriptors.binpb")); err == nil {
		t.Fatal("descriptors.binpb is in the bundle root; expected it inside a layer subdirectory")
	}
	// ...and the bundle must load.
	if _, err := bundle.Load(dir); err != nil {
		t.Fatalf("Load on bundletest.Dir output: %v", err)
	}
}

func TestMultiDirWritesEachLayer(t *testing.T) {
	dir := bundletest.MultiDir(t,
		bundletest.Layer{Name: "2024-11", Descriptors: bundletest.FDSBytes(t), OpenAPI: bundletest.ValidOpenAPI, Versions: bundletest.ValidVersions},
	)
	if _, err := os.Stat(filepath.Join(dir, "2024-11", "versions.yaml")); err != nil {
		t.Fatalf("layer 2024-11/versions.yaml missing: %v", err)
	}
}
