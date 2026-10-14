// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// mappingEraApprovalRecord is fleet.ApprovalRecord as #451 slice 2 left it,
// before the identity scheme stamp (#526): what every pending record on a
// deployment that upgrades was written as.
type mappingEraApprovalRecord struct {
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
	ApproverMappingDigest    auth.Digest
	ApproverProvenance       auth.GrantProvenance
	MaterializedAt           time.Time
}

func pendingApprovalRecord() fleet.ApprovalRecord {
	request := testActionRecord()
	return fleet.ApprovalRecord{
		ID: append([]byte(nil), request.ID...), Version: 1,
		State: fleet.ApprovalPending, Request: request,
		RequestDigest:    fleet.ApprovalRequestDigest(request),
		PolicyGeneration: request.PolicyGeneration,
		RequestedAt:      request.CreatedAt,
		ExpiresAt:        request.CreatedAt.Add(30 * time.Minute),
		UpdatedAt:        request.CreatedAt,
	}
}

// TestApprovalRecordWithoutIdentitySchemeDecodesAsLegacy: a record written
// before the stamp decodes with it zero — the legacy scheme, which the
// service treats as "made before schemes existed", never as a scheme that
// differs from every other — and a previous build reads a stamped record by
// skipping the field.
func TestApprovalRecordWithoutIdentitySchemeDecodesAsLegacy(t *testing.T) {
	current := pendingApprovalRecord()
	old := mappingEraApprovalRecord{
		ID: current.ID, Version: current.Version, State: current.State,
		Request: current.Request, RequestDigest: current.RequestDigest,
		PolicyGeneration: current.PolicyGeneration,
		RequestedAt:      current.RequestedAt, ExpiresAt: current.ExpiresAt,
		UpdatedAt: current.UpdatedAt,
	}
	var buffer bytes.Buffer
	buffer.Write(approvalMagic)
	if err := gob.NewEncoder(&buffer).Encode(old); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeApproval(buffer.Bytes())
	if err != nil {
		t.Fatalf("a pre-scheme record no longer decodes: %v", err)
	}
	if decoded.IdentityScheme != (auth.Digest{}) || !reflect.DeepEqual(decoded, current) {
		t.Fatalf("a pre-scheme record decoded as %#v", decoded)
	}

	stamped := fleet.CloneApprovalRecord(current)
	stamped.IdentityScheme = auth.DigestBytes("scheme", []byte("stable"))
	encoded, err := encodeApproval(stamped)
	if err != nil {
		t.Fatalf("a stamped pending record does not encode: %v", err)
	}
	roundTrip, err := decodeApproval(encoded)
	if err != nil || roundTrip.IdentityScheme != stamped.IdentityScheme {
		t.Fatalf("the stamp did not survive the codec: %v %#v", err, roundTrip)
	}
	var rolledBack mappingEraApprovalRecord
	if err := gob.NewDecoder(bytes.NewReader(
		encoded[len(approvalMagic):])).Decode(&rolledBack); err != nil {
		t.Fatalf("the previous build cannot read a stamped record: %v", err)
	}
	if !reflect.DeepEqual(rolledBack, old) {
		t.Fatalf("rolled back record = %#v", rolledBack)
	}
}

