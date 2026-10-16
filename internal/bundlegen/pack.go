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
	"strings"
	"time"
)

// Pack verifies the canonical bundle directory at bundleDir and writes a
// deterministic gzip-compressed tar archive to output. The source directory
// is never modified; the archive is a derived delivery artifact.
func Pack(bundleDir, output string) error {
	info, err := os.Stat(bundleDir)
	if err != nil {
		return fmt.Errorf("stat bundle directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("pack source %s is not a bundle directory", bundleDir)
	}
	if err := outputOutsideBundle(bundleDir, output); err != nil {
		return err
	}
	if err := Verify(bundleDir); err != nil {
		return fmt.Errorf("verify source bundle: %w", err)
	}

	entries, err := packEntries(bundleDir)
	if err != nil {
		return err
	}
	outputDir := filepath.Dir(output)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tmp, err := os.CreateTemp(outputDir, ".wavefront-bundle-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := writePackedBundle(tmp, bundleDir, entries); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("set archive permissions: %w", err)
	}
	if err := Verify(tmpName); err != nil {
		return fmt.Errorf("verify packed archive: %w", err)
	}
	if err := os.Rename(tmpName, output); err != nil {
		return fmt.Errorf("install archive: %w", err)
	}
	return nil
}

func outputOutsideBundle(bundleDir, output string) error {
	bundleAbs, err := filepath.Abs(bundleDir)
	if err != nil {
		return fmt.Errorf("resolve bundle directory: %w", err)
	}
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	rel, err := filepath.Rel(bundleAbs, outputAbs)
	if err != nil {
		return fmt.Errorf("compare bundle and output paths: %w", err)
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return fmt.Errorf("archive output must be outside the canonical bundle directory")
	}
	return nil
}

func packEntries(bundleDir string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(bundleDir, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == bundleDir || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundle entry %s is not a regular file", filename)
		}
		entries = append(entries, filename)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk bundle directory: %w", err)
	}
	sort.Strings(entries)
	return entries, nil
}

func writePackedBundle(dst *os.File, bundleDir string, entries []string) error {
	gz, err := gzip.NewWriterLevel(dst, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	gz.ModTime = time.Time{}
	gz.OS = 255
	tw := tar.NewWriter(gz)

	for _, filename := range entries {
		info, err := os.Stat(filename)
		if err != nil {
			return fmt.Errorf("stat bundle entry: %w", err)
		}
		rel, err := filepath.Rel(bundleDir, filename)
		if err != nil {
			return fmt.Errorf("resolve bundle entry: %w", err)
		}
		hdr := &tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			ModTime:  time.Time{},
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write archive header for %s: %w", rel, err)
		}
		src, err := os.Open(filename)
		if err != nil {
			return fmt.Errorf("open bundle entry %s: %w", rel, err)
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return fmt.Errorf("write bundle entry %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close bundle entry %s: %w", rel, closeErr)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish gzip archive: %w", err)
	}
	return nil
}
