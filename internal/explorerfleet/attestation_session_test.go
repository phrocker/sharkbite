// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// TestRefusalSessionsAreDistinctPerRefusingRequest: a refusal writes no
// version, so the refusing request is part of the refusal phase's session
// identity — and only that phase's, so every other session ID is unchanged.
func TestRefusalSessionsAreDistinctPerRefusingRequest(t *testing.T) {
	record := testActionRecord()
	one, two := record, record
	one.TransitionRequestID, two.TransitionRequestID = "refused-1", "refused-2"
	audit := func(phase string, r fleet.ActionRecord) fleet.ActionAudit {
		return fleet.ActionAudit{Phase: phase, Operation: auth.OperationExecute, Record: r}
	}
	if actionSessionID(audit(fleet.ClaimRefusedAttestationPhase, one)) ==
		actionSessionID(audit(fleet.ClaimRefusedAttestationPhase, two)) {
		t.Fatal("two refusals at one version share a session")
	}
	if actionSessionID(audit("claim_admission", one)) != actionSessionID(audit("claim_admission", two)) {
		t.Fatal("another phase's session identity changed")
	}
}
