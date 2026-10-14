// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Model-level tests for executor attestation required by policy (#446 slice
// 3). The verifier and store are internal/executorattest's, adapted in
// cmd/shoal-explore-web, where the composed acceptance tests run them against
// the durable dispatch store; here a fake stands in so each guard can be
// pinned and mutation-checked on its own.

type fakeAttestations struct {
	mu    sync.Mutex
	rows  map[string]ExecutorAttestation
	err   error
	calls int
}

func attestationKey(p AttestationPrincipal, ref string) string {
	return string(p.Domain) + "|" + string(p.Subject) + "|" + string(p.ClientID) + "|" + ref
}

func (f *fakeAttestations) set(subject, client string, id string, expires time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]ExecutorAttestation{}
	}
	f.rows[attestationKey(AttestationPrincipal{
		Domain: []byte("domain"), Subject: shoal.ID(subject), ClientID: shoal.ID(client),
	}, "exec")] = ExecutorAttestation{ID: shoal.ID(id), ExpiresAt: expires, OK: true}
}

// Current mirrors executorattest.Store.Current with needUntil = now: a row
// that has expired by the clock it is handed is not current.
func (f *fakeAttestations) Current(
	_ context.Context, principal AttestationPrincipal, ref string, now time.Time,
) (ExecutorAttestation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return ExecutorAttestation{}, f.err
	}
	row, ok := f.rows[attestationKey(principal, ref)]
	if !ok || !now.Before(row.ExpiresAt) {
		return ExecutorAttestation{}, nil
	}
	return row, nil
}

func (f *fakeAttestations) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type trustRefs map[string]bool

func (t trustRefs) Configured(ref string) bool { return t[ref] }

type attestationFixture struct {
	service      *DispatchService
	admission    *AdmissionService
	registry     *Service
	store        *memoryDispatchStore
	registryRows *memoryStore
	recorder     *auditCapture
	attestations *fakeAttestations
	authority    *auth.Authority
	queued       ActionRecord
	clock        *time.Time
}

func (f *attestationFixture) now() time.Time { return *f.clock }

func (f *attestationFixture) advance(by time.Duration) { *f.clock = f.clock.Add(by) }

func attestedDescriptor(now time.Time) Descriptor {
	descriptor := dispatchDescriptor(now)
	descriptor.LeaseExpiresAt = now.Add(24 * time.Hour)
	action := &descriptor.Capabilities[0].Actions[0]
	action.Effects = Effects{EffectMutatesExternal}
	action.RequiresAttestation = true
	return descriptor
}

func newAttestationFixture(t *testing.T, require bool) *attestationFixture {
	t.Helper()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := &now
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return *clock })
	if err != nil {
		t.Fatal(err)
	}
	registryRows := newMemoryStore()
	registry, err := NewService(Config{
		Store: registryRows, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now}, Executors: executorMap{"exec": &remoteBoundExecutor{}},
		Clock: func() time.Time { return *clock }, AttestationTrust: trustRefs{"exec": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := attestedDescriptor(now)
	descriptor.Capabilities[0].Actions[0].RequiresAttestation = require
	registryRows.records["agent"] = Stored{Descriptor: descriptor}
	store := newMemoryDispatchStore()
	recorder := &auditCapture{dispatchRecorder: &dispatchRecorder{}}
	attestations := &fakeAttestations{}
	service, err := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: recorder, Events: dispatchEvents{},
		Clock: func() time.Time { return *clock }, Attestations: attestations,
	})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := NewAdmissionService(AdmissionConfig{Dispatch: service})
	if err != nil {
		t.Fatal(err)
	}
	enqueuer := bindDecision(t, authority, dispatchDecision(t,
		"owner", "actor", "request", auth.OperationDispatch, auth.OperationInvoke))
	queued, err := service.Enqueue(enqueuer, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}
	return &attestationFixture{
		service: service, admission: admission, registry: registry, store: store,
		registryRows: registryRows, recorder: recorder, attestations: attestations,
		authority: authority, queued: queued, clock: clock,
	}
}

// worker binds an execute-holding principal with a client, as an attested
// executor process authenticates.
func (f *attestationFixture) worker(t *testing.T, name string, onBehalfOf ...shoal.ID) (context.Context, RequestContext) {
	t.Helper()
	request := name + "-request-" + hex.EncodeToString([]byte(f.now().Format(time.RFC3339Nano)))
	who := principal{
		subject: name, actor: name + "-actor", request: request,
		clientID: shoal.ID(name + "-client"), onBehalfOf: onBehalfOf,
	}
	// Bound to the descriptor's executor, as a worker is since #391. A
	// delegated caller cannot be bound, so it stays an unbound execute-holder,
	// which the execute route refuses.
	decision := executorDecisionFor(t, who, "exec")
	if len(onBehalfOf) > 0 {
		decision = dispatchDecisionFor(t, who, auth.OperationExecute)
	}
	return bindDecision(t, f.authority, decision), dispatchContext(f.now(), request)
}

func (f *attestationFixture) attest(name string, expires time.Time) {
	f.attestations.set(name, name+"-client", "exattest:"+name+"-"+expires.Format(time.RFC3339), expires)
}

