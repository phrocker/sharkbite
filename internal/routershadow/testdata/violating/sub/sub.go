// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package sub is a violating subpackage of the authority-check fixture: a new
// package under a guarded tree must be checked without being named.
package sub

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// Complete reports a claimed action as finished.
func Complete(ctx context.Context, s *fleet.DispatchService) {
	_, _ = s.CompleteClaim(ctx, fleet.CompletionRequest{})
}
