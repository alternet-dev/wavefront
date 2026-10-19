package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// maxArchiveBytes bounds the memory committed before a packed bundle has
// passed schema validation. Bundle archives are operator-owned input, but a
// corrupt gzip size or tar header must still fail rather than exhaust the
// process. A canonical bundle above this size can still be loaded as a
// directory.
const (
	maxArchiveBytes     int64 = 1 << 30
	maxArchiveEntries         = 100_000
	maxArchivePathBytes       = 4 << 10
)

// ArchiveError reports a packed bundle that cannot be decompressed or whose
// tar shape is unsafe or unsupported.
type ArchiveError struct {
	File string
	Err  error
}

func (e *ArchiveError) Error() string {
	return fmt.Sprintf("read bundle archive %s: %v", e.File, e.Err)
}
func (e *ArchiveError) Unwrap() error { return e.Err }

// openSource returns a filesystem rooted at bundlePath. Directories are read
// in place. Regular files are gzip-compressed tar archives and are expanded
// into an in-memory filesystem once, before the normal loader runs.
type bundleSource interface {
	readFile(name string) ([]byte, error)
	readDir(name string) ([]fs.DirEntry, error)
}

type directorySource struct{ fs.FS }

func (s directorySource) readFile(name string) ([]byte, error) { return fs.ReadFile(s.FS, name) }
func (s directorySource) readDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(s.FS, name)
}

type memorySource struct {
	files map[string][]byte
	dirs  map[string]struct{}
}

func (s *memorySource) readFile(name string) ([]byte, error) {
	data, ok := s.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return data, nil
}

func (s *memorySource) readDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		if _, ok := s.dirs[name]; !ok {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
		}
	}
	var entries []fs.DirEntry
	for dirname := range s.dirs {
		if path.Dir(dirname) == name {
			entries = append(entries, memoryDirEntry{name: path.Base(dirname), dir: true})
		}
	}
	for filename, data := range s.files {
		if path.Dir(filename) == name {
			entries = append(entries, memoryDirEntry{name: path.Base(filename), size: int64(len(data))})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

type memoryDirEntry struct {
	name string
	dir  bool
	size int64
}

func (e memoryDirEntry) Name() string      { return e.name }
func (e memoryDirEntry) IsDir() bool       { return e.dir }
func (e memoryDirEntry) Type() fs.FileMode { return e.Mode().Type() }
func (e memoryDirEntry) Info() (fs.FileInfo, error) {
	return e, nil
}
func (e memoryDirEntry) Size() int64        { return e.size }
func (e memoryDirEntry) ModTime() time.Time { return time.Time{} }
func (e memoryDirEntry) Sys() any           { return nil }
func (e memoryDirEntry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func openSource(bundlePath string) (bundleSource, error) {
	info, err := os.Stat(bundlePath)
	if err != nil {
		return nil, &ReadError{File: bundlePath, Err: err}
	}
	if info.IsDir() {
		return directorySource{os.DirFS(bundlePath)}, nil
	}
	if !info.Mode().IsRegular() {
		return nil, &ReadError{File: bundlePath, Err: errors.New("bundle path is neither a directory nor a regular archive file")}
	}
	return readArchive(bundlePath)
}

func readArchive(filename string) (bundleSource, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, &ArchiveError{File: filename, Err: err}
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, &ArchiveError{File: filename, Err: fmt.Errorf("gzip header: %w", err)}
	}
	defer gz.Close()
	gz.Multistream(false)

	source := &memorySource{files: map[string][]byte{}, dirs: map[string]struct{}{}}
	seen := map[string]struct{}{}
	tr := tar.NewReader(gz)
	var total int64
	entryCount := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &ArchiveError{File: filename, Err: fmt.Errorf("tar stream: %w", err)}
		}
		entryCount++
		if entryCount > maxArchiveEntries {
			return nil, &ArchiveError{File: filename, Err: fmt.Errorf("archive contains more than %d entries", maxArchiveEntries)}
		}

		name, err := archivePath(hdr.Name)
		if err != nil {
			return nil, &ArchiveError{File: filename, Err: err}
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, &ArchiveError{File: filename, Err: fmt.Errorf("duplicate archive entry %q", hdr.Name)}
		}
		seen[name] = struct{}{}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if _, exists := source.files[name]; exists {
				return nil, &ArchiveError{File: filename, Err: fmt.Errorf("archive entry %q conflicts with a file", hdr.Name)}
			}
			source.dirs[name] = struct{}{}
		case tar.TypeReg, 0: // NUL is the legacy tar type for a regular file.
			if _, exists := source.dirs[name]; exists {
				return nil, &ArchiveError{File: filename, Err: fmt.Errorf("archive entry %q conflicts with a directory", hdr.Name)}
			}
			if hdr.Size < 0 || hdr.Size > maxArchiveBytes-total {
				return nil, &ArchiveError{File: filename, Err: fmt.Errorf("archive expands beyond %d bytes", maxArchiveBytes)}
			}
			data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
			if err != nil {
				return nil, &ArchiveError{File: filename, Err: fmt.Errorf("read %q: %w", hdr.Name, err)}
			}
			if int64(len(data)) != hdr.Size {
				return nil, &ArchiveError{File: filename, Err: fmt.Errorf("entry %q declared %d bytes but contained %d", hdr.Name, hdr.Size, len(data))}
			}
			total += hdr.Size
			source.files[name] = data
		default:
			return nil, &ArchiveError{File: filename, Err: fmt.Errorf("archive entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)}
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := source.files[parent]; exists {
				return nil, &ArchiveError{File: filename, Err: fmt.Errorf("archive entry %q has file parent %q", hdr.Name, parent)}
			}
			source.dirs[parent] = struct{}{}
		}
	}
	// tar.Reader stops at the archive's end markers. Drain the gzip member so
	// its checksum and trailer are validated before the source is accepted.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, &ArchiveError{File: filename, Err: fmt.Errorf("gzip trailer: %w", err)}
	}
	if err := gz.Close(); err != nil {
		return nil, &ArchiveError{File: filename, Err: fmt.Errorf("gzip trailer: %w", err)}
	}
	return source, nil
}

// archivePath accepts only canonical, relative slash-separated paths. This
// rejects traversal, absolute paths, platform-specific separators, and two
// spellings that could otherwise address the same in-memory file.
func archivePath(name string) (string, error) {
	if len(name) > maxArchivePathBytes {
		return "", fmt.Errorf("archive entry path exceeds %d bytes", maxArchivePathBytes)
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.Contains(trimmed, "\\") || path.IsAbs(trimmed) {
		return "", fmt.Errorf("archive entry has invalid path %q", name)
	}
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != trimmed {
		return "", fmt.Errorf("archive entry has invalid path %q", name)
	}
	return clean, nil
}

// ReadFile reads a file relative to a canonical bundle directory or packed
// archive. It is used by bundle tooling that needs source documents without
// duplicating the archive reader.
func ReadFile(bundlePath, name string) ([]byte, error) {
	source, err := openSource(bundlePath)
	if err != nil {
		return nil, err
	}
	data, err := source.readFile(name)
	if err != nil {
		return nil, &ReadError{File: name, Err: err}
	}
	return data, nil
}
