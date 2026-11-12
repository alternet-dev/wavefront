package bundlegen_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

// packMaxPathBytes mirrors the loader's archive path limit.
const packMaxPathBytes = 4 << 10

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

func TestPackRefusesOutputViaSymlinkIntoBundleSubdirectory(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The output parent resolves below the bundle root rather than to the root
	// itself. It must still be rejected before archive installation can replace
	// a canonical bundle file.
	outsideDir := t.TempDir()
	linkPath := filepath.Join(outsideDir, "layer")
	if err := os.Symlink(filepath.Join(bundleDir, "2026-05-17"), linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := bundlegen.Pack(bundleDir, filepath.Join(linkPath, "versions.yaml")); err == nil {
		t.Fatal("Pack output via symlink into canonical bundle subdirectory: want error")
	}
}

func TestPackRefusesTreeBeyondPathLimit(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	const layer = "2026-05-17"
	// Use names below the usual 255-byte component limit so the entry path
	// crosses packMaxPathBytes in a small number of directory-relative steps.
	dirName := strings.Repeat("d", 240)
	depth := (packMaxPathBytes-len(layer))/(len(dirName)+1) + 1
	makeNestedDirs(t, filepath.Join(bundleDir, layer), depth, dirName)

	err := bundlegen.Pack(bundleDir, filepath.Join(t.TempDir(), "bundle.tar.gz"))
	if err == nil {
		t.Fatal("Pack tree beyond path limit: want error")
	}
	if !strings.Contains(err.Error(), "bundle entry path") || !strings.Contains(err.Error(), "exceeds 4096 bytes") {
		t.Fatalf("Pack tree beyond path limit: error %q does not report the path limit", err)
	}
}

func TestPackDeepTreeLoadsAndPreservesLeaf(t *testing.T) {
	in := writeOpenAPI(t, sampleOpenAPI)
	bundleDir := t.TempDir()
	if err := bundlegen.Add(in, bundleDir, false); err != nil {
		t.Fatalf("Add: %v", err)
	}

	const layer = "2026-05-17"
	want := []byte("deep leaf contents\n")
	leaf := makeNestedFile(t, filepath.Join(bundleDir, layer), 40, "d", "leaf.txt", want)

	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := bundlegen.Pack(bundleDir, archive); err != nil {
		t.Fatalf("Pack deep tree: %v", err)
	}
	if _, err := bundle.Load(archive); err != nil {
		t.Fatalf("Load packed deep tree: %v", err)
	}
	got, err := bundle.ReadFile(archive, layer+"/"+leaf)
	if err != nil {
		t.Fatalf("ReadFile packed deep leaf: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile packed deep leaf = %q, want %q", got, want)
	}
}

func makeNestedDirs(t *testing.T, parent string, depth int, dirName string) {
	t.Helper()
	makeNestedFile(t, parent, depth, dirName, "", nil)
}

func makeNestedFile(t *testing.T, parent string, depth int, dirName, name string, contents []byte) string {
	t.Helper()
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatalf("open nested tree parent: %v", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close nested tree directory: %v", err)
		}
	}()

	for range depth {
		if err := root.Mkdir(dirName, 0o755); err != nil {
			t.Fatalf("create nested directory: %v", err)
		}
		next, err := root.OpenRoot(dirName)
		if err != nil {
			t.Fatalf("open nested directory: %v", err)
		}
		if err := root.Close(); err != nil {
			next.Close()
			t.Fatalf("close parent nested directory: %v", err)
		}
		root = next
	}
	if name == "" {
		return ""
	}
	if err := root.WriteFile(name, contents, 0o600); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	return strings.Repeat(dirName+"/", depth) + name
}
