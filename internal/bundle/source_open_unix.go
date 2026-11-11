//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package bundle

import (
	"os"

	"golang.org/x/sys/unix"
)

// openBundleFile avoids blocking on a FIFO substituted for an archive before
// openSource can reject it as a non-regular file.
func openBundleFile(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
