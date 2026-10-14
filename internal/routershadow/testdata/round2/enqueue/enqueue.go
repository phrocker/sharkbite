// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package enqueue is round-2 bypass 3 of the authority review: the same
// generic shape instantiated with the dispatch service. Refused by the import
// rule, as for register.
package enqueue

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

type enqueuer interface {
	Enqueue(context.Context, fleet.EnqueueRequest) (fleet.ActionRecord, error)
}

func call[T enqueuer](ctx context.Context, e T) {
	_, _ = e.Enqueue(ctx, fleet.EnqueueRequest{})
}

func Bypass(ctx context.Context, s *fleet.DispatchService) { call(ctx, s) }
