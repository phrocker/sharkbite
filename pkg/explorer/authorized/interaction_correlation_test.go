// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package authorized_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// correlationDecision is the fixture's retrieve decision carrying one
// correlation, and nothing else different, so two of them share an
// AuthorizationFingerprint (which excludes correlation by design).
func correlationDecision(
	t *testing.T, f *fixture, correlation shoal.ID,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "correlated",
		Actor:                 "correlated-actor",
		AuthorizationDomain:   f.domain,
		AllowedOperations:     []auth.Operation{auth.OperationRetrieve},
		PermittedSourceIDs:    [][]byte{f.sourceA},
		PermittedPolicyIDs:    [][]byte{f.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: f.clock.Now().Add(time.Hour),
		RequestID:             "correlated-request",
		CorrelationID:         correlation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func correlationSession(
	t *testing.T, f *fixture, decision auth.Decision, name string,
) interaction.Session {
	t.Helper()
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	return interaction.Session{
		ID:         interaction.DerivedID("session", name),
		RecordedAt: f.clock.Now(),
		Operation:  interaction.OperationRetrieval,
		SnapshotID: shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
}

// TestAuthorizedSinkStampsDecisionCorrelation: the trusted sink records the
// decision's correlation as it records the decision's actor, and a producer
// cannot supply its own — not even an unrecordable one, which would otherwise
// fail validation and with it the audit.
func TestAuthorizedSinkStampsDecisionCorrelation(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	for name, producer := range map[string]shoal.ID{
		"no producer value": "",
		"forged":            "producer-chosen",
		"unrecordable":      "evil\x1b[2Jtrace",
	} {
		t.Run(name, func(t *testing.T) {
			decision := correlationDecision(t, f, "upstream-532")
			session := correlationSession(t, f, decision, "stamp-"+name)
			session.CorrelationID = producer
			ctx := f.context(t, decision)
			recorded, err := f.clientA.RecordInteractionResult(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.CorrelationID != "upstream-532" {
				t.Fatalf("returned correlation = %q", recorded.CorrelationID)
			}
			record, err := f.base.InteractionRecord(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Session.CorrelationID != "upstream-532" ||
				record.Summary.CorrelationID != "upstream-532" {
				t.Fatalf("stored correlation = session %q summary %q",
					record.Session.CorrelationID, record.Summary.CorrelationID)
			}
		})
	}
}

// TestAuthorizedSinkDropsUnrecordableDecisionCorrelation: a decision can be
// minted outside the hosted authenticators, which are the only place #527's
// printable check runs. A correlation the session boundary would refuse is
// dropped, and the audit is recorded without it rather than failing.
func TestAuthorizedSinkDropsUnrecordableDecisionCorrelation(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := correlationDecision(t, f, "line\nbreak‮")
	session := correlationSession(t, f, decision, "unrecordable-decision")
	ctx := f.context(t, decision)
	recorded, err := f.clientA.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatalf("an unrecordable correlation failed the audit: %v", err)
	}
	if recorded.CorrelationID != "" {
		t.Fatalf("unrecordable correlation was recorded: %q",
			recorded.CorrelationID)
	}
}

// TestAuthorizedRetryUnderAnotherCorrelationIsTheSameSession is the identity
// property: correlation is caller-supplyable, so two recordings that differ
// only in it must be one session — same ID, same materialized subgraph — and
// the second must neither conflict (which would let a caller fail an audit by
// choosing a header) nor overwrite the first (which would let it rewrite one).
func TestAuthorizedRetryUnderAnotherCorrelationIsTheSameSession(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	first := correlationDecision(t, f, "trace-one")
	second := correlationDecision(t, f, "trace-two")
	session := correlationSession(t, f, first, "split-attempt")

	firstCtx := f.context(t, first)
	recorded, err := f.clientA.RecordInteractionResult(firstCtx, session)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.base.InteractionSubgraph(firstCtx, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	f.clock.Set(f.clock.Now().Add(time.Second))
	secondCtx := f.context(t, second)
	retried, err := f.clientA.RecordInteractionResult(secondCtx, session)
	if err != nil {
		t.Fatalf("a retry under another correlation conflicted: %v", err)
	}
	if retried.ID != recorded.ID || !reflect.DeepEqual(retried, recorded) {
		t.Fatalf("retry = %+v, want the first session %+v", retried, recorded)
	}
	if retried.CorrelationID != "trace-one" {
		t.Fatalf("retry rewrote the correlation to %q", retried.CorrelationID)
	}
	after, err := f.base.InteractionSubgraph(secondCtx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the retry changed the materialized session")
	}
	records, err := f.base.InteractionRecords(secondCtx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, record := range records {
		if record.Session.AuthorizationFingerprint ==
			session.AuthorizationFingerprint {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d sessions recorded, want 1", count)
	}
}
