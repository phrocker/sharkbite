// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// completedByAnotherPrincipal leaves the fixture's action terminal, carrying
// one labelled and one unlabelled evidence reference, as a foreign worker's
// completion would.
//
// The shape is the whole point. #369 filtered the paths where a reader asks
// *about* an action — Status, and TeamActions and Pull through
// scanDispatchActions. It left the paths where a caller re-sends its own
// request and is handed the record back, and those differ in exactly the way
// that matters: the caller is the **enqueuer**, while the evidence was
// recorded at completion by whoever executed, which since #437 may be an
// execute-authorized worker retrieving under its own labels.
//
// So the discriminator is who executed, not who is asking. A replay to the
// claimant is safe — it receives its own retrieval, which its own labels
// governed, which is why ExecuteClaim's and completeClaim's terminal replays
// need no filter. A replay to the enqueuer is not.
func completedByAnotherPrincipal(
	t *testing.T, fixture *executorClaimFixture,
) (labelled, open EvidenceRef) {
	t.Helper()
	labelled = EvidenceRef{
		AnchorID: "anchor-secret", Kind: interaction.EvidenceDocument,
		NodeIDs: []shoal.ID{"node-secret"}, Visibility: []string{"secret"},
	}
	open = EvidenceRef{
		AnchorID: "anchor-open", Kind: interaction.EvidenceDocument,
		NodeIDs: []shoal.ID{"node-open"},
	}
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	if stored.State != DispatchQueued {
		t.Fatalf("the fixture's action is %q, so this helper cannot shape it "+
			"as a foreign completion", stored.State)
	}
	stored.State = DispatchSucceeded
	stored.Version++
	stored.ErrorCode = ""
	stored.ClaimID = []byte("worker-claim")
	stored.ClaimFence = 1
	stored.ClaimLease = time.Minute
	stored.ClaimLeaseUntil = stored.UpdatedAt.Add(time.Minute).UTC()
	// A claimant that is not the enqueuer, which is what makes the evidence
	// something the replaying principal may not hold the labels for.
	stored.ClaimantSubject = "worker-subject"
	stored.ClaimantActor = "worker-actor"
	stored.ExecutionPolicyGeneration = 1
	stored.ExecutionExpiresAt = stored.Deadline
	stored.Evidence = []EvidenceRef{labelled, open}
	if stored.ClaimantSubject == stored.Subject {
		t.Fatal("the claimant is the enqueuer, so this fixture cannot " +
			"distinguish a foreign completion from the caller's own work")
	}
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored
	return labelled, open
}

// TestAReplayToTheEnqueuerRedactsForeignEvidence is the #369 leftover found by
// the #562 audit.
//
// Each reference is decided by its nodes' current rules (#564): the
// enqueuer holds the source policy governing node-open and not the label
// policy also governing node-secret.
func TestAReplayToTheEnqueuerRedactsForeignEvidence(t *testing.T) {
	t.Run("enqueue replay", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		fixture.service.evidenceNodes = fixtureCatalog(
			t, fixture.authority.Resolver(), fixture.now)
		completedByAnotherPrincipal(t, fixture)
		// The identical enqueue, which equivalentEnqueue answers as a replay.
		replayed, err := fixture.service.Enqueue(
			fixture.enqueuer, dispatchEnqueue(fixture.now, "request"))
		if err != nil {
			t.Fatalf("the identical enqueue was not answered as a replay: %v",
				err)
		}
		assertRedactedToOpen(t, "Enqueue replay", replayed.Evidence)
	})

	t.Run("invoke terminal replay", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		fixture.service.evidenceNodes = fixtureCatalog(
			t, fixture.authority.Resolver(), fixture.now)
		completedByAnotherPrincipal(t, fixture)
		replayed, err := fixture.service.Invoke(
			fixture.enqueuer, InvokeRequest{
				Enqueue: dispatchEnqueue(fixture.now, "request"),
				ClaimID: []byte("invoke-claim"), Lease: time.Minute,
			})
		if err != nil {
			t.Fatalf("the terminal invoke replay was refused: %v", err)
		}
		assertRedactedToOpen(t, "Invoke terminal replay", replayed.Evidence)
	})
}

