// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/executorattest"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The acceptance tests for #451 run against the composition openService
// builds for the shipped binary: the real dispatch store over the embedded
// runtime, ActionRecorderWithSnapshots over the interaction recorder over the
// authorized client, ComposeWithPublisherAndReader with the real
// InteractionAuditor, and the real ApprovalRecorder and ApprovalStore. No
// recorder or publisher double appears anywhere in this file. Every defect
// #480 lists hid behind a permissive double, and an approval test that passed
// over one would prove nothing about the authorization it exists to test.

var approvalSecondPolicyID = []byte("shoal-explore-web/workspace-second")

type approvalHarness struct {
	t         *testing.T
	root      string
	clock     atomic.Int64
	authority *auth.Authority
	reader    *mutableFleetGeneration
	opened    openedService
	isOpen    bool
	requests  atomic.Int64
	wrap      func(fleet.ApprovalStore) fleet.ApprovalStore
	// wrapEvents wraps the approval event publisher (#653); nil in every
	// test that does not lose a publication on purpose.
	wrapEvents func(fleet.ApprovalEventPublisher) fleet.ApprovalEventPublisher
	// mapping is the approver mapping digest the service is opened with;
	// nil means none, as in the shipped binary without the mapping file.
	mapping func(context.Context) (auth.Digest, error)
	// scheme is the OIDC identity scheme the service is opened under
	// (#526); nil means none, as for a non-OIDC authenticator.
	scheme *identitySchemeConfig
	// recorded is the scheme of the last successful open: what the
	// coordination store's row holds.
	recorded *identitySchemeConfig
	// executors is the configured executor references; nil means "local"
	// alone.
	executors configuredFleetExecutors
	// attestation is the executor attestation trust the service is opened
	// with (-fleet-executor-attestation); nil means none.
	attestation *executorattest.Trust
	// withoutTerminalStates opens the service as the shipped default does,
	// without -approval-terminal-states (#655). Every other test enables
	// them, since they exercise the states.
	withoutTerminalStates bool
}

func newApprovalHarness(t *testing.T) *approvalHarness {
	t.Helper()
	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")
	return h
}

func (h *approvalHarness) now() time.Time {
	return time.Unix(0, h.clock.Load()).UTC()
}

func (h *approvalHarness) advance(by time.Duration) {
	h.clock.Add(int64(by))
}

func (h *approvalHarness) open() {
	h.t.Helper()
	if err := h.tryOpen(); err != nil {
		h.t.Fatal(err)
	}
}

// tryOpen is open that returns the startup refusal instead of failing.
func (h *approvalHarness) tryOpen() error {
	h.t.Helper()
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(h.root, "corpus"),
		policyDir: filepath.Join(h.root, "policy"),
		resolver:  h.authority.Resolver(), clock: h.now,
		// The authority's binder, as run passes it, so the approval sweep's
		// system observer (#655) is composed as in the shipped binary.
		binder:              h.authority.Binder(),
		terminalStates:      !h.withoutTerminalStates,
		executors:           h.configuredExecutors(),
		generationReader:    h.reader,
		wrapApprovalStore:   h.wrap,
		wrapApprovalEvents:  h.wrapEvents,
		approverMapping:     h.mapping,
		identityScheme:      h.scheme,
		executorAttestation: h.attestation,
	})
	if err != nil {
		return err
	}
	if opened.approvals == nil || opened.fleetDispatch == nil ||
		opened.admission == nil {
		opened.close()
		h.t.Fatal("embedded service did not compose approvals")
	}
	h.opened, h.isOpen = opened, true
	h.recorded = h.scheme
	return nil
}

// configuredExecutors is h.executors, or "local" alone.
func (h *approvalHarness) configuredExecutors() configuredFleetExecutors {
	if h.executors != nil {
		return h.executors
	}
	return configuredFleetExecutors{
		"local": configuredFleetExecutor{reference: "local"},
	}
}

func (h *approvalHarness) close() {
	if h.isOpen {
		h.opened.close()
		h.isOpen = false
	}
}

// reopen is a process restart: everything in memory is discarded and the
// service is rebuilt over the same durable directories.
func (h *approvalHarness) reopen() {
	h.t.Helper()
	h.close()
	h.open()
}

type principal struct {
	subject    shoal.ID
	actor      shoal.ID
	client     shoal.ID
	onBehalfOf []shoal.ID
	operations []auth.Operation
	generation int64
	// binding makes the principal an executor-bound worker (#391): the
	// action-execution role, bound to this ref.
	binding string
}

