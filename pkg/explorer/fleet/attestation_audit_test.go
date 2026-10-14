// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// auditCapture keeps every audit the dispatch service hands its recorder, so
// a test can read who a refusal names.
type auditCapture struct {
	*dispatchRecorder
	mu     sync.Mutex
	audits []ActionAudit
}

func (c *auditCapture) RecordAction(ctx context.Context, audit ActionAudit) error {
	c.mu.Lock()
	c.audits = append(c.audits, ActionAudit{
		Phase: audit.Phase, Operation: audit.Operation, Record: cloneActionRecord(audit.Record),
	})
	c.mu.Unlock()
	return c.dispatchRecorder.RecordAction(ctx, audit)
}

func (c *auditCapture) refusals() []ActionRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ActionRecord
	for _, audit := range c.audits {
		if audit.Phase == ClaimRefusedAttestationPhase {
			out = append(out, audit.Record)
		}
	}
	return out
}

func requireRefusalsName(t *testing.T, refusals []ActionRecord, subjects ...shoal.ID) {
	t.Helper()
	if len(refusals) != len(subjects) {
		t.Fatalf("%d refusal audits, want %d", len(refusals), len(subjects))
	}
	seen := map[shoal.ID]bool{}
	for i, refusal := range refusals {
		if refusal.ClaimantSubject != subjects[i] ||
			refusal.ClaimantClientID != shoal.ID(string(subjects[i])+"-client") ||
			refusal.ClaimantActor != shoal.ID(string(subjects[i])+"-actor") {
			t.Fatalf("refusal %d names %q/%q/%q, want %q", i, refusal.ClaimantSubject,
				refusal.ClaimantActor, refusal.ClaimantClientID, subjects[i])
		}
		if refusal.TransitionRequestID == "" || seen[refusal.TransitionRequestID] {
			t.Fatalf("refusal %d does not carry its own request ID: %q", i, refusal.TransitionRequestID)
		}
		seen[refusal.TransitionRequestID] = true
		if refusal.TransitionCorrelationID == "" {
			t.Fatalf("refusal %d has no correlation ID", i)
		}
	}
	if reflect.DeepEqual(refusals[0], refusals[len(refusals)-1]) && len(refusals) > 1 {
		t.Fatal("two refusals produced identical audits")
	}
}

func TestClaimRefusalsAreAuditedUnderTheRefusedCaller(t *testing.T) {
	f := newAttestationFixture(t, true)
	before, _ := f.store.GetAction(context.Background(), f.queued.ID)
	for _, name := range []string{"alpha", "beta"} {
		_, err := f.claim(t, name, time.Minute)
		requireAttestationRefusal(t, err)
	}
	requireRefusalsName(t, f.recorder.refusals(), "alpha", "beta")
	after, _ := f.store.GetAction(context.Background(), f.queued.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the audit copy was persisted:\n%+v\n%+v", before, after)
	}
}

func TestExtensionRefusalsAreAuditedUnderTheRefusedCaller(t *testing.T) {
	f := newAttestationFixture(t, true)
	// beta held it first, so the stored claimant history is not alpha.
	f.attest("beta", f.now().Add(90*time.Second))
	if _, err := f.claim(t, "beta", time.Minute); err != nil {
		t.Fatal(err)
	}
	f.advance(61 * time.Second)
	f.attest("alpha", f.now().Add(2*time.Minute))
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.GetAction(context.Background(), f.queued.ID)
	for range 2 {
		f.advance(time.Second)
		ctx, request := f.worker(t, "alpha")
		_, err := f.service.ExtendClaim(ctx, ExtendRequest{
			ID: f.queued.ID, ExpectedVersion: claimed.Version, ClaimID: []byte("alpha-claim"),
			Lease: 5 * time.Minute, Context: request,
		})
		requireAttestationRefusal(t, err)
	}
	refusals := f.recorder.refusals()
	requireRefusalsName(t, refusals[len(refusals)-2:], "alpha", "alpha")
	after, _ := f.store.GetAction(context.Background(), f.queued.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the extension audit copy was persisted")
	}
}