func (f *attestationFixture) claim(t *testing.T, name string, lease time.Duration) (ActionRecord, error) {
	t.Helper()
	ctx, request := f.worker(t, name)
	current, err := f.store.GetAction(context.Background(), f.queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f.service.Claim(ctx, ClaimRequest{
		ID: f.queued.ID, ExpectedVersion: current.Version,
		ClaimID: []byte(name + "-claim"), Lease: lease, Context: request,
	})
}

func requireAttestationRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrAttestationRequired) || !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("err = %v, want ErrAttestationRequired as a conflict", err)
	}
	if errors.Is(err, ErrExecutionAmbiguous) || errors.Is(err, ErrActionCommitted) ||
		explorer.IsIndeterminateCommit(err) {
		t.Fatalf("an attestation refusal is pre-commit and must never be joined "+
			"with an indeterminate sentinel: %v", err)
	}
	for _, leak := range []string{"exattest:", "sha256:", "verifier"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("refusal %q discloses %q", err, leak)
		}
	}
}

func TestRegistryDigestIsUnchangedWithoutAttestation(t *testing.T) {
	// The same golden TestRegistryDigestIsUnchangedWithoutApproval pins,
	// computed by the build before either field existed.
	const golden = "54af49ed8009c6c7c8d4862c19d5875176dfa8dbe51dd5188521a0c8b672138a"
	plain := registryMutationDigestV1(approvalDigestMutation(false))
	if got := hex.EncodeToString(plain[:]); got != golden {
		t.Fatalf("v1 mutation digest without attestation = %s, want %s", got, golden)
	}
	attested := approvalDigestMutation(false)
	attested.Descriptor.Capabilities[0].Actions[0].RequiresAttestation = true
	both := approvalDigestMutation(true)
	both.Descriptor.Capabilities[0].Actions[0].RequiresAttestation = true
	for _, version := range registryDigestVersions {
		attestedDigest := version.digest(attested)
		if attestedDigest == version.digest(approvalDigestMutation(false)) {
			t.Fatalf("%s: requiring attestation did not change the mutation "+
				"digest", version.name)
		}
		if version.digest(both) == attestedDigest ||
			version.digest(both) == version.digest(approvalDigestMutation(true)) {
			t.Fatalf("%s: approval and attestation are not separately bound "+
				"in the digest", version.name)
		}
	}
	encoded, err := json.Marshal(approvalDigestMutation(false).Descriptor.Capabilities[0].Actions[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("requires_attestation")) {
		t.Fatalf("an action without attestation marshals the key: %s", encoded)
	}
	encoded, _ = json.Marshal(attested.Descriptor.Capabilities[0].Actions[0])
	var decoded Action
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.RequiresAttestation {
		t.Fatalf("attestation did not round-trip through JSON: %s, %v", encoded, err)
	}
	if !bytes.Contains(encoded, []byte(`"requires_attestation":true`)) {
		t.Fatalf("wire key missing: %s", encoded)
	}
}

func TestDelegationCannotDropAttestation(t *testing.T) {
	attested := approvalDigestMutation(false).Descriptor.Capabilities
	attested[0].Actions[0].RequiresAttestation = true
	plain := approvalDigestMutation(false).Descriptor.Capabilities
	if capabilitiesSubset(plain, attested) {
		t.Fatal("a child without attestation is a subset of a parent requiring it")
	}
	if !capabilitiesSubset(attested, plain) || !capabilitiesSubset(attested, attested) {
		t.Fatal("adding or keeping attestation must be allowed")
	}
	if !cloneDescriptor(Descriptor{Capabilities: attested}).Capabilities[0].Actions[0].RequiresAttestation {
		t.Fatal("cloneDescriptor dropped attestation")
	}
	canonical, err := canonicalCapabilities(attested)
	if err != nil || !canonical[0].Actions[0].RequiresAttestation {
		t.Fatalf("canonicalCapabilities dropped attestation: %v", err)
	}
	// Refused without the external effect, egress included.
	for _, effects := range []Effects{nil, {EffectReadsCorpus}, {EffectEgressesContent}} {
		wrong := approvalDigestMutation(false).Descriptor.Capabilities
		wrong[0].Actions[0].Effects = effects
		wrong[0].Actions[0].RequiresAttestation = true
		if _, err := canonicalCapabilities(wrong); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("attestation on %v = %v, want invalid argument", effects, err)
		}
	}
}

