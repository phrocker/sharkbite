// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package authorized_test

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestFleetLifecycleReceiptsNeedNoRetrievePermission(t *testing.T) {
	f := newFixture(t)
	baseSnapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(baseSnapshot.AsOf.Add(time.Second))
	operations := []auth.Operation{
		auth.OperationAgentRegister,
		auth.OperationAgentHeartbeat,
		auth.OperationAgentRevoke,
		auth.OperationAgentResolve,
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "fleet-owner",
		Actor:                 "fleet-agent",
		ClientID:              "fleet-client",
		OnBehalfOf:            []shoal.ID{"delegating-agent"},
		AuthorizationDomain:   f.domain,
		AllowedOperations:     operations,
		PolicyGeneration:      1,
		AuthenticationExpires: f.clock.Now().Add(time.Hour),
		AuditPurpose:          "manage the durable agent registry",
		RequestID:             "fleet-request",
		CorrelationID:         "fleet-correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.context(t, decision)
	if err := decision.Authorize(
		auth.OperationRetrieve,
		auth.ResourceRequest{AuthorizationDomain: f.domain},
		f.clock.Now(),
	); err == nil {
		t.Fatal("action-only principal unexpectedly has retrieve permission")
	}
	recorder, err := explorerfleet.NewLifecycleRecorder(f.clientA)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	for index, operation := range operations {
		lifecycle := fleet.Lifecycle{
			Operation:                operation,
			RequestID:                shoal.ID(string(decision.RequestID()) + string(rune('a'+index))),
			CorrelationID:            decision.CorrelationID(),
			Subject:                  decision.Subject(),
			Actor:                    decision.Actor(),
			ClientID:                 decision.ClientID(),
			OnBehalfOf:               decision.OnBehalfOf(),
			AgentID:                  "registered-agent",
			Deadline:                 f.clock.Now().Add(time.Minute).UnixNano(),
			AuthorizationFingerprint: fingerprint,
			AuthorizationExpiresAt:   decision.AuthenticationExpires(),
			AuditPurpose:             decision.AuditPurpose(),
			SnapshotID:               shoal.ID(baseSnapshot.ID),
			SnapshotAsOf:             baseSnapshot.AsOf,
		}
		if err := recorder.RecordLifecycle(ctx, lifecycle); err != nil {
			t.Fatalf("%s receipt = %v", operation, err)
		}
	}
	summaries, err := f.base.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != len(operations) {
		t.Fatalf("persisted lifecycle receipts = %d, want %d", len(summaries), len(operations))
	}
	seen := make(map[string]bool, len(operations))
	for _, summary := range summaries {
		session, readErr := f.base.Interaction(context.Background(), summary.SessionID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		seen[session.AuthorizationOperation] = true
		if session.Actor.SubjectID != decision.Subject() ||
			session.Actor.ActorID != decision.Actor() ||
			session.Actor.ClientID != decision.ClientID() ||
			session.Reason.Code != "audit_purpose" ||
			session.Reason.Digest != interaction.Digest(decision.AuditPurpose()) ||
			len(session.TouchedNodeIDs()) != 0 {
			t.Fatalf("trusted lifecycle receipt = %#v", session)
		}
	}
	for _, operation := range operations {
		if !seen[string(operation)] {
			t.Errorf("missing %s lifecycle receipt", operation)
		}
	}
}
