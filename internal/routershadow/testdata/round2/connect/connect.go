// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package connect is round-2 bypass 1 of the authority review: assert the
// authorized client held in the shadow router's Config to an interface with a
// write method. The router's Config no longer holds a client, so this does not
// compile; the same assertion on what it does hold (a routerwire wrapper)
// compiles but finds nothing, which internal/routerwire's tests show.
package connect

import (
	"context"

	"github.com/phrocker/shoal-oss/internal/routershadow"
	"github.com/phrocker/shoal-oss/pkg/graph"
)

func Bypass(ctx context.Context, config routershadow.Config) error {
	return any(config.Client).(interface {
		Connect(context.Context, graph.Edge) error
	}).Connect(ctx, graph.Edge{})
}
