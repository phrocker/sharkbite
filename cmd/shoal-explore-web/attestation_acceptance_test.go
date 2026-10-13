// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	"github.com/phrocker/shoal-oss/internal/executorattest"
	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/sdk"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Executor attestation required by policy (#446 slice 3), against the
// composition openService builds for the shipped binary: the durable
// dispatch store, the real action and attestation recorders, the real
// executorattest verifier and its engine-backed store. Nothing on the server
// side is a double.
//
// The claimant is the enqueuer, claiming under invoke. A foreign claimant
// needs OperationExecute on the claim path, which #480 still blocks in this
// composition (the action recorder and the event publisher refuse it). The
// presenter holds execute — presentation does not touch either of those —
// and is the same (domain, subject, client) the claimant authenticates as,
// which is what an attestation is keyed by.

const attestationImage = "sha256:" + "ab" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

var (
	attestedWorker = principal{
		subject: "alice", actor: "alice-gateway", client: "gateway-client",
		operations: []auth.Operation{auth.OperationDispatch, auth.OperationInvoke},
	}
	// Presenters are bound to the executor they attest for (#391).
	attestedPresenter = principal{
		subject: "alice", actor: "alice-gateway", client: "gateway-client",
		operations: []auth.Operation{auth.OperationExecute}, binding: "local",
	}
	foreignPresenter = principal{
		subject: "mallory", actor: "mallory-gateway", client: "mallory-client",
		operations: []auth.Operation{auth.OperationExecute}, binding: "local",
	}
)

type attestationHarness struct {
	*approvalHarness
	verifier ed25519.PrivateKey
}

func newAttestationHarness(t *testing.T) *attestationHarness {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustFile := filepath.Join(t.TempDir(), "trust.json")
	body, _ := json.Marshal(map[string]any{"executors": map[string]any{
		"local": map[string]any{
			"verifiers": []map[string]any{{
				"id": "operator-key:1", "public_key": base64.StdEncoding.EncodeToString(public),
				"max_validity": "1h", "clock_skew": "1m",
			}},
			"image_digests": []string{attestationImage},
		},
	}})
	if err := os.WriteFile(trustFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	external := externalFleetEffectBindings{mutating: []string{"local"}}
	// The flag refuses a trust root for a ref not bound for external work.
	if _, err := loadExecutorAttestationTrust(trustFile, externalFleetEffectBindings{}); err == nil {
		t.Fatal("a trust root for an unbound ref was accepted")
	}
	trust, err := loadExecutorAttestationTrust(trustFile, external)
	if err != nil {
		t.Fatal(err)
	}

	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	h.authority, err = auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	executors, err := newConfiguredFleetExecutors([]string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bindExternalFleetEffects(executors, external); err != nil {
		t.Fatal(err)
	}
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(h.root, "corpus"),
		policyDir: filepath.Join(h.root, "policy"),
		resolver:  h.authority.Resolver(), clock: h.now,
		executors: executors, generationReader: h.reader,
		executorAttestation: trust,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened.attestation == nil {
		opened.close()
		t.Fatal("a configured trust file did not compose the presentation route")
	}
	h.opened, h.isOpen = opened, true
	t.Cleanup(h.close)
	external1 := fleet.Effects{fleet.EffectMutatesExternal}
	if _, err := h.opened.fleetRegistry.Register(h.as(registrant), fleet.RegisterRequest{
		Context: h.context(h.now().Add(time.Minute)), RegistrationKey: "attested-registration",
		Spec: fleet.Spec{
			ID: "gateway", AuthorizationDomain: workspaceAuthorizationDomain,
			Scopes:      []fleet.Scope{{SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID}},
			ExecutorRef: "local",
			Capabilities: []fleet.Capability{{Name: "ops", Actions: []fleet.Action{{
				Name: "deploy", InputSchema: json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"object"}`), Effects: external1,
				RequiresAttestation: true,
			}, {
				Name: "status", InputSchema: json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"object"}`), Effects: external1,
			}}}},
			LeaseExpiresAt: h.now().Add(20 * time.Hour),
		},
	}); err != nil {
		t.Fatalf("register an attested descriptor: %v", err)
	}
	return &attestationHarness{approvalHarness: h, verifier: private}
}

