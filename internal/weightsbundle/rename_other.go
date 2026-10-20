// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
//go:build !linux

package weightsbundle

import "fmt"

// renameNoReplace fails closed where the kernel offers no atomic no-replace
// rename: a bundle is never published by a rename that could overwrite.
func renameNoReplace(staging, dest string) error {
	return fmt.Errorf("%w: atomic no-replace directory publication is only supported on linux", ErrPublicationIndeterminate)
}
