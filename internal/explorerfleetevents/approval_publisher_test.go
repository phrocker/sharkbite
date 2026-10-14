// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
)

// TestApprovalEventWindowIsTheBackendBoundLessSkew: an approval event's
// RetryUntil is its transition's UpdatedAt plus ApprovalEventRetryWindow. The
// backend refuses one further ahead of its clock than MaxMutationRetryWindow,
// and stops deduplicating its token after it. So the window plus the skew it
// absorbs must be exactly the bound. Longer, and a lagging replica could
// never publish. Shorter, and a row lapses while the backend could still
// have deduplicated it.
func TestApprovalEventWindowIsTheBackendBoundLessSkew(t *testing.T) {
	if fleet.ApprovalEventRetryWindow+fleet.ApprovalEventSkewAllowance !=
		fleetevents.MaxMutationRetryWindow {
		t.Fatalf("approval window %v + skew %v != backend bound %v",
			fleet.ApprovalEventRetryWindow, fleet.ApprovalEventSkewAllowance,
			fleetevents.MaxMutationRetryWindow)
	}
}
