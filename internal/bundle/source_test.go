package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testArchiveEntry struct {
	name     string
	data     []byte
	typeflag byte
}

func validArchiveEntries(t *testing.T) []testArchiveEntry {
	t.Helper()
	return []testArchiveEntry{
		{name: "layer/", typeflag: tar.TypeDir},
		{name: "layer/descriptors.binpb", data: fdsBytes(t)},
		{name: "layer/openapi.json", data: []byte(validOpenAPI)},
		{name: "layer/versions.yaml", data: []byte(validVersions)},
	}
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

func TestReadFilesReadsFromOneArchiveSnapshot(t *testing.T) {
	archive := writeTestArchive(t, validArchiveEntries(t))
	files, err := ReadFiles(archive, "layer/openapi.json", "layer/versions.yaml")
	if err != nil {
		t.Fatalf("ReadFiles from packed archive: %v", err)
	}
	if len(files) != 2 || string(files[0]) != validOpenAPI || string(files[1]) != validVersions {
		t.Fatalf("ReadFiles = %q, want OpenAPI and versions documents", files)
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

func TestLoadRejectsRegularFileWithTrailingSlash(t *testing.T) {
	archive := writeTestArchive(t, []testArchiveEntry{{name: "layer/openapi.json", data: []byte("x")}})
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	tarBytes, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	copy(tarBytes, []byte("layer/openapi.json/"))
	for i := 148; i < 156; i++ {
		tarBytes[i] = ' '
	}
	var checksum int
	for _, b := range tarBytes[:512] {
		checksum += int(b)
	}
	copy(tarBytes[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	var rewritten bytes.Buffer
	zw := gzip.NewWriter(&rewritten)
	if _, err := zw.Write(tarBytes); err != nil {
		t.Fatalf("rewrite gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close rewritten gzip: %v", err)
	}
	if err := os.WriteFile(archive, rewritten.Bytes(), 0o600); err != nil {
		t.Fatalf("write rewritten archive: %v", err)
	}
	_, err = Load(archive)
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		t.Fatalf("Load trailing-slash regular file error = %v, want ArchiveError", err)
	}
}

func TestLoadRejectsTrailingGarbageAfterGzipMember(t *testing.T) {
	archive := writeTestArchive(t, validArchiveEntries(t))
	f, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	if _, err := f.Write([]byte("not a gzip member")); err != nil {
		t.Fatalf("append garbage: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	_, err = Load(archive)
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		t.Fatalf("Load trailing garbage error = %v, want ArchiveError", err)
	}
}

func TestLoadCountsImplicitDirectoriesTowardArchiveEntryLimit(t *testing.T) {
	entries := validArchiveEntries(t)
	for i := 0; i < 51; i++ {
		// Each name is within maxArchivePathBytes, but their disjoint parents
		// exceed maxArchiveEntries when materialized in memory.
		entries = append(entries, testArchiveEntry{
			name: "layer/" + fmt.Sprintf("p%03d", i) + strings.Repeat("/d", 2_000) + "/file",
			data: []byte("x"),
		})
	}
	archive := writeTestArchive(t, entries)
	_, err := Load(archive)
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		t.Fatalf("Load archive with excessive implicit directories error = %v, want ArchiveError", err)
	}
}
