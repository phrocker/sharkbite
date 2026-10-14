//go:build !windows

package deployment

import "os"

func replaceFile(from, to string) error {
	return os.Rename(from, to)
}
