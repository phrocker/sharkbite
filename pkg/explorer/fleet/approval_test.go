// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// These are the model-level tests for #451. The behavioural acceptance tests
// run against the hosted composition in cmd/shoal-explore-web, where every
// recorder and publisher is the real one; nothing here substitutes for them.

func approvalDigestMutation(requireApproval bool) Mutation {
	return Mutation{
		RegistrationKey: "key", ExpectedGeneration: 0,
		Descriptor: Descriptor{
			ID: "agent", Generation: 1, Subject: "owner", Actor: "operator",
			AuthorizationDomain: []byte("domain"),
			Scopes:              []Scope{{SourceID: []byte("s"), PolicyID: []byte("p")}},
			ExecutorRef:         "local",
			Capabilities: []Capability{{Name: "ops", Actions: []Action{{
				Name:             "deploy",
				InputSchema:      json.RawMessage(`{"type":"object"}`),
				OutputSchema:     json.RawMessage(`{"type":"object"}`),
				Effects:          Effects{EffectMutatesExternal},
				RequiresApproval: requireApproval,
			}}}},
			LeaseExpiresAt: time.Unix(1_800_000_000, 0).UTC(),
		},
	}
}

// TestRegistryDigestIsUnchangedWithoutApproval pins the v1 mutation digest of
// a descriptor that does not require approval to the value the encoding
// produced before the field existed. A v1 digest is embedded in the QueryDigest
// of every receipt written before v2 (#521), so the legacy reader must still
// reproduce it, or a heartbeat or revoke retry that spans the upgrade reads as
// a divergent mutation.
//
// The golden value was computed by the previous build's registryMutationDigest
// (extracted verbatim from origin/main and run over this mutation), not by the
// function under test, so it is an independent statement of the old encoding.
func TestRegistryDigestIsUnchangedWithoutApproval(t *testing.T) {
	const golden = "54af49ed8009c6c7c8d4862c19d5875176dfa8dbe51dd5188521a0c8b672138a"
	plain := registryMutationDigestV1(approvalDigestMutation(false))
	if got := hex.EncodeToString(plain[:]); got != golden {
		t.Fatalf("v1 mutation digest without approval = %s, want %s", got, golden)
	}
	for _, version := range registryDigestVersions {
		if version.digest(approvalDigestMutation(true)) ==
			version.digest(approvalDigestMutation(false)) {
			t.Fatalf("%s: requiring approval did not change the mutation "+
				"digest, so a replay could add or drop it under the same "+
				"identity", version.name)
		}
	}
	// The JSON form, which descriptorDigest hashes, carries no key at all
	// when the flag is false.
	encoded, err := json.Marshal(approvalDigestMutation(false).Descriptor.Capabilities[0].Actions[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("requires_approval")) {
		t.Fatalf("an action without approval marshals the key: %s", encoded)
	}
	var decoded Action
	encoded, _ = json.Marshal(approvalDigestMutation(true).Descriptor.Capabilities[0].Actions[0])
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.RequiresApproval {
		t.Fatalf("approval did not round-trip through JSON: %s, %v", encoded, err)
	}
}

// TestDelegationCannotDropApproval pins both directions of the subset rule.
func TestDelegationCannotDropApproval(t *testing.T) {
	held := approvalDigestMutation(true).Descriptor.Capabilities
	plain := approvalDigestMutation(false).Descriptor.Capabilities
	if capabilitiesSubset(plain, held) {
		t.Fatal("a child without approval is a subset of a parent requiring it")
	}
	if !capabilitiesSubset(held, plain) {
		t.Fatal("adding approval is narrowing and must be allowed")
	}
	if !capabilitiesSubset(held, held) || !capabilitiesSubset(plain, plain) {
		t.Fatal("an unchanged declaration is not its own subset")
	}
	// The clone a reader is handed keeps it.
	cloned := cloneDescriptor(approvalDigestMutation(true).Descriptor)
	if !cloned.Capabilities[0].Actions[0].RequiresApproval {
		t.Fatal("cloneDescriptor dropped approval")
	}
	canonical, err := canonicalCapabilities(held)
	if err != nil || !canonical[0].Actions[0].RequiresApproval {
		t.Fatalf("canonicalCapabilities dropped approval: %v", err)
	}
}

func approvalTestRecord(t *testing.T) ActionRecord {
	t.Helper()
	now := time.Unix(1_800_000_000, 0).UTC()
	reason, err := interaction.NewReason("test", "detail")
	if err != nil {
		t.Fatal(err)
	}
	return ActionRecord{
		ID: []byte("held"), IdempotencyKey: []byte("key"), Version: 1,
		State: DispatchQueued, AgentID: "agent", AgentGeneration: 1,
		Capability: "ops", Action: "deploy",
		SourceID: []byte("s"), PolicyID: []byte("p"), ObjectID: "object",
		Input: json.RawMessage(`{"a":1}`), Subject: "alice", Actor: "alice-agent",
		ClientID: "client", OnBehalfOf: []shoal.ID{"dave"},
		AuthorizationFingerprint: auth.Fingerprint{1},
		PolicyGeneration:         3, AuthorizationExpiresAt: now.Add(time.Hour),
		AuthorizedOperations: []auth.Operation{auth.OperationDispatch},
		RequestID:            "request", Reason: reason,
		Deadline: now.Add(2 * time.Hour), CreatedAt: now, UpdatedAt: now,
		ExecutorKey: executorKey([]byte("held"), []byte("key")),
	}
}

// TestApprovalDigestCoversWhatEquivalentEnqueueCompares mutates every field
// equivalentEnqueue compares and requires the digest to move with it, and
// mutates fields it does not compare and requires the digest to stay. The two
// must agree, or an approver could review one request and a different one be
// recognised as it.
func TestApprovalDigestCoversWhatEquivalentEnqueueCompares(t *testing.T) {
	base := approvalTestRecord(t)
	original := ApprovalRequestDigest(base)
	compared := map[string]func(*ActionRecord){
		"id":          func(r *ActionRecord) { r.ID = []byte("held2") },
		"key":         func(r *ActionRecord) { r.IdempotencyKey = []byte("key2") },
		"agent":       func(r *ActionRecord) { r.AgentID = "agent2" },
		"generation":  func(r *ActionRecord) { r.AgentGeneration = 2 },
		"capability":  func(r *ActionRecord) { r.Capability = "ops2" },
		"action":      func(r *ActionRecord) { r.Action = "deploy2" },
		"source":      func(r *ActionRecord) { r.SourceID = []byte("s2") },
		"policy":      func(r *ActionRecord) { r.PolicyID = []byte("p2") },
		"object":      func(r *ActionRecord) { r.ObjectID = "object2" },
		"input":       func(r *ActionRecord) { r.Input = json.RawMessage(`{"a":2}`) },
		"subject":     func(r *ActionRecord) { r.Subject = "mallory" },
		"actor":       func(r *ActionRecord) { r.Actor = "mallory" },
		"client":      func(r *ActionRecord) { r.ClientID = "other" },
		"delegation":  func(r *ActionRecord) { r.OnBehalfOf = []shoal.ID{"erin"} },
		"fingerprint": func(r *ActionRecord) { r.AuthorizationFingerprint = auth.Fingerprint{2} },
		"policy gen":  func(r *ActionRecord) { r.PolicyGeneration = 4 },
		"reason code": func(r *ActionRecord) { r.Reason.Code = "other" },
		"reason":      func(r *ActionRecord) { r.Reason.Digest = "other" },
		"deadline":    func(r *ActionRecord) { r.Deadline = r.Deadline.Add(time.Nanosecond) },
	}
	for name, mutate := range compared {
		changed := cloneActionRecord(base)
		mutate(&changed)
		if equivalentEnqueue(changed, base) {
			t.Fatalf("%s: equivalentEnqueue does not compare it; the table "+
				"is stale", name)
		}
		if bytes.Equal(ApprovalRequestDigest(changed), original) {
			t.Fatalf("%s changes the request but not its digest", name)
		}
	}
	notCompared := map[string]func(*ActionRecord){
		"request ID": func(r *ActionRecord) { r.RequestID = "other" },
		"created":    func(r *ActionRecord) { r.CreatedAt = r.CreatedAt.Add(time.Second) },
		"expiry":     func(r *ActionRecord) { r.AuthorizationExpiresAt = r.AuthorizationExpiresAt.Add(time.Second) },
	}
	for name, mutate := range notCompared {
		changed := cloneActionRecord(base)
		mutate(&changed)
		if !equivalentEnqueue(changed, base) ||
			!bytes.Equal(ApprovalRequestDigest(changed), original) {
			t.Fatalf("%s: digest and equivalentEnqueue disagree", name)
		}
	}
}

func validApprovalRecord(t *testing.T) ApprovalRecord {
	t.Helper()
	base := approvalTestRecord(t)
	return ApprovalRecord{
		ID: []byte("held"), Version: 1, State: ApprovalPending, Request: base,
		RequestDigest: ApprovalRequestDigest(base), PolicyGeneration: 3,
		RequestedAt: base.CreatedAt, ExpiresAt: base.CreatedAt.Add(time.Hour),
		UpdatedAt: base.CreatedAt,
	}
}

func decided(record ApprovalRecord, state ApprovalState, verdict ApprovalVerdict) ApprovalRecord {
	record.Version++
	record.State = state
	record.Verdict = verdict
	record.ApproverSubject, record.ApproverActor = "bob", "bob-console"
	record.ApproverFingerprint = auth.Fingerprint{9}
	record.ApproverPolicyGeneration = record.PolicyGeneration
	record.DecidedAt = record.RequestedAt.Add(time.Minute)
	record.DecisionRequestID = "decision"
	record.UpdatedAt = record.DecidedAt
	return record
}

// TestApprovalRecordValidation refuses every record shape the service never
// writes, because a store holding one has been written by something else.
func TestApprovalRecordValidation(t *testing.T) {
	valid := validApprovalRecord(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid pending record: %v", err)
	}
	approved := decided(valid, ApprovalApproved, ApprovalVerdictApprove)
	if err := approved.Validate(); err != nil {
		t.Fatalf("valid approved record: %v", err)
	}
	enqueued := approved
	enqueued.Version++
	enqueued.State = ApprovalEnqueued
	enqueued.MaterializedAt = approved.DecidedAt.Add(time.Minute)
	if err := enqueued.Validate(); err != nil {
		t.Fatalf("valid enqueued record: %v", err)
	}
	for name, broken := range map[string]func() ApprovalRecord{
		"digest of another request": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.Request.Input = json.RawMessage(`{"a":2}`)
			return r
		},
		"tampered digest": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.RequestDigest[0] ^= 1
			return r
		},
		"pending with a verdict": func() ApprovalRecord {
			r := decided(valid, ApprovalPending, ApprovalVerdictApprove)
			return r
		},
		"approved without an approver": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.State, r.Verdict = ApprovalApproved, ApprovalVerdictApprove
			return r
		},
		"approved with a refusal": func() ApprovalRecord {
			return decided(valid, ApprovalApproved, ApprovalVerdictRefuse)
		},
		"enqueued without a materialization time": func() ApprovalRecord {
			r := CloneApprovalRecord(enqueued)
			r.MaterializedAt = time.Time{}
			return r
		},
		"enqueued without approval": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.State = ApprovalEnqueued
			r.MaterializedAt = r.RequestedAt.Add(time.Minute)
			return r
		},
		"materialized after the window": func() ApprovalRecord {
			r := CloneApprovalRecord(enqueued)
			r.MaterializedAt = r.ExpiresAt
			return r
		},
		"decided after the window": func() ApprovalRecord {
			r := CloneApprovalRecord(approved)
			r.DecidedAt = r.ExpiresAt
			return r
		},
		"approver on another generation": func() ApprovalRecord {
			r := CloneApprovalRecord(approved)
			r.ApproverPolicyGeneration++
			return r
		},
		"expired refusal": func() ApprovalRecord {
			return decided(valid, ApprovalExpired, ApprovalVerdictRefuse)
		},
		"window past the deadline": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.ExpiresAt = r.Request.Deadline.Add(time.Second)
			return r
		},
		"request is not a fresh queued record": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.Request.Version = 2
			return r
		},
		"reserved identity": func() ApprovalRecord {
			r := CloneApprovalRecord(valid)
			r.ID = []byte(admissionIDPrefix + "x")
			r.Request.ID = r.ID
			r.RequestDigest = ApprovalRequestDigest(r.Request)
			return r
		},
	} {
		if err := broken().Validate(); err == nil {
			t.Fatalf("%s: Validate accepted it", name)
		}
	}
}