// statement signs a statement for who, bound to key, valid from issued for
// validity.
func (h *attestationHarness) statement(who principal, key string, issued time.Time, validity time.Duration) []byte {
	h.t.Helper()
	want := executorattest.Expectation{
		Principal: executorattest.Principal{
			Domain: workspaceAuthorizationDomain, Subject: who.subject, ClientID: who.client,
		},
		ExecutorRef: "local", Key: []byte(key), Now: issued,
	}
	evidence, err := executorattest.SignStatement("operator-key:1", h.verifier,
		executorattest.StatementFor(want, attestationImage, issued, issued.Add(validity)))
	if err != nil {
		h.t.Fatal(err)
	}
	return evidence
}

func (h *attestationHarness) present(who principal, key string, evidence []byte) (fleet.AttestationReceipt, error) {
	return h.opened.attestation.Present(h.as(who), fleet.AttestationPresentation{
		ExecutorRef: "local", IdempotencyKey: []byte(key), Report: evidence,
	})
}

func (h *attestationHarness) enqueueDeploy(id string) fleet.ActionRecord {
	h.t.Helper()
	queued, err := h.opened.fleetDispatch.Enqueue(h.as(attestedWorker), fleet.EnqueueRequest{
		ID: []byte(id), IdempotencyKey: []byte("key-" + id),
		AgentID: "gateway", AgentGeneration: 1, Capability: "ops", Action: "deploy",
		SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID, ObjectID: "release-7",
		Input: json.RawMessage(`{"version":"7"}`), Context: h.context(h.now().Add(time.Hour)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return queued
}

func (h *attestationHarness) claim(id []byte, version uint64, claimID string, lease time.Duration) (fleet.ActionRecord, error) {
	return h.opened.fleetDispatch.Claim(h.as(attestedWorker), fleet.ClaimRequest{
		ID: id, ExpectedVersion: version, ClaimID: []byte(claimID), Lease: lease,
		Context: h.context(h.now().Add(time.Minute)),
	})
}

func (h *attestationHarness) status(id []byte) fleet.ActionRecord {
	h.t.Helper()
	record, err := h.opened.fleetDispatch.Status(h.as(attestedWorker), fleet.StatusRequest{
		ID: id, Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return record
}

// TestAttestationGateAgainstTheDurableStore drives Claim, ExtendClaim (the
// clamped end) and the record's attestation through the engine-backed
// dispatch store, with the real verifier.
func TestAttestationGateAgainstTheDurableStore(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	queued := h.enqueueDeploy("deploy-1")

	// Unattested: the specific sentinel, nothing written.
	_, err := h.claim(queued.ID, queued.Version, "claim-1", time.Minute)
	if !errors.Is(err, fleet.ErrAttestationRequired) || !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("unattested claim = %v", err)
	}
	if record := h.status(queued.ID); record.Version != queued.Version || record.State != fleet.DispatchQueued {
		t.Fatalf("a refused claim wrote: %+v", record)
	}

	// Attest for two minutes; a five-minute lease is not covered, a
	// one-minute lease is.
	receipt, err := h.present(attestedPresenter, "present-1", h.statement(attestedPresenter, "present-1", h.now(), 2*time.Minute))
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	_, err = h.claim(queued.ID, queued.Version, "claim-1", 5*time.Minute)
	if !errors.Is(err, fleet.ErrAttestationRequired) {
		t.Fatalf("an uncovered lease = %v", err)
	}
	claimed, err := h.claim(queued.ID, queued.Version, "claim-1", time.Minute)
	if err != nil {
		t.Fatalf("a covered claim: %v", err)
	}
	if claimed.ClaimAttestationID != receipt.AttestationID {
		t.Fatalf("ClaimAttestationID = %q, want %q", claimed.ClaimAttestationID, receipt.AttestationID)
	}
	if h.status(queued.ID).ClaimAttestationID != receipt.AttestationID {
		t.Fatal("the durable record lost the claim attestation")
	}

	// Extension past expiry: refused with the sentinel, the claim survives.
	extend := func(version uint64, lease time.Duration) (fleet.ActionRecord, error) {
		return h.opened.fleetDispatch.ExtendClaim(h.as(attestedWorker), fleet.ExtendRequest{
			ID: queued.ID, ExpectedVersion: version, ClaimID: []byte("claim-1"),
			Lease: lease, Context: h.context(h.now().Add(time.Minute)),
		})
	}
	h.advance(30 * time.Second)
	if _, err := extend(claimed.Version, 5*time.Minute); !errors.Is(err, fleet.ErrAttestationRequired) {
		t.Fatalf("extension past expiry = %v", err)
	}
	if record := h.status(queued.ID); record.Version != claimed.Version || record.State != fleet.DispatchClaimed {
		t.Fatalf("a refused extension changed the record: %+v", record)
	}
	// Re-attest (a strictly later statement) and extend.
	second, err := h.present(attestedPresenter, "present-2", h.statement(attestedPresenter, "present-2", h.now(), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	extended, err := extend(claimed.Version, 5*time.Minute)
	if err != nil {
		t.Fatalf("extension under a fresh attestation: %v", err)
	}
	if extended.ClaimAttestationID != second.AttestationID {
		t.Fatalf("extension recorded %q, want %q", extended.ClaimAttestationID, second.AttestationID)
	}
}

func TestAttestationExtensionUsesTheClampedEndAgainstTheDurableStore(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	queued := h.enqueueDeploy("deploy-clamp")
	// The action's deadline is an hour out. Move to three minutes before it
	// and attest to exactly the deadline.
	h.advance(time.Hour - 3*time.Minute)
	validity := queued.Deadline.Sub(h.now())
	if _, err := h.present(attestedPresenter, "clamp", h.statement(attestedPresenter, "clamp", h.now(), validity)); err != nil {
		t.Fatal(err)
	}
	claimed, err := h.claim(queued.ID, queued.Version, "claim-clamp", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	extended, err := h.opened.fleetDispatch.ExtendClaim(h.as(attestedWorker), fleet.ExtendRequest{
		ID: queued.ID, ExpectedVersion: claimed.Version, ClaimID: []byte("claim-clamp"),
		Lease: 5 * time.Minute, Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatalf("a clamped extension the attestation covers: %v", err)
	}
	if !extended.ClaimLeaseUntil.Equal(queued.Deadline) {
		t.Fatalf("lease end %s, want the deadline %s", extended.ClaimLeaseUntil, queued.Deadline)
	}
}

// TestALapsedAttestedClaimAgainstTheDurableStore: the claim lapses with its
// attestation; a re-attested claim takes the record; the old claim's
// completion is refused and its ambiguity report accepted; one completion.
func TestALapsedAttestedClaimAgainstTheDurableStore(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	queued := h.enqueueDeploy("deploy-lapse")
	first, err := h.present(attestedPresenter, "lapse-1", h.statement(attestedPresenter, "lapse-1", h.now(), 90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	old, err := h.claim(queued.ID, queued.Version, "claim-old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Minute)
	// The lease and the attestation have both lapsed: no re-claim until
	// re-attested.
	if _, err := h.claim(queued.ID, old.Version, "claim-new", time.Minute); !errors.Is(err, fleet.ErrAttestationRequired) {
		t.Fatalf("re-claim on a lapsed attestation = %v", err)
	}
	second, err := h.present(attestedPresenter, "lapse-2", h.statement(attestedPresenter, "lapse-2", h.now(), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	taken, err := h.claim(queued.ID, old.Version, "claim-new", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if taken.ClaimAttestationID != second.AttestationID ||
		taken.ClaimHistory[len(taken.ClaimHistory)-1].AttestationID != first.AttestationID {
		t.Fatalf("the attestation did not move with the claim: %q %+v", taken.ClaimAttestationID, taken.ClaimHistory)
	}
	_, err = h.opened.fleetDispatch.CompleteClaim(h.as(attestedWorker), fleet.CompletionRequest{
		ID: queued.ID, ExpectedVersion: old.Version, ClaimID: []byte("claim-old"),
		ClaimFence: old.ClaimFence, Result: fleet.ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: h.context(h.now().Add(time.Minute)),
	})
	if err == nil {
		t.Fatal("the lapsed claim completed")
	}
	if _, err := h.opened.fleetDispatch.ReportAmbiguity(h.as(attestedWorker), fleet.AmbiguityRequest{
		ID: queued.ID, ClaimFence: old.ClaimFence, Outcome: fleet.AmbiguityOutcomeUnknown,
		Context: h.context(h.now().Add(time.Minute)),
	}); err != nil {
		t.Fatalf("the lapsed claim's ambiguity report: %v", err)
	}
	current := h.status(queued.ID)
	completed, err := h.opened.fleetDispatch.CompleteClaim(h.as(attestedWorker), fleet.CompletionRequest{
		ID: queued.ID, ExpectedVersion: current.Version, ClaimID: []byte("claim-new"),
		ClaimFence: taken.ClaimFence, Result: fleet.ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil || completed.State != fleet.DispatchSucceeded {
		t.Fatalf("complete = %v, %v", completed.State, err)
	}
	if final := h.status(queued.ID); final.State != fleet.DispatchSucceeded || len(final.AmbiguityReports) != 1 {
		t.Fatalf("final = %v, %d reports", final.State, len(final.AmbiguityReports))
	}
}

// TestAttestationPresentationVerifiesAgainstTheRealStore covers replay,
// another principal's statement, the server clock and rollback.
func TestAttestationPresentationVerifiesAgainstTheRealStore(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	evidence := h.statement(attestedPresenter, "replay", h.now(), time.Hour)
	first, err := h.present(attestedPresenter, "replay", evidence)
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent replay.
	again, err := h.present(attestedPresenter, "replay", evidence)
	if err != nil || again.AttestationID != first.AttestationID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	refused := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, fleet.ErrAttestationRefused) || err.Error() != "unauthorized: attestation refused" {
			t.Fatalf("%s = %v, want the opaque refusal", name, err)
		}
	}
	// The same statement under another key: nonce mismatch.
	_, err = h.present(attestedPresenter, "other-key", evidence)
	refused("replay under another key", err)
	// Another principal presenting alice's statement, and alice's under its
	// own key: the row key is the caller, never the statement.
	_, err = h.present(foreignPresenter, "replay", evidence)
	refused("another principal", err)
	// An earlier statement after a later one: rollback.
	_, err = h.present(attestedPresenter, "older", h.statement(attestedPresenter, "older", h.now().Add(-time.Second), time.Hour))
	refused("rollback", err)
	// The server clock decides. A statement issued 30s ahead (within the
	// verifier's skew) is accepted; one issued 2m ahead is refused; one that
	// expired by the server clock is refused however the signer saw it.
	if _, err := h.present(attestedPresenter, "skew", h.statement(attestedPresenter, "skew", h.now().Add(30*time.Second), time.Hour)); err != nil {
		t.Fatalf("within skew: %v", err)
	}
	_, err = h.present(attestedPresenter, "ahead", h.statement(attestedPresenter, "ahead", h.now().Add(2*time.Minute), time.Hour))
	refused("issued beyond skew", err)
	_, err = h.present(attestedPresenter, "stale", h.statement(attestedPresenter, "stale", h.now().Add(-2*time.Hour), time.Hour))
	refused("expired by the server clock", err)
	// A caller without execute is refused by authorization, before any
	// verification.
	_, err = h.present(attestedWorker, "no-execute", h.statement(attestedWorker, "no-execute", h.now(), time.Hour))
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) || errors.Is(err, fleet.ErrAttestationRefused) {
		t.Fatalf("no execute = %v", err)
	}
	// mallory's own attestation does not cover alice's claim.
	if _, err := h.present(foreignPresenter, "mallory", h.statement(foreignPresenter, "mallory", h.now().Add(time.Minute), time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestAttestationClaimDisclosesNothingToACallerWithoutStanding(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	attested := h.enqueueDeploy("deploy-hidden")
	plain, err := h.opened.fleetDispatch.Enqueue(h.as(attestedWorker), fleet.EnqueueRequest{
		ID: []byte("status-hidden"), IdempotencyKey: []byte("key-status-hidden"),
		AgentID: "gateway", AgentGeneration: 1, Capability: "ops", Action: "status",
		SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID, ObjectID: "release-7",
		Input: json.RawMessage(`{}`), Context: h.context(h.now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	outsider := principal{subject: "eve", actor: "eve-agent", client: "eve-client",
		operations: []auth.Operation{auth.OperationDispatch, auth.OperationInvoke}}
	answer := func(record fleet.ActionRecord) string {
		_, err := h.opened.fleetDispatch.Claim(h.as(outsider), fleet.ClaimRequest{
			ID: record.ID, ExpectedVersion: record.Version, ClaimID: []byte("x"),
			Lease: time.Minute, Context: h.context(h.now().Add(time.Minute)),
		})
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("outsider = %v", err)
		}
		return err.Error()
	}
	if answer(attested) != answer(plain) {
		t.Fatal("a caller without standing can tell an attested action apart")
	}
}

// newAttestationPlane serves the fleet and admission routes through the
// binary's authenticated transport: webapi.NewAuthenticatedHandler with the
// workspace binder, mounted at the same prefixes main.go mounts. Nothing binds
// a decision onto the request context directly.
//
// The authenticator is the one seam: no shipped authenticator can mint the
// presenter, because none grants OperationExecute until #480, and the
// development principal has no client ID (so it can never be attested). This
// one maps a bearer token to a principal and mints like the shipped ones do,
// with correlationIDFor (#527) supplying the correlation ID — honouring
// Shoal-Correlation-ID when sent, generating one otherwise. The development
// authenticator itself is driven in
// TestTheShippedAuthenticatorReachesTheAttestationGate.
func newAttestationPlane(t *testing.T, h *attestationHarness, tokens map[string]principal) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	authenticator := webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
		who, ok := tokens[strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			return auth.Decision{}, shoal.NewError(shoal.ErrorUnauthorized, "unknown test token")
		}
		requestID, err := newRequestID()
		if err != nil {
			return auth.Decision{}, err
		}
		correlationID, err := correlationIDFor(request, "test-correlation-")
		if err != nil {
			return auth.Decision{}, err
		}
		decision, err := auth.NewDecision(who.bindExecutor(auth.DecisionConfig{
			Subject: who.subject, Actor: who.actor, ClientID: who.client,
			OnBehalfOf: who.onBehalfOf, AuthorizationDomain: workspaceAuthorizationDomain,
			AllowedOperations:     who.operations,
			PermittedSourceIDs:    [][]byte{workspaceSourceID},
			PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
			PolicyGeneration:      workspacePolicyGeneration,
			AuthenticationExpires: h.now().Add(time.Hour),
			RequestID:             requestID, CorrelationID: correlationID,
		}))
		if err == nil && decision.CorrelationID() == "" {
			t.Error("the test mint produced no correlation ID")
		}
		return decision, err
	})
	mountAttestationPlane(t, h, server, authenticator)
	return server
}

// mountAttestationPlane mounts the routes behind authenticator, exactly as
// main.go does, and starts server.
func mountAttestationPlane(t *testing.T, h *attestationHarness, server *httptest.Server, authenticator webapi.Authenticator) {
	t.Helper()
	handler, err := webapi.NewAuthenticatedHandler(h.opened.service, authenticator,
		h.authority.Binder(), server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fleetHandler, err := webapi.NewFleetHandlerWithAttestation(
		h.opened.fleetRegistry, h.opened.fleetDispatch, h.opened.attestation)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(webapi.FleetRoutePrefix, fleetHandler); err != nil {
		t.Fatal(err)
	}
	admissionHandler, err := webapi.NewAdmissionHandler(h.opened.admission)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(webapi.AdmissionRoutePrefix, admissionHandler); err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
}

// TestSDKAttestationEffectsGatewayClaimAndComplete is the real composition:
// sdk.Attestation() presents, the effects gateway's dispatch client claims
// and completes, all over HTTP against the handlers the binary mounts.
func TestSDKAttestationEffectsGatewayClaimAndComplete(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	server := newAttestationPlane(t, h, map[string]principal{
		"presenter": attestedPresenter, "worker": attestedWorker,
	})
	client, err := sdk.New(sdk.Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Token: func(context.Context) (string, error) { return "presenter", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(server.URL)
	gateway, err := effectsgateway.NewDispatchClient(base, server.Client(),
		func() (string, error) { return "worker", nil }, h.now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	queued := h.enqueueDeploy("deploy-sdk")
	gatewayContext := func(reason string) effectsgateway.RequestContext {
		id, err := effectsgateway.NewRequestID(nil)
		if err != nil {
			t.Fatal(err)
		}
		// Every request about the action carries its record's correlation.
		return effectsgateway.RequestContext{RequestID: id, ReasonCode: reason, Deadline: h.now().Add(30 * time.Second),
			CorrelationID: []byte(queued.CorrelationID)}
	}
	claimID, _, err := effectsgateway.NewClaimID("pod-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Before attesting: a 409, and the body names nothing.
	_, err = gateway.Claim(ctx, queued.ID, effectsgateway.ClaimRequest{
		Context: gatewayContext("claim"), ExpectedVersion: queued.Version, ClaimID: claimID, Lease: time.Minute,
	})
	var dispatchErr *effectsgateway.DispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Status != http.StatusConflict ||
		effectsgateway.DispatchKind(err) != effectsgateway.DispatchConflict {
		t.Fatalf("unattested claim over HTTP = %v", err)
	}

	// Refused presentations are one opaque answer.
	if _, err := client.Attestation().Present(ctx, "local", []byte("k1"),
		h.statement(attestedPresenter, "k0", h.now(), time.Hour)); !errors.Is(err, attestationapi.ErrRefused) {
		t.Fatalf("a statement for another key = %v", err)
	}
	receipt, err := client.Attestation().Present(ctx, "local", []byte("k1"),
		h.statement(attestedPresenter, "k1", h.now(), time.Hour))
	if err != nil {
		t.Fatalf("sdk present: %v", err)
	}

	claimed, err := gateway.Claim(ctx, queued.ID, effectsgateway.ClaimRequest{
		Context: gatewayContext("claim"), ExpectedVersion: queued.Version, ClaimID: claimID, Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("attested claim over HTTP: %v", err)
	}
	if h.status(queued.ID).ClaimAttestationID != shoal.ID(receipt.AttestationID) {
		t.Fatal("the claim does not name the presented attestation")
	}
	completed, err := gateway.Complete(ctx, claimed.ID, effectsgateway.Completion{
		Context: gatewayContext("complete"), ExpectedVersion: claimed.Version, ClaimFence: claimed.ClaimFence,
		ClaimID: claimID, Output: json.RawMessage(`{"ok":true}`),
	})
	if err != nil || completed.State != dispatchapi.DispatchSucceeded {
		t.Fatalf("complete over HTTP = %+v, %v", completed.State, err)
	}
}

// TestTheGatewayPresentsItsAttestationFromFiles: the dispatch client's
// PresentAttestation, built on sdk.Attestation(), presents the statement and
// key files for its bound ref over the real route, returns the receipt's
// expires_at, and re-reads both files on every call — a rotated statement is
// picked up by the next presentation. The executor-bound presenter is the
// claimant's principal, so the claim it unlocks succeeds.
func TestTheGatewayPresentsItsAttestationFromFiles(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	server := newAttestationPlane(t, h, map[string]principal{
		"presenter": attestedPresenter, "worker": attestedWorker,
	})
	base, _ := url.Parse(server.URL)
	unbound, err := effectsgateway.NewDispatchClient(base, server.Client(),
		func() (string, error) { return "presenter", nil }, h.now)
	if err != nil {
		t.Fatal(err)
	}
	presenter, err := unbound.BindExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	statementFile, keyFile := filepath.Join(dir, "statement"), filepath.Join(dir, "key")
	write := func(statement []byte, key string) {
		t.Helper()
		if err := os.WriteFile(statementFile, statement, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A statement bound to another key: the one opaque refusal.
	write(h.statement(attestedPresenter, "k0", h.now(), time.Hour), "k1")
	if _, err := presenter.PresentAttestation(ctx, statementFile, keyFile); effectsgateway.DispatchKind(err) !=
		effectsgateway.DispatchAttestationRefused {
		t.Fatalf("a statement for another key = %v", err)
	}

	issued := h.now()
	write(h.statement(attestedPresenter, "k1", issued, 30*time.Minute), "k1")
	expires, err := presenter.PresentAttestation(ctx, statementFile, keyFile)
	if err != nil || !expires.Equal(issued.Add(30*time.Minute)) {
		t.Fatalf("present = %v, %v; want %v", expires, err, issued.Add(30*time.Minute))
	}
	// Idempotent: the same files again are the same receipt.
	if again, err := presenter.PresentAttestation(ctx, statementFile, keyFile); err != nil || !again.Equal(expires) {
		t.Fatalf("re-present = %v, %v", again, err)
	}

	// Rotated by the operator's signer: the next call reads the new files.
	h.advance(time.Minute)
	rotated := h.now()
	write(h.statement(attestedPresenter, "k2", rotated, time.Hour), "k2")
	renewed, err := presenter.PresentAttestation(ctx, statementFile, keyFile)
	if err != nil || !renewed.Equal(rotated.Add(time.Hour)) {
		t.Fatalf("rotated present = %v, %v; want %v", renewed, err, rotated.Add(time.Hour))
	}

	// The attestation unlocks the claim, which never outlives it.
	worker, _ := effectsgateway.NewDispatchClient(base, server.Client(),
		func() (string, error) { return "worker", nil }, h.now)
	queued := h.enqueueDeploy("deploy-files")
	id, _ := effectsgateway.NewRequestID(nil)
	claimID, _, _ := effectsgateway.NewClaimID("pod-0", nil)
	claimed, err := worker.Claim(ctx, queued.ID, effectsgateway.ClaimRequest{
		Context: effectsgateway.RequestContext{RequestID: id, ReasonCode: "claim",
			Deadline: h.now().Add(30 * time.Second), CorrelationID: []byte(queued.CorrelationID)},
		ExpectedVersion: queued.Version, ClaimID: claimID, Lease: time.Minute,
	})
	if err != nil || claimed.ClaimLeaseUntil.After(renewed) {
		t.Fatalf("attested claim = %+v, %v", claimed, err)
	}
}

// TestAdmissionPathBCannotBypassAttestation: the born-claimed grant goes
// through the same gate; unattested, it is the existing durable denial.
func TestAdmissionPathBCannotBypassAttestation(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	server := newAttestationPlane(t, h, map[string]principal{"worker": attestedWorker})
	client, err := sdk.New(sdk.Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Token: func(context.Context) (string, error) { return "worker", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(id string) admissionapi.Request {
		return admissionapi.Request{
			Context: admissionapi.RequestContext{
				RequestID: admissionapi.EncodeID([]byte("body-request")), ReasonCode: "test",
				Deadline: h.now().Add(time.Hour),
			},
			ID: admissionapi.EncodeID([]byte(id)), IdempotencyKey: admissionapi.EncodeID([]byte("key-" + id)),
			TokenID: admissionapi.EncodeID([]byte("token-" + id)), AgentID: admissionapi.EncodeID([]byte("gateway")),
			AgentGeneration: 1, Capability: "ops", Action: "deploy",
			SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			ObjectID: admissionapi.EncodeID([]byte("release-7")),
			Effects:  []string{"external"}, Input: json.RawMessage(`{"version":"7"}`), Lease: time.Minute,
		}
	}
	options := admissionapi.RequestOptions{Clock: h.now}
	if _, err := client.Admission().Request(context.Background(), request("unattested"), options); !errors.Is(err, admissionapi.ErrDenied) {
		t.Fatalf("unattested admission = %v, want the durable denial", err)
	}
	if _, err := h.present(attestedPresenter, "admit", h.statement(attestedPresenter, "admit", h.now(), time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Durable: the same admission still answers denied.
	if _, err := client.Admission().Request(context.Background(), request("unattested"), options); !errors.Is(err, admissionapi.ErrDenied) {
		t.Fatalf("replayed denial = %v", err)
	}
	if _, err := client.Admission().Request(context.Background(), request("attested"), options); err != nil {
		t.Fatalf("attested admission = %v", err)
	}
}
