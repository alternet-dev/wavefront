//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos || windows)

package bundlegen

import "fmt"

// Pack is not implemented on this platform.
func Pack(_, _ string) error {
	return fmt.Errorf("Pack is not supported on this platform")
}
