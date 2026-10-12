// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestTheTolerantScanSkipsAnUndecodableApproval (#655): a stored approval
// that does not decode — corrupt, or written by a newer build — fails the
// strict page Pending reads, and is skipped and counted by the tolerant one
// the sweep reads, which still returns every other record on the page.
func TestTheTolerantScanSkipsAnUndecodableApproval(t *testing.T) {
	ctx := context.Background()
	runtime := openDispatchRuntime(t, t.TempDir())
	defer func() { _ = runtime.Close() }()
	store, err := NewApprovalStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
		Token: []byte("approval-request"), Record: pendingApprovalRecord(),
	}); err != nil {
		t.Fatal(err)
	}
	// A cell at a later approval row whose value is the approval encoding's
	// magic followed by bytes no build decodes.
	id := []byte("zz-undecodable")
	value := append(append([]byte(nil), approvalMagic...), 0xff, 0x00, 0x13)
	lpart, err := explorercoord.Partition(coordination.DomainID("fleet-approval"), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: []byte("fleet.approval.apply.v1"), Token: []byte("garbage"),
		Cells: []explorercoord.Cell{{
			Table: DispatchTable, Row: approvalRow(id),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Value: value, EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []explorercoord.GuardIntent{{
			Entity: approvalEntity(id), Mode: guard.ModeAbsentOrIdentical,
			DesiredState: guard.StateLive, DesiredWinnerID: transitionWinner(value),
			LPART: lpart, LogicalPolicyID: approvalPolicy, RetirementGeneration: 1,
		}},
		Results: []explorercoord.ResultIdentity{{Kind: []byte("fleet-approval"), ID: id}},
	}}); err != nil {
		t.Fatalf("write the undecodable cell: %v", err)
	}
	if _, err := store.ScanApprovals(ctx, nil, fleet.MaxApprovalListResults); !shoal.IsErrorCode(
		err, shoal.ErrorInternal) {
		t.Fatalf("strict scan over an undecodable record = %v, want it to fail closed", err)
	}
	page, err := store.ScanApprovalsTolerant(ctx, nil, fleet.MaxApprovalListResults)
	if err != nil || len(page.Approvals) != 1 || page.Undecodable != 1 {
		t.Fatalf("tolerant scan = %d records, %d undecodable, %v",
			len(page.Approvals), page.Undecodable, err)
	}
}
