// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestApprovalRefusesAMovedApproverMappingBeforeCommitting is the mapping
// counterpart of the superseded-generation test, scoped to advance's own
// check in the same way: an approval given under one approver mapping is
// re-requested after the mapping in force has changed. advance must refuse
// before the approved → enqueued commit, leave the row approved, and create
// no work; with the mapping unchanged, the same call materializes.
func TestApprovalRefusesAMovedApproverMappingBeforeCommitting(t *testing.T) {
	given := auth.DigestBytes("mapping", []byte("v1"))
	approval := decided(validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	approval.ApproverProvenance = auth.GrantProvenance{
		Issuer: "https://issuer.example", Subject: "bob",
		ClaimPath: []string{"groups"}, MatchedValue: "approvers",
		MappingDigest: given,
	}
	approval.ApproverMappingDigest = given
	if err := approval.Validate(); err != nil {
		t.Fatal(err)
	}
	now := approval.DecidedAt.Add(2 * time.Second)
	store := &memoryApprovalStore{records: map[string]ApprovalRecord{
		string(approval.ID): CloneApprovalRecord(approval),
	}}
	dispatchStore := newMemoryDispatchStore()
	service := func(inForce auth.Digest) *ApprovalService {
		return &ApprovalService{
			dispatch: &DispatchService{
				store: dispatchStore, outbox: dispatchStore,
				recorder: &dispatchRecorder{}, events: dispatchEvents{},
				clock: func() time.Time { return now },
			},
			store: store, recorder: unguardedApprovalRecorder{},
			narrowed:    func(context.Context) bool { return false },
			generations: fixedGenerations{generation: approval.PolicyGeneration},
			mapping: func(context.Context) (auth.Digest, error) {
				return inForce, nil
			},
			window: DefaultApprovalWindow,
		}
	}
	decision := approvalBranchDecision(t, approval.RequestedAt, auth.OperationDispatch)

	for name, inForce := range map[string]auth.Digest{
		"a changed mapping": auth.DigestBytes("mapping", []byte("v2")),
		"no mapping now":    {},
	} {
		_, err := service(inForce).advance(
			context.Background(), decision, approval, now)
		if !errors.Is(err, ErrApproverMappingMoved) ||
			!shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("advance under %s = %v", name, err)
		}
		stored, _ := store.GetApproval(context.Background(), approval.ID)
		if stored.State != ApprovalApproved {
			t.Fatalf("%s: the approval moved to %q", name, stored.State)
		}
		if _, err := dispatchStore.GetAction(
			context.Background(), approval.ID); !errors.Is(err, ErrActionNotFound) {
			t.Fatalf("%s: work was created: %v", name, err)
		}
	}

	receipt, err := service(given).advance(
		context.Background(), decision, approval, now)
	if err != nil || receipt.State != ApprovalEnqueued {
		t.Fatalf("advance under the same mapping = %+v, %v", receipt, err)
	}
}

// TestApprovalRecordMappingFieldsAreConsistent pins Validate's rules for the
// additive fields.
func TestApprovalRecordMappingFieldsAreConsistent(t *testing.T) {
	provenance := auth.GrantProvenance{
		Issuer: "https://issuer.example", Subject: "bob",
		ClaimPath: []string{"groups"}, MatchedValue: "approvers",
		MappingDigest: auth.DigestBytes("mapping", []byte("v1")),
	}
	base := decided(validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	if err := base.Validate(); err != nil {
		t.Fatalf("a decided record without mapping fields: %v", err)
	}
	for name, edit := range map[string]func(*ApprovalRecord){
		"digest without provenance": func(r *ApprovalRecord) {
			r.ApproverMappingDigest = provenance.MappingDigest
		},
		"provenance without digest": func(r *ApprovalRecord) {
			r.ApproverProvenance = provenance
		},
		"digest not the provenance's": func(r *ApprovalRecord) {
			r.ApproverProvenance = provenance
			r.ApproverMappingDigest = auth.DigestBytes("mapping", []byte("v2"))
		},
		"incomplete provenance": func(r *ApprovalRecord) {
			r.ApproverProvenance = provenance
			r.ApproverProvenance.MatchedValue = ""
			r.ApproverMappingDigest = provenance.MappingDigest
		},
	} {
		record := CloneApprovalRecord(base)
		edit(&record)
		if err := record.Validate(); err == nil {
			t.Errorf("%s validated", name)
		}
	}
	consistent := CloneApprovalRecord(base)
	consistent.ApproverProvenance = provenance
	consistent.ApproverMappingDigest = provenance.MappingDigest
	if err := consistent.Validate(); err != nil {
		t.Fatalf("a consistent record: %v", err)
	}
	clone := CloneApprovalRecord(consistent)
	clone.ApproverProvenance.ClaimPath[0] = "mutated"
	if consistent.ApproverProvenance.ClaimPath[0] != "groups" {
		t.Fatal("CloneApprovalRecord aliases the provenance")
	}
}