// bindExecutor applies a principal's executor binding to a decision config.
func (who principal) bindExecutor(config auth.DecisionConfig) auth.DecisionConfig {
	if who.binding != "" {
		config.ServiceRole = auth.ServiceRoleActionExecution
		config.ServiceCeilingIdentity = "executor-ceiling"
		config.ExecutorBinding = who.binding
	}
	return config
}

var (
	requester = principal{
		subject: "alice", actor: "alice-agent",
		operations: []auth.Operation{auth.OperationDispatch, auth.OperationInvoke},
	}
	approver = principal{
		subject: "bob", actor: "bob-console",
		operations: []auth.Operation{auth.OperationActionApprove},
	}
	secondApprover = principal{
		subject: "carol", actor: "carol-console",
		operations: []auth.Operation{auth.OperationActionApprove},
	}
	registrant = principal{
		subject: "owner", actor: "operator",
		operations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationDelegate,
		},
	}
)

func (h *approvalHarness) as(who principal) context.Context {
	h.t.Helper()
	generation := who.generation
	if generation == 0 {
		generation = workspacePolicyGeneration
	}
	decision, err := auth.NewDecision(who.bindExecutor(auth.DecisionConfig{
		Subject: who.subject, Actor: who.actor, ClientID: who.client,
		OnBehalfOf:            who.onBehalfOf,
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     who.operations,
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID, approvalSecondPolicyID},
		PolicyGeneration:      generation,
		AuthenticationExpires: h.now().Add(time.Hour),
		RequestID: shoal.ID(fmt.Sprintf(
			"approval-request-%d", h.requests.Add(1))),
		CorrelationID: "approval-correlation",
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, err := h.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		h.t.Fatal(err)
	}
	return ctx
}

func (h *approvalHarness) context(deadline time.Time) fleet.RequestContext {
	return fleet.RequestContext{
		RequestID: "body-request", ReasonCode: "test", Deadline: deadline,
	}
}

func approvalActions(requireApproval bool) []fleet.Capability {
	return []fleet.Capability{{
		Name: "ops",
		Actions: []fleet.Action{
			{
				Name:             "deploy",
				InputSchema:      json.RawMessage(`{"type":"object"}`),
				OutputSchema:     json.RawMessage(`{"type":"object"}`),
				RequiresApproval: requireApproval,
			},
			{
				Name:         "status",
				InputSchema:  json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}}
}

func (h *approvalHarness) register(
	id shoal.ID, key shoal.ID, generation int64, requireApproval bool,
	parent shoal.ID,
) (fleet.Descriptor, error) {
	h.t.Helper()
	descriptor, err := h.opened.fleetRegistry.Register(
		h.as(registrant), fleet.RegisterRequest{
			Context:            h.context(h.now().Add(time.Minute)),
			RegistrationKey:    key,
			ExpectedGeneration: generation,
			Spec: fleet.Spec{
				ID: id, ParentID: parent,
				AuthorizationDomain: workspaceAuthorizationDomain,
				Scopes: []fleet.Scope{
					{SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID},
					{SourceID: workspaceSourceID, PolicyID: approvalSecondPolicyID},
				},
				ExecutorRef:    "local",
				Capabilities:   approvalActions(requireApproval),
				LeaseExpiresAt: h.now().Add(20 * time.Hour),
			},
		})
	if err != nil && generation == 0 && parent == "" && id == "gateway" {
		h.t.Fatal(err)
	}
	return descriptor, err
}

// enqueueRequest is the one request the tests hold for approval. Its deadline
// is fixed at creation, as an absolute time, so a re-request names the same
// deadline the original did.
type heldRequest struct {
	id       []byte
	action   string
	object   shoal.ID
	policy   []byte
	input    string
	deadline time.Time
}

func (h *approvalHarness) held(id string) heldRequest {
	return heldRequest{
		id: []byte(id), action: "deploy", object: "release-7",
		policy: workspaceGrantPolicyID, input: `{"version":"7"}`,
		deadline: h.now().Add(3 * time.Hour),
	}
}

func (h *approvalHarness) enqueue(request heldRequest) fleet.EnqueueRequest {
	return fleet.EnqueueRequest{
		ID: request.id, IdempotencyKey: append([]byte("key-"), request.id...),
		AgentID: "gateway", AgentGeneration: h.generationOf("gateway"),
		Capability: "ops", Action: request.action,
		SourceID: workspaceSourceID, PolicyID: request.policy,
		ObjectID: request.object, Input: json.RawMessage(request.input),
		Context: h.context(request.deadline),
	}
}

var generations sync.Map

func (h *approvalHarness) generationOf(id shoal.ID) int64 {
	if value, ok := generations.Load(h.root + string(id)); ok {
		return value.(int64)
	}
	return 1
}

func (h *approvalHarness) setGeneration(id shoal.ID, generation int64) {
	generations.Store(h.root+string(id), generation)
}

func (h *approvalHarness) request(
	who principal, request heldRequest,
) (fleet.ApprovalReceipt, error) {
	return h.opened.approvals.Request(h.as(who), h.enqueue(request))
}

func (h *approvalHarness) mustRequest(
	who principal, request heldRequest, want fleet.ApprovalState,
) fleet.ApprovalReceipt {
	h.t.Helper()
	receipt, err := h.request(who, request)
	if err != nil {
		h.t.Fatalf("request %s: %v", request.id, err)
	}
	if receipt.State != want {
		h.t.Fatalf("request %s state = %q, want %q", request.id, receipt.State, want)
	}
	return receipt
}

func (h *approvalHarness) decide(
	who principal, receipt fleet.ApprovalReceipt, verdict fleet.ApprovalVerdict,
) (fleet.ApprovalRecord, error) {
	return h.opened.approvals.Decide(h.as(who), fleet.ApprovalDecisionRequest{
		ID: receipt.ID, RequestDigest: receipt.RequestDigest,
		PolicyGeneration: receipt.PolicyGeneration, Verdict: verdict,
		Context: h.context(h.now().Add(time.Minute)),
	})
}

func (h *approvalHarness) status(id []byte) fleet.ApprovalStatus {
	h.t.Helper()
	return h.statusAs(requester, id)
}

// statusAs reads one approval as the given principal: the requester, or an
// approver eligible to decide it.
func (h *approvalHarness) statusAs(who principal, id []byte) fleet.ApprovalStatus {
	h.t.Helper()
	record, err := h.opened.approvals.Status(h.as(who), fleet.ApprovalStatusRequest{
		ID: id, Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		h.t.Fatalf("status %s: %v", id, err)
	}
	return record
}

// assertNotWork is the property the whole design rests on: a held request
// cannot be pulled, claimed, read as an action or performed, by its own
// requester through any dispatch route.
func (h *approvalHarness) assertNotWork(request heldRequest) {
	h.t.Helper()
	ctx := h.as(requester)
	page, err := h.opened.fleetDispatch.Pull(ctx, fleet.PullActionsRequest{
		Limit:   fleet.MaxDispatchListResults,
		Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		h.t.Fatalf("pull: %v", err)
	}
	for _, action := range page.Actions {
		if string(action.ID) == string(request.id) {
			h.t.Fatalf("pull returned held request %s", request.id)
		}
	}
	if _, err := h.opened.fleetDispatch.Claim(h.as(requester), fleet.ClaimRequest{
		ID: request.id, ExpectedVersion: 1, ClaimID: []byte("eager-claim"),
		Lease: time.Minute, Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		h.t.Fatalf("claim of held request = %v, want not found", err)
	}
	if _, err := h.opened.fleetDispatch.Status(h.as(requester), fleet.StatusRequest{
		ID: request.id, Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		h.t.Fatalf("status of held request = %v, want not found", err)
	}
	if _, err := h.opened.fleetDispatch.Enqueue(
		h.as(requester), h.enqueue(request),
	); !errors.Is(err, fleet.ErrApprovalRequired) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		h.t.Fatalf("enqueue of held request = %v, want approval required", err)
	}
	if _, err := h.opened.fleetDispatch.Invoke(h.as(requester), fleet.InvokeRequest{
		Enqueue: h.enqueue(request), ClaimID: []byte("invoke-claim"),
		Lease: time.Minute,
	}); !errors.Is(err, fleet.ErrApprovalRequired) {
		h.t.Fatalf("invoke of held request = %v, want approval required", err)
	}
}

func (h *approvalHarness) admit(id string) (fleet.AdmissionGrant, error) {
	return h.opened.admission.Request(h.as(requester), fleet.AdmissionRequest{
		ID: []byte(id), IdempotencyKey: []byte("admission-key-" + id),
		TokenID: []byte("token-" + id), AgentID: "gateway",
		AgentGeneration: h.generationOf("gateway"),
		Capability:      "ops", Action: "deploy",
		SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
		ObjectID: "release-7", Effects: fleet.Effects{fleet.EffectMutatesExternal},
		Input: json.RawMessage(`{"version":"7"}`), Lease: time.Minute,
		Context: h.context(h.now().Add(time.Hour)),
	})
}

// TestApprovalHeldWorkIsNeverPerformedBeforeApproval covers the first
// acceptance criterion end to end: held across retry and restart, Path B
// durably denied, materialized exactly once on approval, a duplicate approval
// performing nothing twice, and exactly one of two racing claims winning.
func TestApprovalHeldWorkIsNeverPerformedBeforeApproval(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("held-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if len(receipt.RequestDigest) != 32 ||
		receipt.PolicyGeneration != workspacePolicyGeneration ||
		!receipt.ExpiresAt.Equal(h.now().Add(fleet.DefaultApprovalWindow)) {
		t.Fatalf("held receipt = %+v", receipt)
	}
	h.assertNotWork(request)
	// Retried: still held, still not work.
	h.mustRequest(requester, request, fleet.ApprovalPending)
	h.assertNotWork(request)

	// Path B cannot serve it, and the denial is durable: a replay answers
	// from the record rather than re-adjudicating.
	//
	// The first answer is the denial. This was pinned as ErrActionCommitted
	// with a note to flip it when the publisher was fixed (#505): the denial
	// committed a cancelled record whose AuthorizedOperations is [invoke],
	// while the hosted publisher hardcoded dispatch for action.canceled, so
	// the publication failed and the caller was told a committed outcome
	// needed reconciliation. Nothing was ever granted, and the replay below
	// answered correctly, which is why it survived three reviews.
	//
	// deny now records the operation it transitioned under, and both gates
	// admit invoke for action.canceled.
	if grant, err := h.admit("admission-held"); err != nil ||
		grant.Outcome != fleet.AdmissionDenied ||
		len(grant.Token.TokenID) != 0 {
		t.Fatalf("path B first answer = %+v, %v", grant, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		grant, err := h.admit("admission-held")
		if err != nil || grant.Outcome != fleet.AdmissionDenied ||
			len(grant.Token.TokenID) != 0 {
			t.Fatalf("path B replay %d = %+v, %v", attempt, grant, err)
		}
	}

	// Restart: nothing in memory survives, and the request is still held.
	h.reopen()
	h.assertNotWork(request)
	h.mustRequest(requester, request, fleet.ApprovalPending)
	if grant, err := h.admit("admission-held"); err != nil ||
		grant.Outcome != fleet.AdmissionDenied {
		t.Fatalf("path B after restart = %+v, %v", grant, err)
	}

	h.advance(10 * time.Minute)
	approved, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.State != fleet.ApprovalApproved ||
		approved.ApproverSubject != approver.subject ||
		approved.ApproverActor != approver.actor ||
		!approved.DecidedAt.Equal(h.now()) {
		t.Fatalf("approved record = %+v", approved)
	}
	// Approved is not work either. Only the requester's re-request makes it
	// so, which is what keeps the approver's transition out of the action's
	// outbox and keeps the approval a record rather than a grant.
	h.assertNotWork(request)

	h.advance(time.Minute)
	materialized := h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	action := materialized.Action
	if action.State != fleet.DispatchQueued || action.Version != 1 ||
		string(action.ID) != string(request.id) ||
		string(action.ApprovalRequestDigest) != string(receipt.RequestDigest) ||
		action.ApprovalPolicyGeneration != workspacePolicyGeneration ||
		action.ApproverSubject != approver.subject ||
		action.ApproverActor != approver.actor ||
		!action.ApprovedAt.Equal(approved.DecidedAt) ||
		action.Subject != requester.subject {
		t.Fatalf("materialized action = %+v", action)
	}

	// A duplicate approval replays and performs nothing; a different
	// approver, or the same approver changing its mind, conflicts.
	replayed, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove)
	if err != nil || replayed.State != fleet.ApprovalEnqueued {
		t.Fatalf("duplicate approval = %+v, %v", replayed, err)
	}
	if _, err := h.decide(
		secondApprover, receipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalConflict) {
		t.Fatalf("second approver after decision = %v", err)
	}
	if _, err := h.decide(
		approver, receipt, fleet.ApprovalVerdictRefuse,
	); !errors.Is(err, fleet.ErrApprovalConflict) {
		t.Fatalf("changed verdict after decision = %v", err)
	}
	// Re-requesting again returns the one record; it does not make another.
	again := h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	if again.Action.Version != action.Version ||
		!again.Action.UpdatedAt.Equal(action.UpdatedAt) {
		t.Fatalf("re-request after materialization = %+v", again.Action)
	}

	// Exactly one of two racing claims wins.
	var wins atomic.Int64
	var wait sync.WaitGroup
	for _, claimID := range []string{"claim-a", "claim-b"} {
		wait.Add(1)
		ctx := h.as(requester)
		go func(claimID string) {
			defer wait.Done()
			if _, err := h.opened.fleetDispatch.Claim(ctx, fleet.ClaimRequest{
				ID: request.id, ExpectedVersion: 1, ClaimID: []byte(claimID),
				Lease: time.Minute, Context: h.context(h.now().Add(time.Minute)),
			}); err == nil {
				wins.Add(1)
			}
		}(claimID)
	}
	wait.Wait()
	if wins.Load() != 1 {
		t.Fatalf("racing claims won %d times, want exactly 1", wins.Load())
	}

	// The approver's audit went through the real interaction recorder and
	// authorized client under action_approve, attributed to the approver.
	h.close()
	corpus, err := explorer.Open(filepath.Join(h.root, "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, summary := range summaries {
		if summary.AuthorizationOperation != string(auth.OperationActionApprove) {
			continue
		}
		session, err := corpus.Interaction(context.Background(), summary.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.Actor.SubjectID == approver.subject &&
			session.Actor.ActorID == approver.actor &&
			len(session.Turns) == 1 && session.Turns[0].ToolCall != nil &&
			session.Turns[0].ToolCall.Kind ==
				"fleet.action_approve.approval_decision" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no action_approve decision audit attributed to the approver "+
			"among %d interactions", len(summaries))
	}
}

// crashAfterMaterializeWrite lands the approved → enqueued write and then
// reports failure, which is what a crash between the approval's two
// compare-and-set writes looks like: the first is durable, the second never
// ran.
type crashAfterMaterializeWrite struct {
	fleet.ApprovalStore
	armed *atomic.Bool
}

func (s crashAfterMaterializeWrite) ApplyApproval(
	ctx context.Context, mutation fleet.ApprovalMutation,
) (fleet.ApprovalRecord, error) {
	stored, err := s.ApprovalStore.ApplyApproval(ctx, mutation)
	if err == nil && mutation.Record.State == fleet.ApprovalEnqueued &&
		s.armed.Load() {
		return fleet.ApprovalRecord{}, errors.New("injected crash after the approval write")
	}
	return stored, err
}

// TestApprovalCrashBetweenTheTwoWritesRecovers injects the crash and recovers
// from it after a restart — and after the approval window has closed, because
// the decision to make the work was committed inside it.
func TestApprovalCrashBetweenTheTwoWritesRecovers(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	h.wrap = func(store fleet.ApprovalStore) fleet.ApprovalStore {
		return crashAfterMaterializeWrite{ApprovalStore: store, armed: armed}
	}
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")

	request := h.held("crash-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, err := h.request(requester, request); err == nil {
		t.Fatal("the injected crash did not surface")
	}
	// Reported honestly: committed to become work, and the work not yet
	// written.
	if status := h.status(request.id); status.State != fleet.ApprovalEnqueued ||
		status.Condition != fleet.ApprovalConditionAwaitingAction {
		t.Fatalf("approval after crash = %q/%q, want enqueued without action",
			status.State, status.Condition)
	}
	// The first write landed and the second did not: no work exists.
	h.assertNotWork(request)

	// Restart past the window. The approval is committed to become work, so
	// it still materializes, exactly once.
	armed.Store(false)
	h.advance(fleet.DefaultApprovalWindow + time.Minute)
	h.reopen()
	h.assertNotWork(request)
	recovered := h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	if recovered.Action.Version != 1 ||
		recovered.Action.ApproverSubject != approver.subject {
		t.Fatalf("recovered action = %+v", recovered.Action)
	}
	again := h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	if again.Action.Version != 1 ||
		!again.Action.UpdatedAt.Equal(recovered.Action.UpdatedAt) {
		t.Fatalf("second recovery made different work: %+v", again.Action)
	}
}

// TestApprovalRefusesEverySelfApproval walks the ways an identity can end up
// approving its own request. Each is refused, and the request stays pending
// and decidable by an independent approver afterwards.
func TestApprovalRefusesEverySelfApproval(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	delegating := principal{
		subject: "alice", actor: "alice-agent", onBehalfOf: []shoal.ID{"dave"},
		operations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke, auth.OperationDelegate,
		},
	}
	request := h.held("self-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	delegated := h.held("self-2")
	delegatedReceipt := h.mustRequest(delegating, delegated, fleet.ApprovalPending)
	approveOnly := []auth.Operation{auth.OperationActionApprove}
	for _, attempt := range []struct {
		name    string
		who     principal
		receipt fleet.ApprovalReceipt
	}{
		{"the requester's own subject",
			principal{subject: "alice", actor: "alice-console", operations: approveOnly},
			receipt},
		{"the requester's own actor",
			principal{subject: "mallory", actor: "alice-agent", operations: approveOnly},
			receipt},
		{"the requester's subject in another field",
			principal{subject: "mallory", actor: "alice", operations: approveOnly},
			receipt},
		{"acting on behalf of the requester",
			principal{subject: "mallory", actor: "mallory", onBehalfOf: []shoal.ID{"alice"},
				operations: []auth.Operation{
					auth.OperationActionApprove, auth.OperationDelegate,
				}},
			receipt},
		{"acting on behalf of anyone",
			principal{subject: "mallory", actor: "mallory", onBehalfOf: []shoal.ID{"erin"},
				operations: []auth.Operation{
					auth.OperationActionApprove, auth.OperationDelegate,
				}},
			receipt},
		{"the identity the requester acted for",
			principal{subject: "dave", actor: "dave-console", operations: approveOnly},
			delegatedReceipt},
		{"an approver that may also dispatch",
			principal{subject: "mallory", actor: "mallory", operations: []auth.Operation{
				auth.OperationActionApprove, auth.OperationDispatch,
			}},
			receipt},
		{"an approver that may also invoke",
			principal{subject: "mallory", actor: "mallory", operations: []auth.Operation{
				auth.OperationActionApprove, auth.OperationInvoke,
			}},
			receipt},
		{"an approver that may also execute",
			principal{subject: "mallory", actor: "mallory", operations: []auth.Operation{
				auth.OperationActionApprove, auth.OperationExecute,
			}},
			receipt},
		{"the agent itself",
			principal{subject: "gateway", actor: "gateway", operations: approveOnly},
			receipt},
		{"the agent's registrant",
			principal{subject: "owner", actor: "owner-console", operations: approveOnly},
			receipt},
	} {
		_, err := h.decide(attempt.who, attempt.receipt, fleet.ApprovalVerdictApprove)
		// Refused by the separation rule itself, named in the message, and
		// not by some earlier gate that happens to share its code.
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			!(strings.Contains(err.Error(), "not independent") ||
				strings.Contains(err.Error(), "on behalf of") ||
				strings.Contains(err.Error(), "create or perform")) {
			t.Fatalf("%s: decide = %v, want the separation refusal",
				attempt.name, err)
		}
	}
	for _, id := range [][]byte{request.id, delegated.id} {
		if state := h.statusAs(approver, id).State; state != fleet.ApprovalPending {
			t.Fatalf("after refused self-approvals %s = %q", id, state)
		}
	}
	// A principal without approve on the scope is not told the request
	// exists at all.
	if _, err := h.decide(principal{
		subject: "nobody", actor: "nobody",
		operations: []auth.Operation{auth.OperationList},
	}, receipt, fleet.ApprovalVerdictApprove); err == nil {
		t.Fatal("a principal without approve decided a request")
	}
	// And the control is not refusing everyone.
	if record, err := h.decide(
		approver, receipt, fleet.ApprovalVerdictRefuse,
	); err != nil || record.State != fleet.ApprovalRefused {
		t.Fatalf("independent refusal = %+v, %v", record, err)
	}
	h.mustRequest(requester, request, fleet.ApprovalRefused)
	h.assertNotWork(request)
}

// TestApprovalExpiryBoundaries covers expiry at both decision and
// materialization, and that expiry is written rather than inferred.
func TestApprovalExpiryBoundaries(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)

	// Decided at exactly ExpiresAt and one nanosecond after: refused, and the
	// record says expired.
	late := h.held("expiry-late")
	lateReceipt := h.mustRequest(requester, late, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Nanosecond)
	if _, err := h.decide(
		approver, lateReceipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalExpired) {
		t.Fatalf("approval at ExpiresAt+1 = %v", err)
	}
	if record := h.status(late.id); record.State != fleet.ApprovalExpired ||
		record.Approval.State != fleet.ApprovalExpired ||
		record.Approval.Verdict != "" {
		t.Fatalf("late approval record = %+v", record)
	}
	h.mustRequest(requester, late, fleet.ApprovalExpired)
	h.assertNotWork(late)

	// Exactly at ExpiresAt is already too late.
	edge := h.held("expiry-edge")
	edgeReceipt := h.mustRequest(requester, edge, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow)
	if _, err := h.decide(
		approver, edgeReceipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalExpired) {
		t.Fatalf("approval at ExpiresAt = %v", err)
	}

	// Approved in time, not materialized in time: refused, and recorded as
	// expired with the approval kept on the record.
	unused := h.held("expiry-unused")
	unusedReceipt := h.mustRequest(requester, unused, fleet.ApprovalPending)
	h.advance(30 * time.Minute)
	if _, err := h.decide(
		approver, unusedReceipt, fleet.ApprovalVerdictApprove,
	); err != nil {
		t.Fatal(err)
	}
	h.advance(30*time.Minute + time.Nanosecond)
	h.mustRequest(requester, unused, fleet.ApprovalExpired)
	if record := h.status(unused.id); record.Approval.Verdict != fleet.ApprovalVerdictApprove ||
		record.Approval.ApproverSubject != approver.subject {
		t.Fatalf("approved-then-expired record = %+v", record)
	}
	h.assertNotWork(unused)
}

// TestApprovalCoversOneExactRequest refuses every change to what was approved
// and every approval under a superseded policy generation.
func TestApprovalCoversOneExactRequest(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("exact-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)

	for name, changed := range map[string]heldRequest{
		"input":    func() heldRequest { c := request; c.input = `{"version":"8"}`; return c }(),
		"object":   func() heldRequest { c := request; c.object = "release-8"; return c }(),
		"scope":    func() heldRequest { c := request; c.policy = approvalSecondPolicyID; return c }(),
		"deadline": func() heldRequest { c := request; c.deadline = c.deadline.Add(time.Second); return c }(),
	} {
		if _, err := h.request(requester, changed); !errors.Is(err, fleet.ErrApprovalConflict) {
			t.Fatalf("changed %s = %v, want approval conflict", name, err)
		}
	}

	wrong := receipt
	wrong.RequestDigest = append([]byte(nil), receipt.RequestDigest...)
	wrong.RequestDigest[0] ^= 0xff
	if _, err := h.decide(
		approver, wrong, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalConflict) {
		t.Fatalf("approval of a digest nobody requested = %v", err)
	}
	stale := receipt
	stale.PolicyGeneration++
	if _, err := h.decide(
		approver, stale, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalSuperseded) {
		t.Fatalf("approval naming another generation = %v", err)
	}

	// The policy generation moves. An approver on the new generation cannot
	// approve a request made under the old one, and the requester on the new
	// generation is making a different request.
	h.reader.value.Store(workspacePolicyGeneration + 1)
	newer := approver
	newer.generation = workspacePolicyGeneration + 1
	if _, err := h.decide(
		newer, receipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalSuperseded) {
		t.Fatalf("approver on a newer generation = %v", err)
	}
	newerRequester := requester
	newerRequester.generation = workspacePolicyGeneration + 1
	if _, err := h.request(
		newerRequester, request,
	); !errors.Is(err, fleet.ErrApprovalConflict) {
		t.Fatalf("requester re-request under a new generation = %v", err)
	}
	// The approver on the newer generation observed that the request's
	// policy has moved on, and recorded it as stranded (#655): a request
	// under a superseded generation can never be decided again, and moving
	// the generation back does not revive it. Every other refused change
	// left it untouched until then.
	h.reader.value.Store(workspacePolicyGeneration)
	status := h.status(request.id)
	if status.State != fleet.ApprovalStranded ||
		status.Condition != fleet.ApprovalConditionPolicyMoved ||
		status.Approval.State != fleet.ApprovalStranded {
		t.Fatalf("after the policy moved the request is %q/%q stored %q",
			status.State, status.Condition, status.Approval.State)
	}
}

// TestApprovalIsPerActionAndSurvivesRegistration covers flag isolation, what
// registering the flag does to work already queued, and that neither a
// re-registration nor a delegate can drop it.
func TestApprovalIsPerActionAndSurvivesRegistration(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	// The action without the flag on the same agent is unaffected.
	plain := h.held("plain-1")
	plain.action = "status"
	queued, err := h.opened.fleetDispatch.Enqueue(h.as(requester), h.enqueue(plain))
	if err != nil || queued.State != fleet.DispatchQueued {
		t.Fatalf("enqueue of an action without approval = %+v, %v", queued, err)
	}
	if _, err := h.request(requester, plain); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("approval route for an action without approval = %v", err)
	}

	// Work queued before the flag existed: a second agent, flag off.
	if _, err := h.register("relay", "relay-registration", 0, false, ""); err != nil {
		t.Fatal(err)
	}
	early := heldRequest{
		id: []byte("relay-early"), action: "deploy", object: "release-7",
		policy: workspaceGrantPolicyID, input: `{"version":"7"}`,
		deadline: h.now().Add(3 * time.Hour),
	}
	earlyRequest := h.enqueue(early)
	earlyRequest.AgentID = "relay"
	if _, err := h.opened.fleetDispatch.Enqueue(h.as(requester), earlyRequest); err != nil {
		t.Fatalf("enqueue before the flag: %v", err)
	}
	// Registering the flag is a new generation, and the old-generation record
	// stops being offered by Pull and stops being claimable.
	//
	// It used to stop *resolving*, because resolveActionBinding pinned the
	// descriptor generation — which also meant a heartbeat stranded every
	// in-flight action on the agent (#486). The pin is gone from the
	// post-enqueue paths and the requirement is checked directly instead, so
	// the record is refused on the approval requirement rather than on a
	// generation number. Same outcome for this test's purpose, a different
	// and more accurate reason, and a heartbeat no longer causes it.
	descriptor, err := h.register("relay", "relay-approval", 1, true, "")
	if err != nil || descriptor.Generation != 2 {
		t.Fatalf("registering the flag = %+v, %v", descriptor, err)
	}
	page, err := h.opened.fleetDispatch.Pull(h.as(requester), fleet.PullActionsRequest{
		Limit: fleet.MaxDispatchListResults, Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range page.Actions {
		if string(action.ID) == "relay-early" {
			t.Fatal("old-generation work is still pullable after the flag")
		}
	}
	if _, err := h.opened.fleetDispatch.Claim(h.as(requester), fleet.ClaimRequest{
		ID: early.id, ExpectedVersion: 1, ClaimID: []byte("late-claim"),
		Lease: time.Minute, Context: h.context(h.now().Add(time.Minute)),
	}); !errors.Is(err, fleet.ErrApprovalRequired) {
		t.Fatalf("claim of old-generation work = %v, want approval required",
			err)
	}
	// New work at the new generation is refused.
	fresh := earlyRequest
	fresh.ID = []byte("relay-fresh")
	fresh.AgentGeneration = 2
	if _, err := h.opened.fleetDispatch.Enqueue(
		h.as(requester), fresh,
	); !errors.Is(err, fleet.ErrApprovalRequired) {
		t.Fatalf("enqueue after the flag = %v", err)
	}

	// Neither a re-registration nor a delegate can drop it.
	if _, err := h.register("relay", "relay-drop", 2, false, ""); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) {
		t.Fatalf("re-registration dropping approval = %v", err)
	}
	if _, err := h.register("relay-child", "child", 0, false, "relay"); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) {
		t.Fatalf("delegate dropping approval = %v", err)
	}
	if child, err := h.register("relay-child", "child-kept", 0, true, "relay"); err != nil ||
		!child.Capabilities[0].Actions[0].RequiresApproval {
		t.Fatalf("delegate keeping approval = %+v, %v", child, err)
	}

	// And it survives a restart: the descriptor codec carries it.
	h.reopen()
	resolved, err := h.opened.fleetRegistry.Resolve(
		h.as(principal{subject: "owner", actor: "operator",
			operations: []auth.Operation{auth.OperationAgentResolve}}),
		fleet.ResolveRequest{Context: h.context(h.now().Add(time.Minute)), ID: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range resolved.Descriptor.Capabilities {
		for _, action := range capability.Actions {
			if action.Name == "deploy" && !action.RequiresApproval {
				t.Fatal("the approval flag did not survive a restart")
			}
		}
	}
}

// TestApprovalPendingListsOnlyWhatTheCallerMayDecide checks the approver's
// queue: an independent approver sees the request, the requester does not see
// its own, and a decided request leaves the queue.
func TestApprovalPendingListsOnlyWhatTheCallerMayDecide(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("pending-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	list := func(who principal) []fleet.ApprovalRecord {
		page, err := h.opened.approvals.Pending(h.as(who), fleet.PendingApprovalsRequest{
			Limit: fleet.MaxApprovalListResults, Context: h.context(h.now().Add(time.Minute)),
		})
		if err != nil {
			t.Fatalf("pending for %s: %v", who.subject, err)
		}
		return page.Approvals
	}
	if got := list(approver); len(got) != 1 || string(got[0].ID) != "pending-1" {
		t.Fatalf("approver's queue = %+v", got)
	}
	self := principal{subject: "alice", actor: "alice-console",
		operations: []auth.Operation{auth.OperationActionApprove}}
	if got := list(self); len(got) != 0 {
		t.Fatalf("requester's own queue lists its request: %+v", got)
	}
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	if got := list(secondApprover); len(got) != 0 {
		t.Fatalf("decided request still listed: %+v", got)
	}
	if !strings.Contains(string(h.status(request.id).Approval.Request.Input), "version") {
		t.Fatal("status does not return the input an approver reviews")
	}
}
