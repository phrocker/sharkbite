// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// droppingPublisher fails one publication of a chosen kind and then behaves
// normally, which is the fault the outbox exists for: a transient event
// backend error, or a crash between the durable write and the publish.
type droppingPublisher struct {
	inner   *ActionEventPublisher
	kind    string
	dropped bool
	// published counts the publications that actually landed, per kind. The
	// test needs this rather than error codes alone: "reconcile returned nil"
	// is also what a reconcile that silently drained nothing returns, and
	// skipping must not become losing.
	published map[string]int
}

// MayPublishActionEvent delegates, so the fixture does not get to decide
// the question under test.
func (p *droppingPublisher) MayPublishActionEvent(
	ctx context.Context, kind string, record fleet.ActionRecord,
) (bool, error) {
	return p.inner.MayPublishActionEvent(ctx, kind, record)
}

func (p *droppingPublisher) PublishActionEvent(
	ctx context.Context, kind string, record fleet.ActionRecord,
) error {
	if !p.dropped && kind == p.kind {
		p.dropped = true
		return shoal.NewError(
			shoal.ErrorUnavailable, "transient event backend fault")
	}
	if err := p.inner.PublishActionEvent(ctx, kind, record); err != nil {
		return err
	}
	if p.published == nil {
		p.published = map[string]int{}
	}
	p.published[kind]++
	return nil
}

