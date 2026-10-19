package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type testArchiveEntry struct {
	name     string
	data     []byte
	typeflag byte
}

func writeTestArchive(t *testing.T, entries []testArchiveEntry) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "bundle.tar.gz")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.data)), Typeflag: typeflag}
		if typeflag == tar.TypeSymlink {
			hdr.Size = 0
			hdr.Linkname = "target"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", entry.name, err)
		}
		if len(entry.data) > 0 && typeflag == tar.TypeReg {
			if _, err := tw.Write(entry.data); err != nil {
				t.Fatalf("write entry %q: %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return filename
}

func TestLoadPackedArchive(t *testing.T) {
	descriptors := fdsBytes(t)
	archive := writeTestArchive(t, []testArchiveEntry{
		{name: "layer/", typeflag: tar.TypeDir},
		{name: "layer/descriptors.binpb", data: descriptors},
		{name: "layer/openapi.json", data: []byte(validOpenAPI)},
		{name: "layer/versions.yaml", data: []byte(validVersions)},
	})

	b, err := Load(archive)
	if err != nil {
		t.Fatalf("Load packed archive: %v", err)
	}
	c, ok := b.Contract("2024-11")
	if !ok {
		t.Fatal(`Contract("2024-11") not found`)
	}
	if c.Route() != "/v3/me/session" || c.Method() != "GET" {
		t.Errorf("route/method = %q %q", c.Route(), c.Method())
	}
	raw, err := ReadFile(archive, "layer/openapi.json")
	if err != nil {
		t.Fatalf("ReadFile from packed archive: %v", err)
	}
	if string(raw) != validOpenAPI {
		t.Errorf("openapi.json = %q, want %q", raw, validOpenAPI)
	}
}

func TestLoadRejectsArchiveWithBadGzipTrailer(t *testing.T) {
	archive := writeTestArchive(t, []testArchiveEntry{{name: "file", data: []byte("content")}})
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(archive, raw, 0o600); err != nil {
		t.Fatalf("corrupt archive: %v", err)
	}
	_, err = Load(archive)
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		t.Fatalf("Load corrupt trailer error = %v, want ArchiveError", err)
	}
}

func TestLoadRejectsMalformedArchive(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(filename, []byte("not gzip"), 0o600); err != nil {
		t.Fatalf("write malformed archive: %v", err)
	}
	_, err := Load(filename)
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		t.Fatalf("Load malformed archive error = %v, want ArchiveError", err)
	}
}

func TestLoadRejectsUnsafeArchiveEntries(t *testing.T) {
	tests := []struct {
		name  string
		entry testArchiveEntry
	}{
		{name: "path traversal", entry: testArchiveEntry{name: "../escape", data: []byte("x")}},
		{name: "absolute path", entry: testArchiveEntry{name: "/escape", data: []byte("x")}},
		{name: "symlink", entry: testArchiveEntry{name: "layer/link", typeflag: tar.TypeSymlink}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive := writeTestArchive(t, []testArchiveEntry{tc.entry})
			_, err := Load(archive)
			var archiveErr *ArchiveError
			if !errors.As(err, &archiveErr) {
				t.Fatalf("Load error = %v, want ArchiveError", err)
			}
		})
	}
}

func TestLoadRejectsDuplicateArchiveEntry(t *testing.T) {
	archive := writeTestArchive(t, []testArchiveEntry{
		{name: "layer/openapi.json", data: []byte("first")},
		{name: "layer/openapi.json", data: []byte("second")},
	})
	_, err := Load(archive)
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		t.Fatalf("Load duplicate entries error = %v, want ArchiveError", err)
	}
}