// TestActionApprovalProvenanceIsAllOrNothing pins the ActionRecord side.
func TestActionApprovalProvenanceIsAllOrNothing(t *testing.T) {
	record := approvalTestRecord(t)
	record.ApprovalRequestDigest = ApprovalRequestDigest(record)
	record.ApprovalPolicyGeneration = record.PolicyGeneration
	record.ApproverSubject, record.ApproverActor = "bob", "bob-console"
	record.ApprovedAt = record.CreatedAt
	if err := record.Validate(); err != nil {
		t.Fatalf("complete approval provenance: %v", err)
	}
	cloned := cloneActionRecord(record)
	cloned.ApprovalRequestDigest[0] ^= 1
	if record.ApprovalRequestDigest[0] == cloned.ApprovalRequestDigest[0] {
		t.Fatal("cloneActionRecord shares the approval digest")
	}
	for name, mutate := range map[string]func(*ActionRecord){
		"no approver":        func(r *ActionRecord) { r.ApproverSubject = "" },
		"no time":            func(r *ActionRecord) { r.ApprovedAt = time.Time{} },
		"short digest":       func(r *ActionRecord) { r.ApprovalRequestDigest = r.ApprovalRequestDigest[:8] },
		"another generation": func(r *ActionRecord) { r.ApprovalPolicyGeneration++ },
		"approver without digest": func(r *ActionRecord) {
			r.ApprovalRequestDigest = nil
		},
		"on an admission": func(r *ActionRecord) {
			r.AdmittedEffects = Effects{EffectMutatesExternal}
		},
	} {
		changed := cloneActionRecord(record)
		mutate(&changed)
		if err := changed.Validate(); err == nil {
			t.Fatalf("%s: Validate accepted it", name)
		}
	}
}

// TestApprovalRequiredCarriesADeliberateCode pins the code every client
// branches on. See approvalRequired for why it is conflict.
func TestApprovalRequiredCarriesADeliberateCode(t *testing.T) {
	err := approvalRequired()
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) ||
		!strings.Contains(err.Error(), "approval route") {
		t.Fatalf("approval required = %v", err)
	}
}
