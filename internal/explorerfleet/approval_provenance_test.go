// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// legacyApprovalRecord is fleet.ApprovalRecord as it was before the approver
// mapping fields (#451 slice 2), field for field. gob matches fields by name,
// so a record a previous build wrote is exactly this encoding.
type legacyApprovalRecord struct {
	ID                       []byte
	Version                  uint64
	State                    fleet.ApprovalState
	Request                  fleet.ActionRecord
	RequestDigest            []byte
	PolicyGeneration         int64
	RequestedAt              time.Time
	ExpiresAt                time.Time
	UpdatedAt                time.Time
	Verdict                  fleet.ApprovalVerdict
	ApproverSubject          shoal.ID
	ApproverActor            shoal.ID
	ApproverClientID         shoal.ID
	ApproverFingerprint      auth.Fingerprint
	ApproverPolicyGeneration int64
	DecidedAt                time.Time
	DecisionRequestID        shoal.ID
	DecisionCorrelationID    shoal.ID
	MaterializedAt           time.Time
}

func decidedApprovalRecord() fleet.ApprovalRecord {
	request := testActionRecord()
	return fleet.ApprovalRecord{
		ID: append([]byte(nil), request.ID...), Version: 2,
		State: fleet.ApprovalApproved, Request: request,
		RequestDigest:            fleet.ApprovalRequestDigest(request),
		PolicyGeneration:         request.PolicyGeneration,
		RequestedAt:              request.CreatedAt,
		ExpiresAt:                request.CreatedAt.Add(30 * time.Minute),
		UpdatedAt:                request.CreatedAt.Add(time.Minute),
		Verdict:                  fleet.ApprovalVerdictApprove,
		ApproverSubject:          "oidc:https://issuer.example#bob",
		ApproverActor:            "oidc:https://issuer.example#bob",
		ApproverClientID:         "oidc:https://issuer.example#console",
		ApproverFingerprint:      auth.Fingerprint{7},
		ApproverPolicyGeneration: request.PolicyGeneration,
		DecidedAt:                request.CreatedAt.Add(time.Minute),
		DecisionRequestID:        "decision",
	}
}

func mappedProvenance() auth.GrantProvenance {
	return auth.GrantProvenance{
		Issuer: "https://issuer.example", Subject: "bob",
		ClaimPath:     []string{"realm_access", "roles"},
		MatchedValue:  "shoal-approvers",
		MappingDigest: auth.DigestBytes("mapping", []byte("v1")),
	}
}

// TestApprovalRecordWithoutMappingFieldsStillDecodes is the rollout half of
// the additive field: a record written by the previous build decodes into
// this one with the new fields zero, and validates.
func TestApprovalRecordWithoutMappingFieldsStillDecodes(t *testing.T) {
	current := decidedApprovalRecord()
	legacy := legacyApprovalRecord{
		ID: current.ID, Version: current.Version, State: current.State,
		Request: current.Request, RequestDigest: current.RequestDigest,
		PolicyGeneration: current.PolicyGeneration,
		RequestedAt:      current.RequestedAt, ExpiresAt: current.ExpiresAt,
		UpdatedAt: current.UpdatedAt, Verdict: current.Verdict,
		ApproverSubject:          current.ApproverSubject,
		ApproverActor:            current.ApproverActor,
		ApproverClientID:         current.ApproverClientID,
		ApproverFingerprint:      current.ApproverFingerprint,
		ApproverPolicyGeneration: current.ApproverPolicyGeneration,
		DecidedAt:                current.DecidedAt,
		DecisionRequestID:        current.DecisionRequestID,
	}
	var buffer bytes.Buffer
	buffer.Write(approvalMagic)
	if err := gob.NewEncoder(&buffer).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeApproval(buffer.Bytes())
	if err != nil {
		t.Fatalf("a previous build's record no longer decodes: %v", err)
	}
	if !reflect.DeepEqual(decoded, current) {
		t.Fatalf("legacy record decoded as %#v", decoded)
	}
	if decoded.ApproverMappingDigest != (auth.Digest{}) ||
		decoded.ApproverProvenance.Set() {
		t.Fatal("a legacy record decoded with mapping provenance")
	}
	// And the previous build reads what this one writes: gob skips fields
	// the destination does not have, so a rollback decodes a record with
	// mapping provenance, without it.
	withMapping := fleet.CloneApprovalRecord(current)
	withMapping.ApproverProvenance = mappedProvenance()
	withMapping.ApproverMappingDigest = withMapping.ApproverProvenance.MappingDigest
	encoded, err := encodeApproval(withMapping)
	if err != nil {
		t.Fatal(err)
	}
	var rolledBack legacyApprovalRecord
	if err := gob.NewDecoder(bytes.NewReader(
		encoded[len(approvalMagic):])).Decode(&rolledBack); err != nil {
		t.Fatalf("the previous build cannot read this build's record: %v", err)
	}
	if !reflect.DeepEqual(rolledBack, legacy) {
		t.Fatalf("rolled back record = %#v", rolledBack)
	}
}

