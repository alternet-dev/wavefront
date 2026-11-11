//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package bundlegen

import (
	"archive/tar"
	"compress/gzip"
	cryptorand "crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Pack writes a deterministic archive without reopening source pathnames or
// installing through an unpinned destination directory.
func Pack(bundleDir, output string) error {
	root, err := openDir(bundleDir)
	if err != nil {
		return fmt.Errorf("open bundle directory: %w", err)
	}
	defer unix.Close(root)
	if err := outputOutsideBundle(bundleDir, output); err != nil {
		return err
	}
	if err := Verify(bundleDir); err != nil {
		return fmt.Errorf("verify source bundle: %w", err)
	}
	tmp, err := os.CreateTemp("", ".wavefront-bundle-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := writePackedBundle(tmp, root); err != nil {
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
	if err := Verify(tmp.Name()); err != nil {
		return fmt.Errorf("verify packed archive: %w", err)
	}
	return installArchive(root, tmp.Name(), output)
}

func openDir(name string) (int, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return -1, err
	}
	return unix.Open(resolved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
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

func writePackedBundle(dst *os.File, root int) error {
	gz, err := gzip.NewWriterLevel(dst, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.ModTime, gz.OS = time.Time{}, 255
	tw := tar.NewWriter(gz)
	if err := writeDir(tw, root, ""); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish gzip archive: %w", err)
	}
	return nil
}

func writeDir(tw *tar.Writer, dir int, prefix string) error {
	copyFD, err := unix.Dup(dir)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(copyFD), "bundle directory")
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return fmt.Errorf("read bundle directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		fd, err := unix.Openat(dir, entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open bundle entry %s: %w", entry.Name(), err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return err
		}
		name := prefix + entry.Name()
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			err = writeDir(tw, fd, name+"/")
			unix.Close(fd)
			if err != nil {
				return err
			}
			continue
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			unix.Close(fd)
			return fmt.Errorf("bundle entry %s is not a regular file", name)
		}
		src := os.NewFile(uintptr(fd), name)
		h := &tar.Header{Name: name, Mode: int64(st.Mode & 0o777), Size: st.Size, ModTime: time.Time{}, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			src.Close()
			return err
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func installArchive(root int, source, output string) error {
	abs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	parentPath, base := filepath.Dir(abs), filepath.Base(abs)
	if err := os.MkdirAll(parentPath, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	parent, err := openDir(parentPath)
	if err != nil {
		return fmt.Errorf("open output directory: %w", err)
	}
	defer unix.Close(parent)
	if withinDir(parent, root) {
		return fmt.Errorf("archive output must be outside the canonical bundle directory")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	var id [12]byte
	if _, err := cryptorand.Read(id[:]); err != nil {
		return err
	}
	name := fmt.Sprintf(".wavefront-bundle-%x.tar.gz", id)
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	tmp := os.NewFile(uintptr(fd), name)
	_, copyErr := io.Copy(tmp, input)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = unix.Unlinkat(parent, name, 0)
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	if err := unix.Renameat(parent, name, parent, base); err != nil {
		_ = unix.Unlinkat(parent, name, 0)
		return fmt.Errorf("install archive: %w", err)
	}
	return nil
}

func withinDir(dir, ancestor int) bool {
	a, err := unix.Dup(ancestor)
	if err != nil {
		return false
	}
	defer unix.Close(a)
	af := os.NewFile(uintptr(a), "bundle")
	ai, err := af.Stat()
	af.Close()
	if err != nil {
		return false
	}
	c, err := unix.Dup(dir)
	if err != nil {
		return false
	}
	defer func() { unix.Close(c) }()
	for {
		f := os.NewFile(uintptr(c), "directory")
		ci, err := f.Stat()
		f.Close()
		if err != nil {
			return false
		}
		if os.SameFile(ai, ci) {
			return true
		}
		p, err := unix.Openat(c, "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false
		}
		pf := os.NewFile(uintptr(p), "parent")
		pi, e := pf.Stat()
		pf.Close()
		if e != nil || os.SameFile(pi, ci) {
			unix.Close(p)
			return false
		}
		unix.Close(c)
		c = p
	}
}
