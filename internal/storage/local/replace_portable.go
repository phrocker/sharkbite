//go:build plan9 || js || wasip1

package local

import (
	"errors"
	"fmt"
	"os"
)

// These platforms lack hard-link snapshots, so replacement cannot keep the
// destination continuously visible. We instead move the old file aside, rename
// the new file into place, and restore the old file if the second rename
// fails. A crash or ambiguous publish failure can therefore leave the reserved
// backup as the only surviving copy until recovery puts it back.
func platformAtomicReplace(temp, target, backup string, hadOld bool) error {
	if !hadOld {
		return os.Rename(temp, target)
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return errors.Join(
				err,
				fmt.Errorf("local: restore %s from %s after failed replacement: %w", target, backup, restoreErr),
			)
		}
		return err
	}
	return nil
}

func platformAtomicRestore(target, backup string) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(backup, target)
}