func TestRegisterRefusesAttestationWithoutATrustRoot(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		trust AttestationTrust
		ok    bool
	}{
		{"nil trust configures none", nil, false},
		{"another ref is configured", trustRefs{"other": true}, false},
		{"configured", trustRefs{"exec": true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(Config{
				Store: newMemoryStore(), Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
				Snapshots: fixedSnapshot{now}, Executors: executorMap{"exec": &remoteBoundExecutor{}},
				Clock: func() time.Time { return now }, AttestationTrust: test.trust,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := registerRequest(now, "register", "agent", "", "source")
			action := &request.Spec.Capabilities[0].Actions[0]
			action.Effects, action.RequiresAttestation = Effects{EffectMutatesExternal}, true
			ctx := bindDecision(t, authority, testDecision(t, "owner", "owner-actor", "register", [][]byte{[]byte("source")}))
			_, err = service.Register(ctx, request)
			if test.ok != (err == nil) {
				t.Fatalf("Register = %v, want ok=%v", err, test.ok)
			}
			if !test.ok && (!shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
				!strings.Contains(err.Error(), "attestation trust root")) {
				t.Fatalf("refusal = %v", err)
			}
		})
	}
}

func TestClaimRequiresACurrentAttestationCoveringTheLease(t *testing.T) {
	f := newAttestationFixture(t, true)

	// No attestation at all.
	_, err := f.claim(t, "alpha", time.Minute)
	requireAttestationRefusal(t, err)
	if got := f.recorder.recordedOperation("claim_refused_attestation"); got != auth.OperationExecute {
		t.Fatalf("refusal audited under %q, want execute", got)
	}
	unchanged, _ := f.store.GetAction(context.Background(), f.queued.ID)
	if unchanged.Version != f.queued.Version || unchanged.State != DispatchQueued {
		t.Fatalf("a refused claim wrote: %+v", unchanged)
	}

	// An attestation that lapses one nanosecond before the lease would end.
	f.attest("alpha", f.now().Add(time.Minute-time.Nanosecond))
	_, err = f.claim(t, "alpha", time.Minute)
	requireAttestationRefusal(t, err)

	// One that ends exactly at the lease end covers it.
	f.attest("alpha", f.now().Add(time.Minute))
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatalf("claim under a covering attestation: %v", err)
	}
	if want := shoal.ID("exattest:alpha-" + f.now().Add(time.Minute).Format(time.RFC3339)); claimed.ClaimAttestationID != want {
		t.Fatalf("ClaimAttestationID = %q, want %q", claimed.ClaimAttestationID, want)
	}
	if err := claimed.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimGateUsesTheClampedLeaseEnd(t *testing.T) {
	f := newAttestationFixture(t, true)
	// The action's deadline is an hour out; move the clock so a five-minute
	// lease clamps to it, and attest only to the deadline.
	f.advance(time.Hour - 2*time.Minute)
	f.attest("alpha", f.queued.Deadline)
	claimed, err := f.claim(t, "alpha", 5*time.Minute)
	if err != nil {
		t.Fatalf("a claim whose clamped end the attestation covers was refused: %v", err)
	}
	if !claimed.ClaimLeaseUntil.Equal(f.queued.Deadline) {
		t.Fatalf("lease end %s, want the deadline %s", claimed.ClaimLeaseUntil, f.queued.Deadline)
	}
}

func TestStoreFailureIsUnavailableNotARefusal(t *testing.T) {
	f := newAttestationFixture(t, true)
	f.attestations.err = errors.New("backend down: sha256:deadbeef")
	_, err := f.claim(t, "alpha", time.Minute)
	if !errors.Is(err, ErrAttestationUnavailable) || !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		errors.Is(err, ErrAttestationRequired) || explorer.IsIndeterminateCommit(err) {
		t.Fatalf("store failure = %v, want an unmarked unavailable", err)
	}
	if strings.Contains(err.Error(), "sha256") {
		t.Fatalf("store failure leaked its cause: %v", err)
	}
}

func TestExtendClaimIsGatedOnTheClampedEndAndTheClaimSurvivesARefusal(t *testing.T) {
	f := newAttestationFixture(t, true)
	f.attest("alpha", f.now().Add(2*time.Minute))
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	firstAttestation := claimed.ClaimAttestationID
	f.advance(30 * time.Second)
	ctx, request := f.worker(t, "alpha")
	extend := func(lease time.Duration) (ActionRecord, error) {
		current, _ := f.store.GetAction(context.Background(), f.queued.ID)
		return f.service.ExtendClaim(ctx, ExtendRequest{
			ID: f.queued.ID, ExpectedVersion: current.Version,
			ClaimID: []byte("alpha-claim"), Lease: lease, Context: request,
		})
	}
	// Past the attestation's expiry: refused, and the claim is untouched.
	_, err = extend(5 * time.Minute)
	requireAttestationRefusal(t, err)
	survived, _ := f.store.GetAction(context.Background(), f.queued.ID)
	if survived.Version != claimed.Version || survived.State != DispatchClaimed ||
		!survived.ClaimLeaseUntil.Equal(claimed.ClaimLeaseUntil) {
		t.Fatalf("a refused extension changed the claim: %+v", survived)
	}
	// Within it: granted.
	if _, err := extend(90 * time.Second); err != nil {
		t.Fatalf("an extension the attestation covers: %v", err)
	}
	// Re-attested: the extension is granted and the record names the new one.
	f.attest("alpha", f.now().Add(10*time.Minute))
	extended, err := extend(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if extended.ClaimAttestationID == firstAttestation || extended.ClaimAttestationID == "" {
		t.Fatalf("extension did not record the attestation that covers it: %q", extended.ClaimAttestationID)
	}

}

func TestExtendClaimGateUsesTheClampedEnd(t *testing.T) {
	f := newAttestationFixture(t, true)
	// Near the deadline, a long extension clamps to it, and an attestation
	// covering only the deadline suffices. Judged against the unclamped end
	// it would be refused.
	f.advance(time.Hour - 3*time.Minute)
	f.attest("alpha", f.queued.Deadline)
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, request := f.worker(t, "alpha")
	extended, err := f.service.ExtendClaim(ctx, ExtendRequest{
		ID: f.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("alpha-claim"), Lease: 5 * time.Minute, Context: request,
	})
	if err != nil {
		t.Fatalf("a clamped extension the attestation covers: %v", err)
	}
	if !extended.ClaimLeaseUntil.Equal(f.queued.Deadline) {
		t.Fatalf("extended to %s, want the deadline", extended.ClaimLeaseUntil)
	}
}

// TestALapsedClaimIsTakenOverByAnAttestedWorker: the lapse coincides with the
// lease lapse, the fence covers it, and exactly one completion is recorded.
func TestALapsedClaimIsTakenOverByAnAttestedWorker(t *testing.T) {
	f := newAttestationFixture(t, true)
	f.attest("alpha", f.now().Add(90*time.Second))
	first, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.advance(61 * time.Second)
	// beta has no attestation yet: refused even though the lease lapsed.
	_, err = f.claim(t, "beta", time.Minute)
	requireAttestationRefusal(t, err)
	f.attest("beta", f.now().Add(5*time.Minute))
	second, err := f.claim(t, "beta", time.Minute)
	if err != nil {
		t.Fatalf("an attested worker could not take over a lapsed claim: %v", err)
	}
	if second.ClaimAttestationID == first.ClaimAttestationID ||
		len(second.ClaimHistory) != 1 ||
		second.ClaimHistory[0].AttestationID != first.ClaimAttestationID {
		t.Fatalf("the attestation did not move with the claim: %q, history %+v",
			second.ClaimAttestationID, second.ClaimHistory)
	}
	// alpha's completion is refused; its ambiguity report is accepted.
	alpha, alphaRequest := f.worker(t, "alpha")
	_, err = f.service.CompleteClaim(alpha, CompletionRequest{
		ID: f.queued.ID, ExpectedVersion: first.Version, ClaimID: []byte("alpha-claim"),
		ClaimFence: first.ClaimFence, Result: ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: alphaRequest,
	})
	if err == nil {
		t.Fatal("the displaced holder completed")
	}
	if _, err := f.service.ReportAmbiguity(alpha, AmbiguityRequest{
		ID: f.queued.ID, ClaimFence: first.ClaimFence,
		Outcome: AmbiguityOutcomeUnknown, Context: alphaRequest,
	}); err != nil {
		t.Fatalf("the displaced holder's ambiguity report: %v", err)
	}
	// beta completes even after its attestation is withdrawn: completion is
	// never gated.
	f.attestations.rows = nil
	beta, betaRequest := f.worker(t, "beta")
	current, _ := f.store.GetAction(context.Background(), f.queued.ID)
	completed, err := f.service.CompleteClaim(beta, CompletionRequest{
		ID: f.queued.ID, ExpectedVersion: current.Version, ClaimID: []byte("beta-claim"),
		ClaimFence: second.ClaimFence, Result: ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: betaRequest,
	})
	if err != nil || completed.State != DispatchSucceeded {
		t.Fatalf("complete = %+v, %v", completed.State, err)
	}
	completions := 0
	for _, phase := range f.recorder.phases {
		if phase == "effect_outcome" {
			completions++
		}
	}
	if completions != 1 {
		t.Fatalf("completions recorded = %d (%v), want exactly one", completions, f.recorder.phases)
	}
}

func TestAnotherPrincipalsAttestationDoesNotCover(t *testing.T) {
	f := newAttestationFixture(t, true)
	f.attest("alpha", f.now().Add(time.Hour))
	_, err := f.claim(t, "beta", time.Minute)
	requireAttestationRefusal(t, err)
	// alpha acting on behalf of someone is not alpha attested as itself, even
	// with the delegate authority the binding requires. Since #391 it does
	// not reach the attestation gate at all: a delegated decision cannot be
	// executor-bound, and the execute route refuses an unbound or delegated
	// caller as not-found before the attestation store is read.
	decision := dispatchDecisionFor(t, principal{
		subject: "alpha", actor: "alpha-actor", request: "delegated",
		clientID: "alpha-client", onBehalfOf: []shoal.ID{"delegator"},
	}, auth.OperationExecute, auth.OperationDelegate)
	calls := f.attestations.callCount()
	_, err = f.service.Claim(bindDecision(t, f.authority, decision), ClaimRequest{
		ID: f.queued.ID, ExpectedVersion: f.queued.Version,
		ClaimID: []byte("delegated"), Lease: time.Minute,
		Context: dispatchContext(f.now(), "delegated"),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
		f.attestations.callCount() != calls {
		t.Fatalf("a delegated execute claim = %v, store reads %d -> %d; "+
			"want not-found with no store read", err, calls,
			f.attestations.callCount())
	}
	// And alpha as itself is covered, so the refusal above was the chain.
	if _, err := f.claim(t, "alpha", time.Minute); err != nil {
		t.Fatal(err)
	}
}

// What "without standing" means in the tests below, precisely.
//
// A caller has standing on an action when it is authorized on the action's
// *descriptor*: authorizedClaimant accepts it by one of its two routes —
// OperationExecute on the record's source and policy (#437, no principal
// requirement), or OperationInvoke as the enqueuing principal — and
// resolveActionBinding then accepts it against the live descriptor: the same
// authorization domain, a declared scope, and AuthorizeObject for the
// operation (and for OperationDelegate when it acts on someone's behalf).
//
// A caller without standing fails one of those. It is NOT "anyone but the
// enqueuer": an execute-holder in the descriptor's own scope and domain has
// standing, is meant to reach the attestation gate, and is told the
// requirement. A probe built from such a caller sees the conflict and reads as
// an oracle when it is the probe that is wrong (the lesson of #533's approval
// twin, TestAGateRefusalNeverPrecedesTheStandingCheck).
//
// The attestation gate's refusal is a distinguishable conflict, a deliberate
// departure from #398's rule that every standing refusal is not-found. That is
// sound only while the standing check runs first, so for every caller without
// standing the answer on an attestation-required action must be byte-for-byte
// what the same caller is told for an action that does not exist, and the
// attestation store must never be read.

// TestACallerWithoutStandingCannotTellTheRequirementExists: identical answers
// with and without the requirement, and the store is never consulted. The
// outsider holds only invoke and is not the enqueuer, so it fails both of
// authorizedClaimant's routes.
func TestACallerWithoutStandingCannotTellTheRequirementExists(t *testing.T) {
	answers := make([]string, 0, 4)
	for _, require := range []bool{true, false} {
		f := newAttestationFixture(t, require)
		// A client ID, so the store-count assertion below is not vacuous:
		// claimAttestation never reads the store for a decision without one,
		// so without it a gate that ran first would still count zero calls.
		outsider := bindDecision(t, f.authority, dispatchDecisionFor(t, principal{
			subject: "outsider", actor: "outsider-actor", request: "outsider-request",
			clientID: "outsider-client",
		}, auth.OperationInvoke))
		_, err := f.service.Claim(outsider, ClaimRequest{
			ID: f.queued.ID, ExpectedVersion: f.queued.Version, ClaimID: []byte("x"),
			Lease: time.Minute, Context: dispatchContext(f.now(), "outsider-request"),
		})
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("outsider claim = %v, want not found", err)
		}
		if f.attestations.callCount() != 0 {
			t.Fatal("the attestation store was consulted for a caller without standing")
		}
		answers = append(answers, err.Error())
	}
	if answers[0] != answers[1] {
		t.Fatalf("answers differ: %q vs %q", answers[0], answers[1])
	}
}

// stranger is a caller that holds the operation the route needs, and whose
// lack of standing on the fixture's descriptor is one specific thing.
type stranger struct {
	name string
	// where names the check that refuses it, so a failure names the layer.
	where      string
	domain     string
	source     string
	policy     string
	onBehalfOf []shoal.ID
	// storeCountable is false where claimAttestation would skip the store
	// even if the gate were reached (a delegated chain), so the count
	// assertion would be vacuous; the byte comparison still bites there.
	storeCountable bool
	// code is the error code the refusal must carry.
	//
	// Not always not-found, and the difference is the point. A refusal that
	// answers a question about the *object* has to be indistinguishable from
	// the object's absence, so it is not-found. A refusal that answers a
	// question about the *caller* — it holds no delegate grant, it holds no
	// operation — discloses nothing about any object, so it may say what it
	// is, and #536's fix moved those checks ahead of the lookup precisely so
	// they can. What both kinds must share is the byte comparison below:
	// identical whether or not the agent exists.
	code shoal.ErrorCode
}

// strangers hold the operation and a client ID, so none is refused for
// lacking the grant at begin and each would be attestation-checked if it
// reached the gate. What none has is standing on *this descriptor*.
//
// Variants considered and not constructible here:
//   - Failing AuthorizeObject on the domain. authorizedCurrentBinding passes
//     the decision's own domain as the resource's, so that comparison cannot
//     fail there; a foreign domain is refused by resolveActionBinding's
//     descriptor comparison instead, which is the "other domain" case.
//   - Inside the caller's grant but outside the descriptor's scopes. Enqueue
//     writes only a record whose scope the descriptor declares, and the
//     generation pin refuses a re-registration that narrows it, so every
//     scope failure lands at AuthorizeObject first. That is also why
//     weakening resolveActionBinding's scope loop alone is not a mutant these
//     tests can kill: AuthorizeObject refuses the same caller regardless.
var strangers = []stranger{
	{
		name: "other scope", code: shoal.ErrorNotFound, where: "AuthorizeObject (source and policy)",
		domain: "domain", source: "other-source", policy: "other-policy",
		storeCountable: true,
	},
	{
		name: "right source, other policy", code: shoal.ErrorNotFound, where: "AuthorizeObject (policy)",
		domain: "domain", source: "source", policy: "other-policy",
		storeCountable: true,
	},
	{
		name: "other domain", code: shoal.ErrorNotFound, where: "resolveActionBinding (descriptor domain)",
		domain: "other-domain", source: "source", policy: "policy",
		storeCountable: true,
	},
	{
		// In scope and in domain: refused by resolveActionBinding's second
		// AuthorizeObject, for OperationDelegate.
		// Caller-only, so it is told what it is: it holds no delegate grant.
		// Since #536 this check runs before the lookup, so the answer is the
		// same whether or not the agent exists — which is what makes an
		// honest message safe here.
		name: "delegated without delegate", code: shoal.ErrorUnauthorized,
		where:  "AuthorizeObject (delegate), before the lookup",
		domain: "domain", source: "source", policy: "policy",
		onBehalfOf: []shoal.ID{"delegator"},
	},
}

func (s stranger) bind(
	t *testing.T, f *attestationFixture, request string, operation auth.Operation,
) context.Context {
	t.Helper()
	slug := "stranger-" + strings.NewReplacer(" ", "-", ",", "").Replace(s.name)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(slug), Actor: shoal.ID(slug + "-actor"),
		ClientID: shoal.ID(slug + "-client"), OnBehalfOf: s.onBehalfOf,
		AuthorizationDomain:   []byte(s.domain),
		AllowedOperations:     []auth.Operation{operation},
		PermittedSourceIDs:    [][]byte{[]byte(s.source)},
		PermittedPolicyIDs:    [][]byte{[]byte(s.policy)},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             shoal.ID(request), CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return bindDecision(t, f.authority, decision)
}

// requireIndistinguishable is the comparison every case below makes.
func requireIndistinguishable(
	t *testing.T, s stranger, f *attestationFixture, route string,
	refused, absent error, callsBefore int,
	// wantCode is the route's answer, not the stranger's, because the
	// concealment is layered. resolveActionBinding answers a caller-only
	// failure honestly since #536 — it runs before the lookup, so there is
	// nothing to disclose. The claim and extension routes then normalize
	// *everything* to not-found at authorizedCurrent, because those routes
	// are about an existing action and must conceal its existence too. So the
	// same stranger is told different things on different routes, and that is
	// correct rather than inconsistent.
	wantCode shoal.ErrorCode,
) {
	t.Helper()
	if refused == nil || absent == nil {
		t.Fatalf("%s: %s was not refused: %v / %v", route, s.name, refused, absent)
	}
	if refused.Error() != absent.Error() {
		t.Fatalf("%s: an attestation-required action refuses %q (expected to "+
			"stop at %s) as %q while an absent one refuses it as %q: the gate "+
			"is an existence oracle", route, s.name, s.where, refused, absent)
	}
	if !shoal.IsErrorCode(refused, wantCode) {
		t.Fatalf("%s: %s refusal is %v, want code %q", route, s.name,
			refused, wantCode)
	}
	if s.storeCountable && f.attestations.callCount() != callsBefore {
		t.Fatalf("%s: the attestation store was read for %s", route, s.name)
	}
}

// requireNoDescriptorLookup asserts a refusal happened without the descriptor
// being read at all.
//
// This is what licenses a caller-only failure to answer honestly. The oracle
// in #536 was not the error code — it was that the check ran *after* the
// lookup, so reaching it implied the agent existed. A refusal that never
// touches the store cannot disclose anything about what is in it, whatever it
// says.
//
// Asserted separately from the byte comparison because the two can fail
// independently: a reordered check could still be reached after a lookup made
// for some other reason, and byte-identical refusals would hide that.
func requireNoDescriptorLookup(
	t *testing.T, f *attestationFixture, route string, before int,
) {
	t.Helper()
	if after := f.registryRows.getCount(); after != before {
		t.Fatalf("%s: the descriptor was looked up %d time(s) before the "+
			"refusal, so an honest caller-only error can still disclose "+
			"whether the agent exists", route, after-before)
	}
}

// TestAnExecuteHolderWithoutStandingCannotReachTheClaimGate is the attestation
// twin of TestAGateRefusalNeverPrecedesTheStandingCheck: callers holding
// execute but without standing on the descriptor are told exactly what an
// absent action ID is, and the store is never read.
func TestAnExecuteHolderWithoutStandingCannotReachTheClaimGate(t *testing.T) {
	for _, s := range strangers {
		t.Run(s.name, func(t *testing.T) {
			f := newAttestationFixture(t, true)
			ctx := s.bind(t, f, "stranger-request", auth.OperationExecute)
			probe := func(id []byte) error {
				_, err := f.service.Claim(ctx, ClaimRequest{
					ID: id, ExpectedVersion: f.queued.Version,
					ClaimID: []byte("probe-claim"), Lease: time.Minute,
					Context: dispatchContext(f.now(), "stranger-request"),
				})
				return err
			}
			refused, absent := probe(f.queued.ID), probe([]byte("no-such-action"))
			// Not-found for every stranger: authorizedCurrent normalizes
			// whatever resolveActionBinding returned, because this route is
			// about an existing action and conceals its existence too.
			requireIndistinguishable(
				t, s, f, "claim", refused, absent, 0, shoal.ErrorNotFound)

			// The control: the gate is armed. An unattested execute-holder
			// *with* standing (in scope, in domain, not the enqueuer) is told
			// the requirement, so the not-found above was standing.
			_, err := f.claim(t, "insider", time.Minute)
			requireAttestationRefusal(t, err)
		})
	}
}

// TestAnExecuteHolderWithoutStandingCannotReachTheExtensionGate: ExtendClaim
// is reachable by every principal holding execute and gates the extension on
// attestation, so it needs the same guarantee, here against a live attested
// claim.
func TestAnExecuteHolderWithoutStandingCannotReachTheExtensionGate(t *testing.T) {
	for _, s := range strangers {
		t.Run(s.name, func(t *testing.T) {
			f := newAttestationFixture(t, true)
			f.attest("alpha", f.now().Add(time.Hour))
			claimed, err := f.claim(t, "alpha", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			callsAfterClaim := f.attestations.callCount()
			ctx := s.bind(t, f, "stranger-request", auth.OperationExecute)
			probe := func(id []byte) error {
				_, err := f.service.ExtendClaim(ctx, ExtendRequest{
					ID: id, ExpectedVersion: claimed.Version,
					ClaimID: []byte("alpha-claim"), Lease: 2 * time.Minute,
					Context: dispatchContext(f.now(), "stranger-request"),
				})
				return err
			}
			refused, absent := probe(f.queued.ID), probe([]byte("no-such-action"))
			requireIndistinguishable(
				t, s, f, "extend", refused, absent, callsAfterClaim,
				shoal.ErrorNotFound)
		})
	}
}

// TestAnInvokeHolderWithoutStandingCannotReachTheAdmissionGate: admission is
// the other claim grant that reads attestation, under invoke. There is no
// action ID to guess, because the durable one is derived from the caller, so
// the absent probe names an agent that is not registered.
//
// The delegated variant is compared byte for byte too, since #536. It used to
// be excluded because it did not hold: queuedRecordBinding returned
// resolveActionBinding's error unnormalised, so a delegated caller without
// delegate authority was told unauthorized for a registered agent and
// not-found for an unregistered one. The caller-only checks now run *before*
// the descriptor lookup, so that answer is identical either way — and may
// therefore be honest about which grant is missing, which is what
// requireIndistinguishable's per-route wantCode expresses.
func TestAnInvokeHolderWithoutStandingCannotReachTheAdmissionGate(t *testing.T) {
	// Every stranger, including the delegated ones. This loop used to skip
	// those: a delegated caller holding invoke but lacking delegate authority
	// was told "unauthorized" for a registered agent and "not found" for an
	// unregistered one, because resolveActionBinding returned
	// AuthorizeObject's error un-normalized while concealing every other
	// standing refusal (#536). The skip is gone with the leak.
	for _, s := range strangers {
		t.Run(s.name, func(t *testing.T) {
			f := newAttestationFixture(t, true)
			ctx := s.bind(t, f, "admission-stranger", auth.OperationInvoke)
			// Captured after the fixture has built itself, which reads the
			// descriptor several times. Assuming zero here made the first
			// version of the assertion fail on the fixture's own setup
			// rather than on anything the probes did.
			lookupsBefore := f.registryRows.getCount()
			refused := admissionProbe(f, ctx, "agent")
			absent := admissionProbe(f, ctx, "no-such-agent")
			// The stranger's own code here: no action exists yet, so a
			// caller-only failure discloses nothing and says what it is,
			// while a scope or domain failure still answers as an absent
			// object would.
			requireIndistinguishable(
				t, s, f, "admission", refused, absent, 0, s.code)
			if s.code == shoal.ErrorUnauthorized {
				// The honest answer is only safe because nothing was read.
				requireNoDescriptorLookup(
					t, f, "admission", lookupsBefore)
			}
		})
	}
}

// TestAdmissionDelegateRefusalDoesNotDependOnTheRequirement: whatever the
// delegated stranger is told, it is told the same with the requirement on and
// off, it is never the attestation denial, and the store is never read.
//
// Since #536 it also compares against a nonexistent agent, which this could
// not do before: the answer used to differ by existence, so only the
// requirement-invariance half held. Four answers now — requirement on and
// off, agent present and absent — and all four must be identical. That
// subsumes the byte comparison without losing what this test uniquely
// asserts, which is that the attestation requirement plays no part in the
// refusal at all.
func TestAdmissionDelegateRefusalDoesNotDependOnTheRequirement(t *testing.T) {
	var delegated stranger
	for _, s := range strangers {
		if len(s.onBehalfOf) > 0 {
			delegated = s
		}
	}
	answers := make([]string, 0, 4)
	for _, require := range []bool{true, false} {
		f := newAttestationFixture(t, require)
		ctx := delegated.bind(t, f, "admission-stranger", auth.OperationInvoke)
		for _, agent := range []shoal.ID{"agent", "no-such-agent"} {
			err := admissionProbe(f, ctx, agent)
			if err == nil {
				t.Fatalf("require=%v agent=%q: a delegated caller without "+
					"delegate was admitted", require, agent)
			}
			if f.recorder.recordedOperation(
				ClaimRefusedAttestationPhase) != "" {
				t.Fatalf("require=%v agent=%q: the refusal was audited as "+
					"an attestation refusal", require, agent)
			}
			answers = append(answers, err.Error())
		}
	}
	for index := 1; index < len(answers); index++ {
		if answers[index] != answers[0] {
			t.Fatalf("the delegated answer varies across requirement and "+
				"agent existence: %q vs %q", answers[0], answers[index])
		}
	}
}

// admissionProbe requests an admission against agent and returns only the
// error, which is what these probes compare.
func admissionProbe(f *attestationFixture, ctx context.Context, agent shoal.ID) error {
	_, err := f.admission.Request(ctx, AdmissionRequest{
		ID: []byte("probe"), IdempotencyKey: []byte("key-probe"),
		TokenID: []byte("token-probe"), AgentID: agent, AgentGeneration: 1,
		Capability: "search", Action: "query",
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Effects: Effects{EffectMutatesExternal}, Input: json.RawMessage(`{"value":1}`),
		Lease: time.Minute, Context: dispatchContext(f.now(), "admission-stranger"),
	})
	return err
}

func TestAdmissionWithoutAttestationIsADurableDenial(t *testing.T) {
	f := newAttestationFixture(t, true)
	admit := func(id string) (AdmissionGrant, error) {
		request := "admission-" + id
		decision := dispatchDecisionFor(t, principal{
			subject: "gateway", actor: "gateway-actor", request: request, clientID: "gateway-client",
		}, auth.OperationInvoke)
		return f.admission.Request(bindDecision(t, f.authority, decision), AdmissionRequest{
			ID: []byte(id), IdempotencyKey: []byte("key-" + id), TokenID: []byte("token-" + id),
			AgentID: "agent", AgentGeneration: 1, Capability: "search", Action: "query",
			SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
			Effects: Effects{EffectMutatesExternal}, Input: json.RawMessage(`{"value":1}`),
			Lease: time.Minute, Context: dispatchContext(f.now(), request),
		})
	}
	grant, err := admit("one")
	if err != nil || grant.Outcome != AdmissionDenied {
		t.Fatalf("unattested admission = %+v, %v; want the existing durable denial", grant, err)
	}
	if got := f.recorder.recordedOperation("claim_refused_attestation"); got != auth.OperationInvoke {
		t.Fatalf("admission refusal audited under %q", got)
	}
	// Durable: attesting afterwards does not turn the same admission round.
	f.attestations.set("gateway", "gateway-client", "exattest:gw", f.now().Add(time.Hour))
	if grant, err := admit("one"); err != nil || grant.Outcome != AdmissionDenied {
		t.Fatalf("replayed denial = %+v, %v", grant, err)
	}
	granted, err := admit("two")
	if err != nil || grant.Outcome == AdmissionDenied && granted.Outcome == AdmissionDenied {
		t.Fatalf("attested admission = %+v, %v", granted, err)
	}
	if granted.Outcome != AdmissionAllowed {
		t.Fatalf("attested admission outcome = %v", granted.Outcome)
	}
	// A store failure is unavailable and writes nothing.
	f.attestations.err = errors.New("down")
	if _, err := admit("three"); !errors.Is(err, ErrAttestationUnavailable) {
		t.Fatalf("admission under a store failure = %v", err)
	}
	if _, err := f.store.GetAction(context.Background(), admissionActionIDFor(t, "three")); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("a store failure wrote an admission: %v", err)
	}
}

func admissionActionIDFor(t *testing.T, id string) []byte {
	t.Helper()
	decision := dispatchDecisionFor(t, principal{
		subject: "gateway", actor: "gateway-actor", request: "admission-" + id, clientID: "gateway-client",
	}, auth.OperationInvoke)
	return admissionActionID(decision, []byte(id))
}

func TestNeitherCompletionStatusNorCancelIsGated(t *testing.T) {
	f := newAttestationFixture(t, true)
	f.attest("alpha", f.now().Add(time.Hour))
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.attestations.err = errors.New("down")
	owner := bindDecision(t, f.authority, dispatchDecision(t,
		"owner", "actor", "status", auth.OperationDispatch, auth.OperationInvoke))
	if _, err := f.service.Status(owner, StatusRequest{
		ID: f.queued.ID, Context: dispatchContext(f.now(), "status"),
	}); err != nil {
		t.Fatalf("status was gated: %v", err)
	}
	ctx, request := f.worker(t, "alpha")
	if _, err := f.service.CompleteClaim(ctx, CompletionRequest{
		ID: f.queued.ID, ExpectedVersion: claimed.Version, ClaimID: []byte("alpha-claim"),
		ClaimFence: claimed.ClaimFence, Result: ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: request,
	}); err != nil {
		t.Fatalf("completion was gated: %v", err)
	}
	if f.attestations.callCount() != 1 {
		t.Fatalf("store consulted %d times, want once (the claim)", f.attestations.callCount())
	}
}

func TestAnActionWithoutTheRequirementNeverReadsTheStore(t *testing.T) {
	f := newAttestationFixture(t, false)
	f.attestations.err = errors.New("down")
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimAttestationID != "" || f.attestations.callCount() != 0 {
		t.Fatalf("an unattested action consulted the store or recorded an attestation")
	}
}

func TestPolicyFlipGatesNewClaims(t *testing.T) {
	f := newAttestationFixture(t, false)
	// Registering the requirement is a new generation; a record queued at
	// the old one stops resolving (the stranding #486 is about), and new work
	// at the new generation is gated.
	stored := f.registryRows.records["agent"]
	stored.Descriptor.Generation = 2
	stored.Descriptor.Capabilities[0].Actions[0].RequiresAttestation = true
	f.registryRows.records["agent"] = stored
	_, err := f.claim(t, "alpha", time.Minute)
	requireFlipRefusal(t, err)
	enqueuer := bindDecision(t, f.authority, dispatchDecision(t,
		"owner", "actor", "request-2", auth.OperationDispatch, auth.OperationInvoke))
	request := dispatchEnqueue(f.now(), "request-2")
	request.ID, request.IdempotencyKey, request.AgentGeneration = []byte("action-2"), []byte("idem-2"), 2
	queued, err := f.service.Enqueue(enqueuer, request)
	if err != nil {
		t.Fatal(err)
	}
	f.queued = queued
	_, err = f.claim(t, "alpha", time.Minute)
	requireAttestationRefusal(t, err)
	f.attest("alpha", f.now().Add(time.Hour))
	if _, err := f.claim(t, "alpha", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestClaimAttestationIDBounds(t *testing.T) {
	record := approvalTestRecord(t)
	record.ClaimAttestationID = "exattest:x"
	if err := record.Validate(); err == nil {
		t.Fatal("a queued record carried a claim attestation")
	}
	f := newAttestationFixture(t, true)
	f.attestations.set("alpha", "alpha-client", strings.Repeat("a", MaxClaimAttestationIDBytes+1), f.now().Add(time.Hour))
	_, err := f.claim(t, "alpha", time.Minute)
	if !errors.Is(err, ErrAttestationUnavailable) {
		t.Fatalf("an oversized attestation ID = %v, want refused at the boundary", err)
	}
	holder := ClaimHolder{Subject: "s", Actor: "a", ClaimID: []byte("c"), ClaimFence: 1,
		HeldAt: time.Now().UTC(), AttestationID: shoal.ID(strings.Repeat("a", MaxClaimAttestationIDBytes+1))}
	if err := holder.validate(); err == nil {
		t.Fatal("an oversized holder attestation validated")
	}
}