// TestApprovalStoreRefusesARewrittenApproval is the approval record's #461:
// the version compare-and-set orders writers, and says nothing about what
// the winner may change. Each probe is a sequential read-N, write-N+1
// through the real durable store, valid by Validate, that changes one field
// the record must never change, and each must fail with the invariant's own
// error (ErrorInternal, naming the field) rather than any other refusal.
func TestApprovalStoreRefusesARewrittenApproval(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T) (*ApprovalStore, fleet.ApprovalRecord, func()) {
		t.Helper()
		runtime := openDispatchRuntime(t, t.TempDir())
		store, err := NewApprovalStore(runtime, nil)
		if err != nil {
			_ = runtime.Close()
			t.Fatal(err)
		}
		record := pendingApprovalRecord()
		record.IdentityScheme = auth.DigestBytes("scheme", []byte("stable"))
		created, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
			Token: []byte("approval-request"), Record: record,
		})
		if err != nil {
			_ = runtime.Close()
			t.Fatal(err)
		}
		return store, created, func() { _ = runtime.Close() }
	}
	next := func(current fleet.ApprovalRecord) fleet.ApprovalRecord {
		record := fleet.CloneApprovalRecord(current)
		record.Version++
		record.UpdatedAt = current.UpdatedAt.Add(time.Second)
		return record
	}
	decide := func(record *fleet.ApprovalRecord) {
		record.State = fleet.ApprovalApproved
		record.Verdict = fleet.ApprovalVerdictApprove
		record.ApproverSubject = "oidcid:https://issuer.example#bob"
		record.ApproverActor = record.ApproverSubject
		record.ApproverFingerprint = auth.Fingerprint{7}
		record.ApproverPolicyGeneration = record.PolicyGeneration
		record.DecidedAt = record.UpdatedAt
		record.DecisionRequestID = "decision"
	}
	rewriteRequest := func(edit func(*fleet.ActionRecord)) func(*fleet.ApprovalRecord) {
		return func(record *fleet.ApprovalRecord) {
			edit(&record.Request)
			record.RequestDigest = fleet.ApprovalRequestDigest(record.Request)
		}
	}

	// The controls: a decision and an expiry, the transitions the service
	// makes, are accepted, so every refusal below is the field's.
	t.Run("control", func(t *testing.T) {
		store, created, done := open(t)
		defer done()
		decided := next(created)
		decide(&decided)
		stored, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
			Token: []byte("approval-decision"), Record: decided,
			ExpectedVersion: created.Version,
		})
		if err != nil {
			t.Fatalf("a decision was refused: %v", err)
		}
		expired := next(stored)
		expired.State = fleet.ApprovalExpired
		if _, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
			Token: []byte("approval-expiry"), Record: expired,
			ExpectedVersion: stored.Version,
		}); err != nil {
			t.Fatalf("an expiry was refused: %v", err)
		}
	})

	for _, probe := range []struct {
		field   string
		decided bool
		rewrite func(*fleet.ApprovalRecord)
	}{
		{"identity scheme", false, func(r *fleet.ApprovalRecord) {
			r.IdentityScheme = auth.Digest{}
		}},
		{"identity scheme", false, func(r *fleet.ApprovalRecord) {
			r.IdentityScheme = auth.DigestBytes("scheme", []byte("other"))
		}},
		{"subject", false, rewriteRequest(func(a *fleet.ActionRecord) {
			a.Subject = "oidcid:https://issuer.example#someone-else"
		})},
		{"actor", false, rewriteRequest(func(a *fleet.ActionRecord) {
			a.Actor = "another-actor"
		})},
		{"client ID", false, rewriteRequest(func(a *fleet.ActionRecord) {
			a.ClientID = "another-client"
		})},
		{"delegation chain", false, rewriteRequest(func(a *fleet.ActionRecord) {
			a.OnBehalfOf = []shoal.ID{"smuggled"}
		})},
		{"input", false, rewriteRequest(func(a *fleet.ActionRecord) {
			a.Input = json.RawMessage(`{"value":99}`)
		})},
		{"expiry", false, func(r *fleet.ApprovalRecord) {
			r.ExpiresAt = r.ExpiresAt.Add(-time.Minute)
		}},
		{"approver subject", true, func(r *fleet.ApprovalRecord) {
			r.ApproverSubject = "oidcid:https://issuer.example#carol"
			r.ApproverActor = r.ApproverSubject
		}},
		{"verdict", true, func(r *fleet.ApprovalRecord) {
			r.State, r.Verdict = fleet.ApprovalRefused, fleet.ApprovalVerdictRefuse
		}},
		{"decision time", true, func(r *fleet.ApprovalRecord) {
			r.DecidedAt = r.DecidedAt.Add(time.Second)
		}},
	} {
		t.Run(probe.field, func(t *testing.T) {
			store, current, done := open(t)
			defer done()
			if probe.decided {
				decided := next(current)
				decide(&decided)
				stored, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
					Token: []byte("approval-decision"), Record: decided,
					ExpectedVersion: current.Version,
				})
				if err != nil {
					t.Fatal(err)
				}
				current = stored
			}
			rewritten := next(current)
			probe.rewrite(&rewritten)
			if err := rewritten.Validate(); err != nil {
				t.Fatalf("the probe cannot express its rewrite validly: %v", err)
			}
			_, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
				Token: []byte("approval-rewrite"), Record: rewritten,
				ExpectedVersion: current.Version,
			})
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) ||
				!strings.Contains(err.Error(), probe.field+" is immutable") {
				t.Fatalf("rewriting the %s = %v, want the immutability invariant",
					probe.field, err)
			}
			stored, readErr := store.GetApproval(ctx, current.ID)
			if readErr != nil || !reflect.DeepEqual(stored, current) {
				t.Fatalf("the refused rewrite changed the record: %v %#v", readErr, stored)
			}
		})
	}
}

// TestApprovalQueryDigestCommitsToTheIdentityClaimPath: the audit session of
// a decision by a stable-identity approver (#526) commits to the claim path
// that named it, and a provenance without one digests as before.
func TestApprovalQueryDigestCommitsToTheIdentityClaimPath(t *testing.T) {
	record := decidedApprovalRecord()
	audit := fleet.ApprovalAudit{
		Phase: "approval_decision", Operation: auth.OperationActionApprove,
		Record: record, Subject: record.ApproverSubject,
		Actor: record.ApproverActor, RequestID: "decision",
		Provenance: mappedProvenance(),
	}
	// Pinned before the field existed.
	const before = "78ef15baa97c0ed1e727fcd53f200a0f1a18a25af032fcaeb04bee07c50ff466"
	if got := approvalQueryDigest(audit); got != before {
		t.Fatalf("a provenance without an identity claim path digests %s, want %s", got, before)
	}
	stable := audit
	stable.Provenance = mappedProvenance()
	stable.Provenance.IdentityClaimPath = []string{"oid"}
	nested := audit
	nested.Provenance = mappedProvenance()
	nested.Provenance.IdentityClaimPath = []string{"ext", "oid"}
	if approvalQueryDigest(stable) == approvalQueryDigest(audit) ||
		approvalQueryDigest(stable) == approvalQueryDigest(nested) {
		t.Fatal("the session does not commit to the identity claim path")
	}
}
