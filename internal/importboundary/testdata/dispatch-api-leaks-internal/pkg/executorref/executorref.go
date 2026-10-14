// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// The dispatch contracts import executorref, so its closure is theirs: an
// internal import here leaks through the allowlist.
package executorref

import (
	_ "github.com/phrocker/shoal-oss/internal/secret"
)