func TestAdmissionRefusalsAreAuditedUnderTheRefusedCaller(t *testing.T) {
	f := newAttestationFixture(t, true)
	for _, name := range []string{"alpha", "beta"} {
		decision := dispatchDecisionFor(t, principal{
			subject: name, actor: name + "-actor", request: name + "-admission",
			clientID: shoal.ID(name + "-client"),
		}, auth.OperationInvoke)
		grant, err := f.admission.Request(bindDecision(t, f.authority, decision), AdmissionRequest{
			ID: []byte("admission"), IdempotencyKey: []byte("key"), TokenID: []byte("token"),
			AgentID: "agent", AgentGeneration: 1, Capability: "search", Action: "query",
			SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
			Effects: Effects{EffectMutatesExternal}, Input: json.RawMessage(`{"value":1}`),
			Lease: time.Minute, Context: dispatchContext(f.now(), name+"-admission"),
		})
		if err != nil || grant.Outcome != AdmissionDenied {
			t.Fatalf("%s admission = %+v, %v", name, grant, err)
		}
		stored, err := f.store.GetAction(context.Background(), admissionActionID(decision, []byte("admission")))
		if err != nil || stored.ClaimantSubject != "" || stored.State != DispatchCanceled {
			t.Fatalf("the denial record carries the audit copy: %+v, %v", stored, err)
		}
	}
	requireRefusalsName(t, f.recorder.refusals(), "alpha", "beta")
}

// The gate reads the action from the *current* descriptor (resolveActionBinding
// reads the stored descriptor, not a snapshot on the record), so a requirement
// registered after a record was enqueued governs that record whenever it
// resolves at all. Generation pinning makes such a record stop resolving
// today; these tests hold that no unattested claim or extension is granted
// whether or not pinning holds. Revisit with #486, which may unpin it.
func flipOnAttestation(f *attestationFixture) {
	stored := f.registryRows.records["agent"]
	stored.Descriptor.Generation++
	stored.Descriptor.Capabilities[0].Actions[0].RequiresAttestation = true
	f.registryRows.records["agent"] = stored
}

// requireFlipRefusal accepts the two answers a record from before the flip can
// get: not found while generation pinning holds (today), or the attestation
// refusal if pinning is lifted (#486). Never a grant.
func requireFlipRefusal(t *testing.T, err error) {
	t.Helper()
	switch {
	case err == nil:
		t.Fatal("a record from before the flip was granted without attestation")
	case shoal.IsErrorCode(err, shoal.ErrorNotFound):
	case errors.Is(err, ErrAttestationRequired):
	default:
		t.Fatalf("after the flip = %v", err)
	}
}

func TestFlipOnThenClaimNeverGrantsAnUnattestedClaim(t *testing.T) {
	f := newAttestationFixture(t, false)
	flipOnAttestation(f)
	_, err := f.claim(t, "alpha", time.Minute)
	requireFlipRefusal(t, err)
}

// TestClaimThenFlipOnThenExtendIsRefusedUnlessAttested pins what a live claim
// does when the requirement is registered under it: it is not revoked and runs
// to its CURRENT lease end — the end it was granted, which is already bounded
// by the action deadline (the same clamp ExtendClaim applies), not
// now+MaxActionClaimTTL — and no extension is granted to an unattested holder.
func TestClaimThenFlipOnThenExtendIsRefusedUnlessAttested(t *testing.T) {
	f := newAttestationFixture(t, false)
	// Claim 90s before the deadline with a one-minute lease: the lease ends
	// 30s before the deadline, and an extension could only reach the
	// deadline (the clamp), never claim time plus MaxActionClaimTTL.
	f.advance(time.Hour - 90*time.Second)
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	window := f.queued.Deadline.Add(-30 * time.Second)
	if !claimed.ClaimLeaseUntil.Equal(window) {
		t.Fatalf("lease end %s, want %s", claimed.ClaimLeaseUntil, window)
	}
	flipOnAttestation(f)
	f.advance(10 * time.Second)
	ctx, request := f.worker(t, "alpha")
	_, err = f.service.ExtendClaim(ctx, ExtendRequest{
		ID: f.queued.ID, ExpectedVersion: claimed.Version, ClaimID: []byte("alpha-claim"),
		Lease: 5 * time.Minute, Context: request,
	})
	requireFlipRefusal(t, err)
	// The claim keeps exactly its current window: it ends at its granted
	// lease end, not at the deadline the extension would have clamped to.
	after, _ := f.store.GetAction(context.Background(), f.queued.ID)
	if after.Version != claimed.Version || after.State != DispatchClaimed ||
		!after.ClaimLeaseUntil.Equal(window) {
		t.Fatalf("the live claim's window changed: %+v", after)
	}
}

// TestEffectiveClaimRequirementsStricterWins pins the helper directly.
func TestEffectiveClaimRequirementsStricterWins(t *testing.T) {
	for _, test := range []struct {
		current, attestedClaim, want bool
	}{{false, false, false}, {true, false, true}, {false, true, true}, {true, true, true}} {
		record := ActionRecord{}
		if test.attestedClaim {
			record.ClaimAttestationID = "exattest:1"
		}
		got := effectiveClaimRequirements(record, Action{RequiresAttestation: test.current})
		if got.Attestation != test.want {
			t.Fatalf("current=%v claim=%v: %v", test.current, test.attestedClaim, got)
		}
	}
}
