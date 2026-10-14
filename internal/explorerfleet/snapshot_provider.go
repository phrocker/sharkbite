// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// InteractionSnapshotProvider exposes trusted snapshots from the host-owned
// durable corpus without routing the already-authorized lifecycle operation
// through a separate Retrieve authorization check.
type InteractionSnapshotProvider struct {
	corpus *explorer.Explorer
}

// NewInteractionSnapshotProvider binds lifecycle snapshot acquisition to the
// same concrete durable corpus used by the host.
func NewInteractionSnapshotProvider(
	corpus *explorer.Explorer,
) (*InteractionSnapshotProvider, error) {
	if corpus == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet interaction snapshot corpus is required",
		)
	}
	return &InteractionSnapshotProvider{corpus: corpus}, nil
}

// InteractionSnapshot returns the corpus's current authoritative snapshot.
func (p *InteractionSnapshotProvider) InteractionSnapshot(
	ctx context.Context,
) (explorer.Snapshot, error) {
	if p == nil || p.corpus == nil {
		return explorer.Snapshot{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet interaction snapshot provider is required",
		)
	}
	return p.corpus.Snapshot(ctx)
}

var _ fleet.InteractionSnapshotProvider = (*InteractionSnapshotProvider)(nil)
