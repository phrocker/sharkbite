// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package register is round-2 bypass 2 of the authority review: a generic
// function whose constraint names Register, instantiated with the fleet
// registry. Naming *fleet.Service requires importing fleet, which the import
// rule refuses for every guarded package.
package register

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

type registrar interface {
	Register(context.Context, fleet.RegisterRequest) (fleet.Descriptor, error)
}

func call[T registrar](ctx context.Context, r T) {
	_, _ = r.Register(ctx, fleet.RegisterRequest{})
}

func Bypass(ctx context.Context, s *fleet.Service) { call(ctx, s) }
