// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestDescriptorCodecWritesAttestationOnlyWhenRequired pins the rollout shape
// of RequiresAttestation (#446): a descriptor without it keeps the version and
// bytes it had (3, or 4 with approval), and one with it is written as version
// 5, which an earlier build refuses rather than reading the action without
// the requirement.
func TestDescriptorCodecWritesAttestationOnlyWhenRequired(t *testing.T) {
	plain := effectDescriptor(fleet.Effects{fleet.EffectMutatesExternal})
	before, _ := encodeDescriptor(plain, [sha256.Size]byte{})
	if version := binary.BigEndian.Uint16(before); version != codecVersion {
		t.Fatalf("plain descriptor wrote version %d", version)
	}
	approved := effectDescriptor(fleet.Effects{fleet.EffectMutatesExternal})
	approved.Capabilities[0].Actions[0].RequiresApproval = true
	approvedBytes, _ := encodeDescriptor(approved, [sha256.Size]byte{})
	if version := binary.BigEndian.Uint16(approvedBytes); version != codecVersionApproval {
		t.Fatalf("approval descriptor wrote version %d", version)
	}

	attested := effectDescriptor(fleet.Effects{fleet.EffectMutatesExternal})
	attested.Capabilities[0].Actions[0].RequiresAttestation = true
	attested.Capabilities[0].Actions = append(attested.Capabilities[0].Actions, fleet.Action{
		Name: "status", InputSchema: plain.Capabilities[0].Actions[0].InputSchema,
		OutputSchema: plain.Capabilities[0].Actions[0].OutputSchema, RequiresApproval: true,
	})
	encoded, err := encodeDescriptor(attested, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if version := binary.BigEndian.Uint16(encoded); version != codecVersionAttestation {
		t.Fatalf("attested descriptor wrote version %d", version)
	}
	decoded, _, err := decodeDescriptor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	actions := decoded.Capabilities[0].Actions
	if !actions[0].RequiresAttestation || actions[0].RequiresApproval ||
		actions[1].RequiresAttestation || !actions[1].RequiresApproval {
		t.Fatalf("flags did not round-trip per action: %+v", actions)
	}
	// The attestation byte follows the approval byte; anything but 0 or 1 is
	// corruption.
	flagAt := bytes.Index(encoded, []byte("external")) + len("external") + 1
	if encoded[flagAt] != 1 {
		t.Fatalf("attestation flag not where expected: %d", encoded[flagAt])
	}
	corrupt := append([]byte(nil), encoded...)
	corrupt[flagAt] = 2
	if _, _, err := decodeDescriptor(corrupt); err == nil {
		t.Fatal("a corrupt attestation flag decoded")
	}
}

func claimedActionRecord() fleet.ActionRecord {
	record := testActionRecord()
	now := record.CreatedAt
	record.Version, record.State = 2, fleet.DispatchClaimed
	record.ClaimID, record.ClaimFence = []byte("claim"), 1
	record.ClaimLease, record.ClaimLeaseUntil = time.Minute, now.Add(time.Minute)
	record.ClaimantSubject, record.ClaimantActor, record.ClaimantClientID = "worker", "worker-actor", "worker-client"
	record.ExecutionPolicyGeneration, record.ExecutionExpiresAt = 1, now.Add(time.Hour)
	record.ExecutionFingerprint = auth.Fingerprint{3}
	record.TransitionOperation = auth.OperationExecute
	record.ClaimAttestationID = "exattest:0123"
	record.ClaimHistory = []fleet.ClaimHolder{{
		Subject: "earlier", Actor: "earlier-actor", ClaimID: []byte("old"),
		ClaimFence: 1, HeldAt: now, AttestationID: "exattest:old",
	}}
	record.ClaimFence = 2
	return record
}

// TestClaimAttestationSurvivesTheDispatchCodecAndOldBuildsReadIt round-trips
// the field, decodes a record written before it existed, and shows that an
// earlier build's ActionRecord — which lacks the field — still decodes a record
// carrying it, because gob ignores fields the target type does not have.
func TestClaimAttestationSurvivesTheDispatchCodecAndOldBuildsReadIt(t *testing.T) {
	record := claimedActionRecord()
	encoded, err := encodeAction(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAction(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("claim attestation did not survive: %#v", decoded)
	}

	legacy := claimedActionRecord()
	legacy.ClaimAttestationID = ""
	legacy.ClaimHistory[0].AttestationID = ""
	old, err := encodeAction(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := decodeAction(old); err != nil || decoded.ClaimAttestationID != "" {
		t.Fatalf("a record without the field = %+v, %v", decoded.ClaimAttestationID, err)
	}

	// The previous build's view of the record: the same type without the two
	// new fields. gob matches by field name and skips what it lacks.
	type previousHolder struct {
		Subject    string
		ClaimFence uint64
	}
	type previousRecord struct {
		ID           []byte
		Version      uint64
		State        fleet.DispatchState
		ClaimFence   uint64
		ClaimHistory []previousHolder
	}
	var previous previousRecord
	if err := gob.NewDecoder(bytes.NewReader(encoded[len(dispatchMagic):])).Decode(&previous); err != nil {
		t.Fatalf("an earlier build could not read a record carrying the field: %v", err)
	}
	if previous.ClaimFence != 2 || len(previous.ClaimHistory) != 1 {
		t.Fatalf("earlier build read %+v", previous)
	}

	// The bound is refused at encode, not only on read.
	tooLong := claimedActionRecord()
	tooLong.ClaimAttestationID = shoal.ID("exattest:" + string(bytes.Repeat([]byte("a"), fleet.MaxClaimAttestationIDBytes)))
	if _, err := encodeAction(tooLong); err == nil {
		t.Fatal("an oversized claim attestation ID encoded")
	}
}
