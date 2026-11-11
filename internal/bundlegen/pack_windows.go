//go:build windows

package bundlegen

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Pack writes a deterministic archive on Windows. Windows does not expose
// directory-descriptor operations through the standard library, so it rejects
// link entries and checks the output's existing ancestors before writing it.
func Pack(bundleDir, output string) error {
	if err := Verify(bundleDir); err != nil {
		return fmt.Errorf("verify source bundle: %w", err)
	}
	if err := outputOutsideBundle(bundleDir, output); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := outputOutsideBundle(bundleDir, output); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".wavefront-bundle-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writePackedBundle(tmp, bundleDir); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := Verify(tmp.Name()); err != nil {
		return fmt.Errorf("verify packed archive: %w", err)
	}
	if err := os.Rename(tmp.Name(), output); err != nil {
		return fmt.Errorf("install archive: %w", err)
	}
	return nil
}

func outputOutsideBundle(bundleDir, output string) error {
	bundle, err := filepath.EvalSymlinks(bundleDir)
	if err != nil {
		return fmt.Errorf("resolve bundle directory: %w", err)
	}
	bi, err := os.Stat(bundle)
	if err != nil {
		return fmt.Errorf("stat bundle directory: %w", err)
	}
	p, err := filepath.Abs(filepath.Dir(output))
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	for {
		if _, e := os.Stat(p); e == nil {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				return fmt.Errorf("resolve output directory: %w", err)
			}
			inside, err := pathWithin(resolved, bi)
			if err != nil {
				return fmt.Errorf("check archive output containment: %w", err)
			}
			if inside {
				return fmt.Errorf("archive output must be outside the canonical bundle directory")
			}
		}
		n := filepath.Dir(p)
		if n == p {
			break
		}
		p = n
	}
	return nil
}

func pathWithin(dir string, ancestor os.FileInfo) (bool, error) {
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		if os.SameFile(ancestor, info) {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

type packedFile struct {
	name string
	info fs.FileInfo
}

func writePackedBundle(dst *os.File, bundleDir string) error {
	var files []packedFile
	var totalBytes int64
	var entryCount int
	err := filepath.WalkDir(bundleDir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entryCount++
		if entryCount > maxPackEntries {
			return fmt.Errorf("bundle has more than %d entries", maxPackEntries)
		}
		if name != bundleDir && !entry.IsDir() {
			info, e := entry.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("bundle entry %s is not a regular file", name)
			}
			rel, _ := filepath.Rel(bundleDir, name)
			relSlash := filepath.ToSlash(rel)
			if len(relSlash) > maxPackPathBytes {
				return fmt.Errorf("bundle entry path %s exceeds %d bytes", relSlash, maxPackPathBytes)
			}
			if info.Size() > maxPackBytes || totalBytes > maxPackBytes-info.Size() {
				return fmt.Errorf("bundle exceeds %d bytes", maxPackBytes)
			}
			totalBytes += info.Size()
			files = append(files, packedFile{name: name, info: info})
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	gz := gzip.NewWriter(dst)
	gz.ModTime, gz.OS = time.Time{}, 255
	tw := tar.NewWriter(gz)
	for _, file := range files {
		f, err := os.Open(file.name)
		if err != nil {
			return err
		}
		info, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return statErr
		}
		if !info.Mode().IsRegular() || !os.SameFile(file.info, info) {
			f.Close()
			return fmt.Errorf("bundle entry %s changed while packing", file.name)
		}
		rel, _ := filepath.Rel(bundleDir, file.name)
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: time.Time{}, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			f.Close()
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