// assertRedactedToOpen requires exactly the unlabelled reference, with no
// count of what was withheld — #369's shape, for #398's reason.
func assertRedactedToOpen(t *testing.T, path string, got []EvidenceRef) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("%s returned %d evidence references, want 1: a caller that "+
			"did not execute the action received evidence recorded under "+
			"another principal's labels — %#v", path, len(got), got)
	}
	if got[0].AnchorID != "anchor-open" {
		t.Fatalf("%s returned %q, want the unlabelled reference",
			path, got[0].AnchorID)
	}
}

// TestAnApprovalReRequestRedactsForeignEvidence is the third leftover: an
// approval re-request whose work has already been executed is answered with
// the materialized record, and the requester is not the principal that
// executed it.
func TestAnApprovalReRequestRedactsForeignEvidence(t *testing.T) {
	approval := decided(
		validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	now := approval.DecidedAt.Add(2 * time.Second)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	dispatchStore := newMemoryDispatchStore()
	service := &ApprovalService{
		dispatch: &DispatchService{
			store: dispatchStore, outbox: dispatchStore,
			recorder: &dispatchRecorder{}, events: dispatchEvents{},
			clock:         func() time.Time { return now },
			evidenceNodes: fixtureCatalog(t, authority.Resolver(), now),
		},
		store: &memoryApprovalStore{records: map[string]ApprovalRecord{
			string(approval.ID): CloneApprovalRecord(approval),
		}},
		recorder:    unguardedApprovalRecorder{},
		narrowed:    func(context.Context) bool { return false },
		generations: fixedGenerations{generation: approval.PolicyGeneration},
		window:      DefaultApprovalWindow,
	}

	// The materialized action, completed by whoever executed it, carrying one
	// labelled and one unlabelled reference. Built from approval.Request so
	// isMaterializationOf accepts it — a record it rejects is answered
	// ErrActionConflict and never reaches the return under test.
	action := cloneActionRecord(approval.Request)
	action.ApprovalRequestDigest = approval.RequestDigest
	action.ApproverSubject = approval.ApproverSubject
	action.ApproverActor = approval.ApproverActor
	action.ApproverClientID = approval.ApproverClientID
	action.ApprovedAt = approval.DecidedAt
	action.State = DispatchSucceeded
	action.ClaimID = []byte("worker-claim")
	action.ClaimFence = 1
	action.ClaimLease = time.Minute
	action.ClaimLeaseUntil = now.Add(time.Minute).UTC()
	action.ClaimantSubject = "worker-subject"
	action.ClaimantActor = "worker-actor"
	action.ExecutionPolicyGeneration = 1
	action.ExecutionExpiresAt = action.Deadline
	action.Evidence = []EvidenceRef{
		{
			AnchorID: "anchor-secret", Kind: interaction.EvidenceDocument,
			NodeIDs: []shoal.ID{"node-secret"}, Visibility: []string{"secret"},
		},
		{
			AnchorID: "anchor-open", Kind: interaction.EvidenceDocument,
			NodeIDs: []shoal.ID{"node-open"},
		},
	}
	if action.ClaimantSubject == action.Subject {
		t.Fatal("the claimant is the requester, so this fixture cannot " +
			"distinguish a foreign completion from the requester's own work")
	}

	requester := bindDecision(t, authority, ownerUntil(
		t, now.Add(time.Hour), false, auth.OperationInvoke))
	replayed, err := service.replayMaterialized(requester, action, approval)
	if err != nil {
		t.Fatalf("the materialized replay was refused, so this test did not "+
			"reach the return under test: %v", err)
	}
	assertRedactedToOpen(t, "approval re-request", replayed.Evidence)
}