// TestAStrandedTransitionDoesNotBlockAnotherPrincipalsClaim is #480 item 3, driven
// through the real store, the real event service and the real publisher.
//
// ReconcileActionTransitions publishes every pending transition for an action,
// in order, under one caller's decision. The gates it passes through are
// per-kind: publisherMatchesTransition wants the enqueuer for
// action.enqueued and the claimant for action.claimed, and both
// decision.AuthorizeObject and fleetevents' own authorize run against the
// *row's* operation. An outbox holding both kinds is therefore drainable by
// neither principal.
//
// Before #437 a single identity owned every transition of an action, so
// reconcile always drained. The per-kind rule is correct for attribution and
// makes this case worse than it was: the claim is durably written and the
// worker is told to reconcile an outcome no principal can reconcile.
func TestAStrandedTransitionDoesNotBlockAnotherPrincipalsClaim(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	config = explorerfleet.ConfigureRuntime(config)
	ConfigureRuntime(&config)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()

	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	bindConfig := func(config auth.DecisionConfig) context.Context {
		t.Helper()
		decision, err := auth.NewDecision(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := authority.Binder().Bind(context.Background(), decision)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	configFor := func(subject, actor, request string, ops ...auth.Operation) auth.DecisionConfig {
		return auth.DecisionConfig{
			Subject: shoal.ID(subject), Actor: shoal.ID(actor),
			AuthorizationDomain:   []byte("domain"),
			AllowedOperations:     ops,
			PermittedSourceIDs:    [][]byte{[]byte("source")},
			PermittedPolicyIDs:    [][]byte{[]byte("policy")},
			PolicyGeneration:      1,
			AuthenticationExpires: now.Add(2 * time.Hour),
			RequestID:             shoal.ID(request),
			CorrelationID:         "correlation",
		}
	}
	bind := func(subject, actor, request string, ops ...auth.Operation) context.Context {
		t.Helper()
		return bindConfig(configFor(subject, actor, request, ops...))
	}
	enqueuer := bind("owner", "actor", "request",
		auth.OperationAgentRegister, auth.OperationDispatch, auth.OperationInvoke)
	// A worker since #391: bound to the descriptor's executor ref.
	workerConfig := configFor("worker", "worker-actor", "request",
		auth.OperationExecute)
	workerConfig.ServiceRole = auth.ServiceRoleActionExecution
	workerConfig.ServiceCeilingIdentity = "executor-ceiling"
	workerConfig.ExecutorBinding = "exec"
	worker := bindConfig(workerConfig)

	registry, _ := newIntegrationRegistry(
		t, authority.Resolver(), now,
		&integrationExecutor{
			result: fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		})
	eventBackend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	capability := explorerfleetcap.New()
	eventService, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: eventBackend, Resolver: authority.Resolver(),
		GenerationReader: integrationGeneration{}, LeaseValidator: integrationLease{},
		Auditor: integrationAuditor{}, CursorKey: bytes32(7),
		Clock: func() time.Time { return now },
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	real, err := NewActionEventPublisher(
		eventService, authority.Resolver(), capability,
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// The enqueued publication is dropped once, so its outbox row stays
	// pending while the durable record is committed.
	publisher := &droppingPublisher{inner: real, kind: "action.enqueued"}
	dispatch, err := explorerfleet.ComposeDispatch(
		runtime, registry, authority.Resolver(), integrationActionRecorder{},
		publisher, nil, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}

	request := integrationEnqueueRequest(now, "wedged", "wedged-key")
	queued, enqueueErr := dispatch.Enqueue(enqueuer, request)
	if !publisher.dropped {
		t.Fatal("the enqueued publication was never attempted, so no row is " +
			"left pending and this test cannot reach the wedge")
	}
	if enqueueErr == nil {
		t.Fatal("the dropped publication was not reported, so the fixture " +
			"does not reproduce the fault the outbox exists for")
	}
	// The record is committed even though the publication failed. That is the
	// whole point of the outbox, and the precondition for the wedge.
	stored, err := dispatch.Status(enqueuer, fleet.StatusRequest{
		ID: request.ID, Context: integrationRequestContext(now),
	})
	if err != nil {
		t.Fatalf("the enqueue did not commit, so there is no pending row: %v", err)
	}
	if stored.State != fleet.DispatchQueued {
		t.Fatalf("stored state = %q, want queued", stored.State)
	}
	_ = queued

	// The predicate the reconciler now asks, exercised against the real
	// publisher and this real row before anything claims it.
	//
	// It is what makes the skip a decision rather than an inference from an
	// error code, and the distinction matters for a specific reason:
	// classifying on ErrorUnauthorized made correctness depend on where
	// #398's concealment turns a denial into ObjectNotFound. A visibility
	// denial arriving as not-found would have blocked the caller's own
	// transition again, and treating not-found as a skip would have
	// swallowed a row that was genuinely absent. Asked as a question neither
	// ambiguity exists: the row came out of the outbox, so it exists, and
	// both codes mean only that this caller has no standing on the object.
	for _, probe := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"the transition's own author", enqueuer, true},
		// A false answer, not an error, which is the whole point.
		{"a different principal", worker, false},
		// The guard the per-kind rule was providing, and the one thing a
		// later relaxation could quietly widen: same scopes, so it can see
		// the action, and no operation that publishes this kind.
		{"a read-only caller with visibility",
			bind("owner", "actor", "request", auth.OperationRetrieve), false},
	} {
		may, err := real.MayPublishActionEvent(
			probe.ctx, "action.enqueued", stored)
		if err != nil {
			t.Fatalf("%s: the question failed instead of answering: %v",
				probe.name, err)
		}
		if may != probe.want {
			t.Fatalf("%s: MayPublishActionEvent = %v, want %v",
				probe.name, may, probe.want)
		}
	}
	// An unanswerable question is an error, never a skip. A kind the
	// authorization table does not recognise cannot be judged, and skipping
	// it would leave a row pending that nothing will ever deliver.
	if _, err := real.MayPublishActionEvent(
		enqueuer, "action.invented", stored); err == nil {
		t.Fatal("an unrecognised kind answered instead of erroring, so a " +
			"row nothing can classify would be skipped forever")
	}
	// And asking must not publish: the enqueued row is still pending below.
	if publisher.published["action.enqueued"] != 0 {
		t.Fatal("asking whether a row may be published published it")
	}

	// Now a worker that is not the enqueuer claims it. Its own transition is
	// publishable by it; the stranded action.enqueued row is not — it carries
	// the enqueuer's authority, and the publisher's gates are per-kind.
	//
	// This used to fail. ReconcileActionTransitions returned on the first
	// refusal, which was the stranded row, so the claim committed and was
	// answered ErrActionCommitted: a gateway worker that had just performed
	// an irreversible external effect was told to reconcile an outcome that
	// needed no reconciling, over somebody else's undelivered event.
	if _, err := dispatch.Claim(worker, fleet.ClaimRequest{
		ID: request.ID, ExpectedVersion: stored.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: integrationRequestContext(now),
	}); err != nil {
		t.Fatalf("a worker's claim was refused over another principal's "+
			"stranded transition: %v", err)
	}
	// Its own event landed, which is what makes the skip a skip rather than a
	// silent no-op: the claim did publish something.
	if publisher.published["action.claimed"] != 1 {
		t.Fatalf("action.claimed published %d times, want 1: the claim "+
			"succeeded without delivering its own transition",
			publisher.published["action.claimed"])
	}
	// And the stranded row is still pending, not quietly dropped.
	if publisher.published["action.enqueued"] != 0 {
		t.Fatalf("action.enqueued published %d times already: the worker "+
			"delivered a transition it has no authority for",
			publisher.published["action.enqueued"])
	}

	// The skip must not have become a grant. This is the guard the per-kind
	// gate was providing, and the one thing relaxing it could quietly widen
	// from "must be this transition's author" to "anyone who can see the
	// action". Probed here, while the enqueued row is still pending, because
	// a reconcile against an empty outbox is a no-op and would pass whatever
	// the gate did.
	//
	// A reader has the action's scopes and so can see it; it holds no
	// operation that publishes any lifecycle kind.
	reader := bind("reader", "reader-actor", "request", auth.OperationRetrieve)
	readerErr := dispatch.ReconcileActionTransitions(reader, request.ID)
	if publisher.published["action.enqueued"] != 0 {
		t.Fatalf("a caller holding only a read operation delivered the "+
			"stranded transition (err=%v): skipping a row this caller may "+
			"not publish must not become permission to publish it", readerErr)
	}

	// The enqueuer drains what is its own. Before the fix this returned the
	// claimed row's refusal after publishing the enqueued one — partial
	// progress reported as total failure.
	if err := dispatch.ReconcileActionTransitions(
		enqueuer, request.ID); err != nil {
		t.Fatalf("the enqueuer could not drain its own stranded row: %v", err)
	}
	if publisher.published["action.enqueued"] != 1 {
		t.Fatalf("action.enqueued published %d times after the enqueuer "+
			"reconciled, want 1", publisher.published["action.enqueued"])
	}

	// Nothing is left for anyone, and a second pass by either principal is a
	// no-op rather than an error. Both are asserted because "returns nil" and
	// "published nothing new" are different claims and the fix must make both
	// true.
	before := publisher.published["action.enqueued"] +
		publisher.published["action.claimed"]
	for name, caller := range map[string]context.Context{
		"enqueuer": enqueuer, "worker": worker,
	} {
		if err := dispatch.ReconcileActionTransitions(
			caller, request.ID); err != nil {
			t.Fatalf("a drained outbox refused a %s reconcile: %v", name, err)
		}
	}
	if after := publisher.published["action.enqueued"] +
		publisher.published["action.claimed"]; after != before {
		t.Fatalf("a drained outbox published %d more events, so rows are "+
			"being completed without being delivered or vice versa",
			after-before)
	}

}

func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// refusingPublisher refuses one kind with Unauthorized, every time.
type refusingPublisher struct {
	inner *ActionEventPublisher
	kind  string
}

// MayPublishActionEvent delegates. The refusal under test is a *publish*
// failure of the caller's own transition, which is never asked about — so
// answering truthfully here keeps the test about the own-row rule rather
// than about the predicate.
func (p *refusingPublisher) MayPublishActionEvent(
	ctx context.Context, kind string, record fleet.ActionRecord,
) (bool, error) {
	return p.inner.MayPublishActionEvent(ctx, kind, record)
}

func (p *refusingPublisher) PublishActionEvent(
	ctx context.Context, kind string, record fleet.ActionRecord,
) error {
	if kind == p.kind {
		return shoal.NewError(
			shoal.ErrorUnauthorized, "refused for this test")
	}
	return p.inner.PublishActionEvent(ctx, kind, record)
}

// TestACallersOwnPublicationFailureIsStillAnError is the other half of the
// skip, and without it the skip is strictly worse than the bug it fixes.
//
// Skipping a row refused as Unauthorized is right when the row carries
// another principal's authority — it will drain when that principal
// reconciles. It is wrong for the caller's *own* transition: there is nobody
// else to drain it, and a committed write whose event never published is
// exactly what ErrActionCommitted exists to report. Swallowing that would
// turn a lost event into a silent success.
//
// Mutating the service to skip the caller's own row as well left every test
// in explorerfleetevents, fleet and explorerfleet green, which is why this
// exists.
func TestACallersOwnPublicationFailureIsStillAnError(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	config = explorerfleet.ConfigureRuntime(config)
	ConfigureRuntime(&config)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()

	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationDispatch,
			auth.OperationInvoke,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(2 * time.Hour),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}

	registry, _ := newIntegrationRegistry(
		t, authority.Resolver(), now,
		&integrationExecutor{
			result: fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		})
	eventBackend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	capability := explorerfleetcap.New()
	eventService, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: eventBackend, Resolver: authority.Resolver(),
		GenerationReader: integrationGeneration{}, LeaseValidator: integrationLease{},
		Auditor: integrationAuditor{}, CursorKey: bytes32(7),
		Clock: func() time.Time { return now },
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	real, err := NewActionEventPublisher(
		eventService, authority.Resolver(), capability,
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := explorerfleet.ComposeDispatch(
		runtime, registry, authority.Resolver(), integrationActionRecorder{},
		&refusingPublisher{inner: real, kind: "action.enqueued"}, nil,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}

	// The enqueue's own event is refused as Unauthorized. The write lands —
	// the outbox exists for exactly that — and the caller must be told.
	request := integrationEnqueueRequest(now, "own-failure", "own-failure-key")
	if _, err := dispatch.Enqueue(ctx, request); err == nil {
		t.Fatal("a caller's own transition failed to publish and the " +
			"operation reported success: the event is undelivered, nobody " +
			"else can deliver it, and nothing says so")
	} else if !errors.Is(err, fleet.ErrActionCommitted) {
		t.Fatalf("own publication failure = %v, want ErrActionCommitted: "+
			"the write landed, so this is not a refusal", err)
	}

	// And it really did commit, which is what makes the error the right one.
	stored, err := dispatch.Status(ctx, fleet.StatusRequest{
		ID: request.ID, Context: integrationRequestContext(now),
	})
	if err != nil || stored.State != fleet.DispatchQueued {
		t.Fatalf("the refused publication rolled the write back: %#v, %v",
			stored, err)
	}
}
