// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// TestDescriptorCodecWritesApprovalOnlyWhenRequired pins the rollout shape of
// the approval flag (#451). A descriptor that requires no approval is written
// as version 3, byte for byte what the previous build wrote, so a rollback
// reads it. One that does is written as version 4, which the previous build
// refuses — so it cannot read the action and silently drop the flag.
func TestDescriptorCodecWritesApprovalOnlyWhenRequired(t *testing.T) {
	plain := effectDescriptor(fleet.Effects{fleet.EffectMutatesExternal})
	encoded, err := encodeDescriptor(plain, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if version := binary.BigEndian.Uint16(encoded); version != codecVersion {
		t.Fatalf("a descriptor without approval wrote version %d", version)
	}
	decoded, _, err := decodeDescriptor(encoded)
	if err != nil || decoded.Capabilities[0].Actions[0].RequiresApproval {
		t.Fatalf("plain descriptor = %+v, %v", decoded, err)
	}

	held := effectDescriptor(fleet.Effects{fleet.EffectMutatesExternal})
	held.Capabilities[0].Actions[0].RequiresApproval = true
	held.Capabilities[0].Actions = append(held.Capabilities[0].Actions, fleet.Action{
		Name: "status", InputSchema: plain.Capabilities[0].Actions[0].InputSchema,
		OutputSchema: plain.Capabilities[0].Actions[0].OutputSchema,
	})
	encoded, err = encodeDescriptor(held, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if version := binary.BigEndian.Uint16(encoded); version != codecVersionApproval {
		t.Fatalf("a descriptor requiring approval wrote version %d", version)
	}
	decoded, _, err = decodeDescriptor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Capabilities[0].Actions[0].RequiresApproval ||
		decoded.Capabilities[0].Actions[1].RequiresApproval {
		t.Fatalf("approval flags did not round-trip per action: %+v",
			decoded.Capabilities[0].Actions)
	}

	// A flag byte other than zero or one is corruption, refused rather than
	// read either way.
	flagAt := bytes.Index(encoded, []byte("external")) + len("external")
	corrupt := append([]byte(nil), encoded...)
	if corrupt[flagAt] != 1 {
		t.Fatalf("flag byte not where expected: %d", corrupt[flagAt])
	}
	corrupt[flagAt] = 2
	if _, _, err := decodeDescriptor(corrupt); err == nil {
		t.Fatal("a corrupt approval flag decoded")
	}
}

func approvedActionRecord(t *testing.T) fleet.ActionRecord {
	t.Helper()
	record := testActionRecord()
	record.ApprovalRequestDigest = fleet.ApprovalRequestDigest(record)
	record.ApprovalPolicyGeneration = record.PolicyGeneration
	record.ApproverSubject, record.ApproverActor = "bob", "bob-console"
	record.ApproverClientID = "console"
	record.ApprovedAt = record.CreatedAt
	return record
}

// TestApprovalProvenanceSurvivesTheDispatchCodec pins that the approval a
// record was materialized under is still on it after a round trip. ApplyAction
// compares the stored record by reflect.DeepEqual, so a dropped field would make
// every materialization retry look like a conflicting record.
func TestApprovalProvenanceSurvivesTheDispatchCodec(t *testing.T) {
	record := approvedActionRecord(t)
	encoded, err := encodeAction(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAction(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("approval provenance did not survive: %#v", decoded)
	}
}

// TestApprovalRecordSurvivesItsCodec round-trips a decided approval and
// refuses one whose digest no longer describes its request.
func TestApprovalRecordSurvivesItsCodec(t *testing.T) {
	request := testActionRecord()
	record := fleet.ApprovalRecord{
		ID: append([]byte(nil), request.ID...), Version: 2,
		State: fleet.ApprovalApproved, Request: request,
		RequestDigest:    fleet.ApprovalRequestDigest(request),
		PolicyGeneration: request.PolicyGeneration,
		RequestedAt:      request.CreatedAt,
		ExpiresAt:        request.CreatedAt.Add(30 * time.Minute),
		UpdatedAt:        request.CreatedAt.Add(time.Minute),
		Verdict:          fleet.ApprovalVerdictApprove,
		ApproverSubject:  "bob", ApproverActor: "bob-console",
		ApproverFingerprint:      auth.Fingerprint{7},
		ApproverPolicyGeneration: request.PolicyGeneration,
		DecidedAt:                request.CreatedAt.Add(time.Minute),
		DecisionRequestID:        "decision",
	}
	encoded, err := encodeApproval(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeApproval(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("approval record did not survive: %#v", decoded)
	}
	tampered := fleet.CloneApprovalRecord(record)
	tampered.Request.Input = []byte(`{"value":2}`)
	if _, err := encodeApproval(tampered); err == nil {
		t.Fatal("encoded an approval whose digest does not describe its request")
	}
	// The approval key space is disjoint from the action key space, so no
	// action scan or read can ever return a held request.
	if bytes.HasPrefix(approvalRow(record.ID), []byte("action/")) ||
		approvalKind == dispatchKind || approvalKind == transitionKind {
		t.Fatal("approval rows share the action key space")
	}
}
