//go:build windows || plan9 || js || wasip1

package bundle

import "os"

func openBundleFile(name string) (*os.File, error) {
	return os.Open(name)
}
