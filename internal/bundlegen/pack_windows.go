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
		if i, e := os.Stat(p); e == nil && os.SameFile(bi, i) {
			return fmt.Errorf("archive output must be outside the canonical bundle directory")
		}
		n := filepath.Dir(p)
		if n == p {
			break
		}
		p = n
	}
	return nil
}

func writePackedBundle(dst *os.File, bundleDir string) error {
	var names []string
	err := filepath.WalkDir(bundleDir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name != bundleDir && !entry.IsDir() {
			info, e := entry.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("bundle entry %s is not a regular file", name)
			}
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	gz := gzip.NewWriter(dst)
	gz.ModTime, gz.OS = time.Time{}, 255
	tw := tar.NewWriter(gz)
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(bundleDir, name)
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: time.Time{}, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		f.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
