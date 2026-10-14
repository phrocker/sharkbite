//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris || plan9

package local

import (
	"os"
	"path/filepath"
)

func (osReplacementOps) SyncPath(path string) error {
	return syncOpenPath(path)
}

func (osReplacementOps) SyncParent(path string) error {
	return syncOpenPath(filepath.Dir(path))
}

func syncOpenPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