// TestApprovalRecordMappingProvenanceRoundTrips pins the new fields through
// the codec and Validate's consistency rules.
func TestApprovalRecordMappingProvenanceRoundTrips(t *testing.T) {
	record := decidedApprovalRecord()
	record.ApproverProvenance = mappedProvenance()
	record.ApproverMappingDigest = record.ApproverProvenance.MappingDigest
	encoded, err := encodeApproval(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeApproval(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("mapping provenance did not survive: %#v", decoded)
	}

	digestOnly := decidedApprovalRecord()
	digestOnly.ApproverMappingDigest = mappedProvenance().MappingDigest
	if _, err := encodeApproval(digestOnly); err == nil {
		t.Fatal("encoded a mapping digest with no provenance behind it")
	}
	mismatched := fleet.CloneApprovalRecord(record)
	mismatched.ApproverMappingDigest = auth.DigestBytes("mapping", []byte("v2"))
	if _, err := encodeApproval(mismatched); err == nil {
		t.Fatal("encoded a mapping digest that is not its provenance's")
	}
	pending := decidedApprovalRecord()
	pending.State, pending.Verdict = fleet.ApprovalPending, ""
	pending.ApproverSubject, pending.ApproverActor = "", ""
	pending.ApproverClientID, pending.ApproverFingerprint = "", auth.Fingerprint{}
	pending.ApproverPolicyGeneration, pending.DecidedAt = 0, time.Time{}
	pending.DecisionRequestID = ""
	if _, err := encodeApproval(pending); err != nil {
		t.Fatalf("an undecided record no longer encodes: %v", err)
	}
	pending.ApproverProvenance = mappedProvenance()
	pending.ApproverMappingDigest = pending.ApproverProvenance.MappingDigest
	if _, err := encodeApproval(pending); err == nil {
		t.Fatal("encoded an undecided record carrying approver provenance")
	}
}

// TestApprovalRecorderPinsProvenance pins what the recorder does with an
// acting principal's provenance: the session commits to it, a decision audit
// must carry exactly the provenance its record stores, and a transition by a
// principal with none digests exactly as before.
func TestApprovalRecorderPinsProvenance(t *testing.T) {
	record := decidedApprovalRecord()
	record.ApproverProvenance = mappedProvenance()
	record.ApproverMappingDigest = record.ApproverProvenance.MappingDigest
	audit := fleet.ApprovalAudit{
		Phase: "approval_decision", Operation: auth.OperationActionApprove,
		Record: record, Subject: record.ApproverSubject,
		Actor: record.ApproverActor, RequestID: "decision",
		Provenance: mappedProvenance(),
	}
	withProvenance := approvalQueryDigest(audit)
	plain := audit
	plain.Provenance = auth.GrantProvenance{}
	if approvalQueryDigest(plain) == withProvenance {
		t.Fatal("the session does not commit to the provenance")
	}
	if approvalQueryDigest(plain) != approvalQueryDigestBefore(plain) {
		t.Fatal("a transition without provenance digests differently than before")
	}
	other := audit
	other.Provenance = mappedProvenance()
	other.Provenance.MatchedValue = "other-approvers"
	if approvalQueryDigest(other) == withProvenance {
		t.Fatal("the session digest ignores the matched value")
	}

	// Validation refuses before any dependency is reached, so none is wired.
	recorder := &ApprovalRecorder{}
	mismatch := audit
	mismatch.Provenance = other.Provenance
	if err := recorder.RecordApproval(t.Context(), mismatch); err == nil ||
		!shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("a decision audit with another provenance = %v", err)
	}
	invalid := audit
	invalid.Provenance.ClaimPath = nil
	if err := recorder.RecordApproval(t.Context(), invalid); err == nil ||
		!shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("a decision audit with incomplete provenance = %v", err)
	}
}

// approvalQueryDigestBefore is the query digest as the recorder computed it
// before provenance existed.
func approvalQueryDigestBefore(audit fleet.ApprovalAudit) string {
	return interaction.Digest(string(audit.Operation) + ":" + audit.Phase + ":" +
		string(audit.Record.State))
}
