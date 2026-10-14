// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package teamoverview

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestFleetAuthorizationCoversEveryFleetOperation pins the classifier against
// the authorization vocabulary rather than against a list someone remembered to
// update.
//
// A lifecycle audit's AuthorizationOperation decides whether the activity feed
// treats it as fleet activity or as a user interaction. OperationExecute was
// missing, so every claim or completion authorized under it — the whole point
// of #437 — was classified as a user interaction and surfaced beside its own
// fleet_action entry.
//
// Written as a complete partition of the operations an action's lifecycle can
// carry, so adding a fleet operation without classifying it fails here.
func TestFleetAuthorizationCoversEveryFleetOperation(t *testing.T) {
	fleetOperations := []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke, auth.OperationExecute,
		auth.OperationActionApprove,
		auth.OperationAgentRegister, auth.OperationAgentHeartbeat,
		auth.OperationAgentRevoke, auth.OperationAgentResolve,
		auth.OperationEventPublish,
	}
	for _, operation := range fleetOperations {
		if !isFleetAuthorization(string(operation)) {
			t.Errorf("isFleetAuthorization(%q) = false; a fleet lifecycle "+
				"audit under it is classified as a user interaction and "+
				"surfaces in the activity feed twice", operation)
		}
	}
	// And the classifier is not vacuous: an operation that is not part of an
	// action's lifecycle must stay out, or it would absorb unrelated audits.
	for _, operation := range []auth.Operation{
		auth.OperationRead, auth.OperationList, auth.OperationIngest,
		auth.OperationTeamOverviewRead, auth.Operation("not-an-operation"),
	} {
		if isFleetAuthorization(string(operation)) {
			t.Errorf("isFleetAuthorization(%q) = true, want false", operation)
		}
	}
}
