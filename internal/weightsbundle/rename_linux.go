// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
//go:build linux

package weightsbundle

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace atomically publishes staging at dest, failing if dest
// exists, exactly as experiments/local-ml/train.py does with RENAME_NOREPLACE.
func renameNoReplace(staging, dest string) error {
	err := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
		return os.ErrExist
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return fmt.Errorf("%w: filesystem does not support no-replace rename", ErrPublicationIndeterminate)
	}
	return fmt.Errorf("%w: %v", ErrPublicationIndeterminate, err)
}
