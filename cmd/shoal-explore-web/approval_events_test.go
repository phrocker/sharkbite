// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetevents"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Approval lifecycle events (#653), against the composition openService
// builds: the real approval store as the outbox, the real approval event
// publisher and ledger, the real fleet event service and backend, and the
// real interaction auditor behind them. The only addition is
// eventProbe, which wraps the real publisher. It records who published and
// can lose a publication before or after it reaches the event service, which
// is what a crash between a transition and its event looks like.

var (
	requesterSubscriber = principal{
		subject: requester.subject, actor: requester.actor,
		operations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke,
			auth.OperationSubscriptionCreate, auth.OperationAgentResolve,
		},
	}
	approverSubscriber = principal{
		subject: approver.subject, actor: approver.actor,
		operations: []auth.Operation{
			auth.OperationActionApprove,
			auth.OperationSubscriptionCreate, auth.OperationAgentResolve,
		},
	}
	// coTenant holds dispatch and subscription on the request's scope, which
	// is everything the event join asks. It is neither the requester nor an
	// eligible approver, and approval Status answers it 404.
	coTenant = principal{
		subject: "mallory", actor: "mallory-agent",
		operations: []auth.Operation{
			auth.OperationDispatch, auth.OperationSubscriptionCreate,
			auth.OperationAgentResolve,
		},
	}
)

type publication struct {
	kind    string
	subject shoal.ID
	actor   shoal.ID
}

type eventProbe struct {
	mu sync.Mutex
	// before and after are, per kind, how many publications to lose before
	// they reach the event service and after it accepted them.
	before    map[string]int
	after     map[string]int
	published []publication
	// inner is the real publisher the probe wraps.
	inner fleet.ApprovalEventPublisher
}

func newEventProbe() *eventProbe {
	return &eventProbe{before: map[string]int{}, after: map[string]int{}}
}

func (p *eventProbe) take(budget map[string]int, kind string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if budget[kind] > 0 {
		budget[kind]--
		return true
	}
	return false
}

func (p *eventProbe) loseBefore(kind string, times int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.before[kind] = times
}

func (p *eventProbe) loseAfter(kind string, times int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.after[kind] = times
}

func (p *eventProbe) publications() []publication {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]publication(nil), p.published...)
}

type probedApprovalEvents struct {
	fleet.ApprovalEventPublisher
	probe    *eventProbe
	resolver auth.Resolver
}

func (p probedApprovalEvents) PublishApprovalEvent(
	ctx context.Context, transition fleet.ApprovalTransition,
) error {
	if p.probe.take(p.probe.before, transition.Kind) {
		return shoal.NewError(shoal.ErrorUnavailable, "injected: lost before publish")
	}
	if err := p.ApprovalEventPublisher.PublishApprovalEvent(ctx, transition); err != nil {
		return err
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	p.probe.mu.Lock()
	p.probe.published = append(p.probe.published, publication{
		kind: transition.Kind, subject: decision.Subject(), actor: decision.Actor(),
	})
	p.probe.mu.Unlock()
	if p.probe.take(p.probe.after, transition.Kind) {
		return shoal.NewError(shoal.ErrorUnavailable, "injected: lost after publish")
	}
	return nil
}

func newEventHarness(t *testing.T) (*approvalHarness, *eventProbe) {
	t.Helper()
	probe := newEventProbe()
	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	h.wrapEvents = func(inner fleet.ApprovalEventPublisher) fleet.ApprovalEventPublisher {
		probe.inner = inner
		return probedApprovalEvents{
			ApprovalEventPublisher: inner, probe: probe,
			resolver: authority.Resolver(),
		}
	}
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")
	return h, probe
}

var subscriptions sync.Map

// eventsFor subscribes as who, from the start of the retained stream, and
// returns every event delivered to it.
func (h *approvalHarness) eventsFor(ctx context.Context) []fleetevents.Event {
	h.t.Helper()
	counter, _ := subscriptions.LoadOrStore(h.root, new(int))
	token := counter.(*int)
	*token++
	subscription, err := h.opened.fleetEvents.Create(ctx, fleetevents.CreateRequest{
		Token:      []byte(fmt.Sprintf("subscribe-%d", *token)),
		RetryUntil: h.now().Add(time.Hour), AgentID: "gateway",
		AgentGeneration: h.generationOf("gateway"), TTL: time.Hour,
	})
	if err != nil {
		h.t.Fatalf("subscribe: %v", err)
	}
	page, err := h.opened.fleetEvents.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription.ID, Limit: fleetevents.MaxPageSize,
	})
	if err != nil {
		h.t.Fatalf("pull: %v", err)
	}
	return page.Events
}

// approvalEventsOf returns the kinds delivered to who for one approval, in
// stream order. Action kinds are included so a test can see that the
// subscriber was delivered something at all.
func (h *approvalHarness) approvalEventsOf(
	who principal, id []byte,
) ([]string, []fleetevents.Event) {
	h.t.Helper()
	var kinds []string
	var events []fleetevents.Event
	for _, event := range h.eventsFor(h.as(who)) {
		if bytes.Equal(event.ActionID, id) {
			kinds = append(kinds, event.Kind)
			events = append(events, event)
		}
	}
	return kinds, events
}

func eventOf(t *testing.T, events []fleetevents.Event, kind string) fleetevents.Event {
	t.Helper()
	var found []fleetevents.Event
	for _, event := range events {
		if event.Kind == kind {
			found = append(found, event)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s events, want exactly one", len(found), kind)
	}
	return found[0]
}

func assertKinds(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: delivered %v, want %v", name, got, want)
	}
}

// TestApprovalEventsArePublishedOncePerTransitionByItsMaker: each transition
// is one event, published by the principal that made it. The request and the
// materialization are published by the requester, and the decision by the
// approver. A replay at any step publishes nothing more.
func TestApprovalEventsArePublishedOncePerTransitionByItsMaker(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("events-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatalf("duplicate decision: %v", err)
	}
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)

	want := []publication{
		{fleet.ApprovalEventRequested, requester.subject, requester.actor},
		{fleet.ApprovalEventApproved, approver.subject, approver.actor},
		{fleet.ApprovalEventMaterialized, requester.subject, requester.actor},
	}
	if got := probe.publications(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	kinds, events := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "requester", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventApproved,
		fleet.ApprovalEventMaterialized, "action.enqueued")
	stored := h.status(request.id).Approval
	for kind, at := range map[string]time.Time{
		fleet.ApprovalEventRequested:    stored.RequestedAt,
		fleet.ApprovalEventApproved:     stored.DecidedAt,
		fleet.ApprovalEventMaterialized: stored.MaterializedAt,
	} {
		event := eventOf(t, events, kind)
		if !event.OccurredAt.Equal(at) || string(event.ProducerID) != "gateway" {
			t.Fatalf("%s occurred %v by %q, want %v by gateway",
				kind, event.OccurredAt, event.ProducerID, at)
		}
	}
	// The event names no approver and carries no input: the transition
	// happened, and Status is where its details are.
	approved := eventOf(t, events, fleet.ApprovalEventApproved)
	if approved.Reason.Code != "" || len(approved.ConsumedEvidence) != 0 ||
		len(approved.CorrelationID) != 0 {
		t.Fatalf("approved event carries %+v", approved)
	}
	// Nor the requester's correlation or reason, which the approval wire
	// does not show an approver.
	requested := eventOf(t, events, fleet.ApprovalEventRequested)
	if requested.Reason != (fleetevents.Event{}).Reason ||
		len(requested.CorrelationID) != 0 {
		t.Fatalf("requested event carries %+v", requested)
	}
}

// TestApprovalEventsForRefusalAndEachExpiryObserver: a refusal is the
// approver's. An expiry has no actor, so it is published by whoever's call
// observed it, and the event says which side that was. It occurred when the
// window closed, not when it was observed.
func TestApprovalEventsForRefusalAndEachExpiryObserver(t *testing.T) {
	h, probe := newEventHarness(t)
	refused := h.held("events-refused")
	byRequester := h.held("events-expired-requester")
	byApprover := h.held("events-expired-approver")
	refusedReceipt := h.mustRequest(requester, refused, fleet.ApprovalPending)
	h.mustRequest(requester, byRequester, fleet.ApprovalPending)
	approverReceipt := h.mustRequest(requester, byApprover, fleet.ApprovalPending)
	if _, err := h.decide(approver, refusedReceipt, fleet.ApprovalVerdictRefuse); err != nil {
		t.Fatal(err)
	}
	h.advance(fleet.DefaultApprovalWindow + time.Minute)
	h.mustRequest(requester, byRequester, fleet.ApprovalExpired)
	if _, err := h.decide(
		approver, approverReceipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalExpired) {
		t.Fatalf("decide after the window = %v", err)
	}

	for _, probeCase := range []struct {
		request  heldRequest
		kind     string
		observer fleet.ApprovalObserver
		by       principal
	}{
		{refused, fleet.ApprovalEventRefused, "", approver},
		{byRequester, fleet.ApprovalEventExpired,
			fleet.ApprovalObservedByRequester, requester},
		{byApprover, fleet.ApprovalEventExpired,
			fleet.ApprovalObservedByApprover, approver},
	} {
		kinds, events := h.approvalEventsOf(requesterSubscriber, probeCase.request.id)
		assertKinds(t, string(probeCase.request.id), kinds,
			fleet.ApprovalEventRequested, probeCase.kind)
		event := eventOf(t, events, probeCase.kind)
		stored := h.status(probeCase.request.id).Approval
		if probeCase.observer != "" {
			if event.Reason.Code != fleet.ApprovalExpiryReasonCode(probeCase.observer) ||
				!event.OccurredAt.Equal(stored.ExpiresAt) ||
				!stored.UpdatedAt.After(stored.ExpiresAt) {
				t.Fatalf("%s expiry event %+v, record %+v",
					probeCase.request.id, event, stored)
			}
		}
	}
	var expiries []publication
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventExpired {
			expiries = append(expiries, published)
		}
	}
	want := []publication{
		{fleet.ApprovalEventExpired, requester.subject, requester.actor},
		{fleet.ApprovalEventExpired, approver.subject, approver.actor},
	}
	if fmt.Sprint(expiries) != fmt.Sprint(want) {
		t.Fatalf("expiries published by %v, want %v", expiries, want)
	}
}

// TestApprovalEventLostInACrashIsRecoveredOnce: a transition commits and its
// event does not reach the event service, or reaches it and is not
// acknowledged. The caller is told the transition committed and the event did
// not publish, as an unmarked 503. The same call, retried after a restart,
// publishes the event exactly once.
func TestApprovalEventLostInACrashIsRecoveredOnce(t *testing.T) {
	h, probe := newEventHarness(t)

	// Lost after the event service accepted it: the requester's hold.
	request := h.held("events-crash")
	probe.loseAfter(fleet.ApprovalEventRequested, 1)
	if _, err := h.request(requester, request); !errors.Is(
		err, fleet.ErrApprovalEventUnpublished) {
		t.Fatalf("hold whose acknowledgement was lost = %v", err)
	}
	if status := h.status(request.id); status.Approval.State != fleet.ApprovalPending {
		t.Fatalf("the hold did not commit: %q", status.Approval.State)
	}
	h.reopen()
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)

	// Lost before it reached the event service: the approver's decision.
	probe.loseBefore(fleet.ApprovalEventApproved, 1)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); !errors.Is(
		err, fleet.ErrApprovalEventUnpublished) {
		t.Fatalf("decision whose event was lost = %v", err)
	}
	if kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id); len(kinds) != 1 {
		t.Fatalf("before recovery: %v", kinds)
	}
	h.reopen()
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatalf("the approver's retry: %v", err)
	}
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after recovery", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventApproved,
		fleet.ApprovalEventMaterialized, "action.enqueued")
}

// TestAStrandedApproverEventDoesNotBlockTheRequester is #480's wedge and
// #650's, for approvals. The approver's decision commits and its event never
// publishes, and the approver does not come back. The requester's own
// transitions are unaffected: its materialization answers success, and its
// events publish. The approver's row stays the approver's, so nothing the
// requester does publishes it under the requester's name. When the approver
// does return, inside the window, the event publishes once. It is late, not
// lost.
func TestAStrandedApproverEventDoesNotBlockTheRequester(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("events-stranded")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	probe.loseBefore(fleet.ApprovalEventApproved, 1000)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); !errors.Is(
		err, fleet.ErrApprovalEventUnpublished) {
		t.Fatalf("decision = %v", err)
	}
	materialized := h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	if materialized.Action.Version != 1 {
		t.Fatalf("materialized %+v", materialized.Action)
	}
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "with the approver's event stranded", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventMaterialized,
		"action.enqueued")
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventApproved {
			t.Fatalf("the approver's event was published by %v", published)
		}
	}

	probe.loseBefore(fleet.ApprovalEventApproved, 0)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatalf("the approver's return: %v", err)
	}
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	kinds, _ = h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after the approver returned", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventMaterialized,
		"action.enqueued", fleet.ApprovalEventApproved)
}

// TestApprovalEventsAreConcealedLikeStatus: who is delivered an approval's
// events is who approval Status would answer. That is the requester, and an
// approver eligible to decide it. A co-tenant holding dispatch and
// subscription on the scope is delivered the action event the request became
// (the control: its subscription works) and none of the approval's. So is an
// approver who is not independent of the request, and one whose eligibility
// comes only through a workspace narrowing.
func TestApprovalEventsAreConcealedLikeStatus(t *testing.T) {
	h, _ := newEventHarness(t)
	request := h.held("events-concealed")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	all := []string{
		fleet.ApprovalEventRequested, fleet.ApprovalEventApproved,
		fleet.ApprovalEventMaterialized, "action.enqueued",
	}

	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "requester", kinds, all...)
	kinds, _ = h.approvalEventsOf(coTenant, request.id)
	assertKinds(t, "co-tenant", kinds, "action.enqueued")
	// The approval's 404 for the same principal, so the stream is not a
	// side door into what Status refuses.
	if _, err := h.opened.approvals.Status(h.as(coTenant), fleet.ApprovalStatusRequest{
		ID: request.id, Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("co-tenant status = %v", err)
	}
	// An eligible approver subscribing (approve, never dispatch) is shown
	// them, as Status would show them.
	kinds, _ = h.approvalEventsOf(approverSubscriber, request.id)
	approvalOnly := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if fleetevents.IsApprovalKind(kind) {
			approvalOnly = append(approvalOnly, kind)
		}
	}
	assertKinds(t, "eligible approver", approvalOnly, all[:3]...)
	// The registrant holds approve, but registered the agent: not
	// independent, so not shown.
	owner := principal{subject: registrant.subject, actor: "owner-console",
		operations: approverSubscriber.operations}
	kinds, _ = h.approvalEventsOf(owner, request.id)
	for _, kind := range kinds {
		if fleetevents.IsApprovalKind(kind) {
			t.Fatalf("an approver not independent of the request was "+
				"delivered %s", kind)
		}
	}

	// A narrowing that sheds dispatch would make mallory pass separation.
	// Every approver path refuses a narrowed decision, and so does this one.
	mallory := principal{subject: "mallory-approver", actor: "mallory-console",
		operations: []auth.Operation{
			auth.OperationActionApprove, auth.OperationDispatch,
			auth.OperationSubscriptionCreate, auth.OperationAgentResolve,
			auth.OperationWorkspaceSettingsWrite,
		}}
	created, err := h.opened.settings.Update(
		h.as(mallory), "mallory-events", workspace.UpdateRequest{
			MutationID: "narrow-to-approve-and-subscribe",
			Narrowing: workspace.UpdateNarrowing{
				AllowedOperations: workspace.OperationSelection{
					Present: true, Values: []auth.Operation{
						auth.OperationActionApprove,
						auth.OperationSubscriptionCreate,
						auth.OperationAgentResolve,
					},
				},
			},
		})
	if err != nil {
		t.Fatalf("create the narrowing workspace: %v", err)
	}
	applier, ok := h.opened.settings.(webapi.WorkspaceSettingsApplier)
	if !ok {
		t.Fatal("the composed settings provider cannot apply settings")
	}
	narrowed, err := webapi.ApplyWorkspaceSettingsForOperation(
		h.as(mallory), applier, h.authority.Binder(), created.WorkspaceID,
		auth.OperationSubscriptionCreate, workspace.MaximumLimits(), nil)
	if err != nil {
		t.Fatalf("apply the narrowing: %v", err)
	}
	decision, err := h.authority.Resolver().Resolve(narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if decision.AuthorizeObject(auth.OperationDispatch, auth.ResourceRequest{
		AuthorizationDomain: workspaceAuthorizationDomain,
		SourceID:            workspaceSourceID, PolicyID: workspaceGrantPolicyID,
		ObjectID: "release-7",
	}, h.now()) == nil {
		t.Fatal("the narrowed decision still holds dispatch; the probe is vacuous")
	}
	for _, event := range h.eventsFor(narrowed) {
		if bytes.Equal(event.ActionID, request.id) &&
			fleetevents.IsApprovalKind(event.Kind) {
			t.Fatalf("a narrowed approver was delivered %s", event.Kind)
		}
	}
}

// TestApprovalEventsCannotBeForgedOnThePublicRoute: a principal holding
// event_publish on the scope cannot publish an approval event for a request
// nobody approved. Nor can it for one somebody did.
func TestApprovalEventsCannotBeForgedOnThePublicRoute(t *testing.T) {
	h, _ := newEventHarness(t)
	request := h.held("events-forged")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	forger := principal{subject: "forger", actor: "forger",
		operations: []auth.Operation{auth.OperationEventPublish}}
	for _, kind := range []string{
		fleet.ApprovalEventApproved, fleet.ApprovalEventMaterialized,
		fleet.ApprovalEventRequested,
	} {
		_, err := h.opened.fleetEvents.Publish(h.as(forger), fleetevents.PublishRequest{
			Token: []byte("forged-" + kind), RetryUntil: h.now().Add(time.Hour),
			Event: fleetevents.Event{
				Kind: kind, ProducerID: []byte("gateway"), ProducerGeneration: 1,
				ActionID: request.id, TransitionID: []byte("forged"),
				Evidence: []fleetevents.Evidence{{
					SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
					ObjectID: "release-7",
				}},
				OccurredAt: h.now(),
			},
		})
		if err == nil || !strings.Contains(err.Error(), "trusted publication") {
			t.Fatalf("forged %s = %v", kind, err)
		}
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after forgery attempts", kinds, fleet.ApprovalEventRequested)
}

// TestTheLedgerConfirmsOnlyDurableTransitions: the trusted route admits an
// approval event only as the event of a durable outbox row, field for field.
// The probe here is the ledger the event service consults and the publisher
// the approval service uses, over the outbox the store wrote. A changed kind,
// time, operation or receipt, or an event for a request nobody approved, is
// refused. So is publishing a real row under anyone but the principal it
// names.
func TestTheLedgerConfirmsOnlyDurableTransitions(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("events-ledger")
	unapproved := h.held("events-ledger-unapproved")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	h.mustRequest(requester, unapproved, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	rows, err := h.opened.approvalOutbox.ApprovalTransitions(
		context.Background(), request.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Outcome != fleet.ApprovalEventPublished ||
		rows[1].Outcome != fleet.ApprovalEventPublished ||
		rows[0].Transition.Publisher.Subject != requester.subject ||
		rows[1].Transition.Publisher.Subject != approver.subject ||
		rows[1].Transition.Publisher.Operation != auth.OperationActionApprove {
		t.Fatalf("outbox rows = %+v", rows)
	}
	decision := rows[1].Transition
	ledger, err := explorerfleetevents.NewApprovalLedger(h.opened.approvalOutbox)
	if err != nil {
		t.Fatal(err)
	}
	genuine, genuineReceipt, err := explorerfleetevents.ApprovalPublication(decision)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := ledger.VerifyApprovalEvent(
		ctx, auth.OperationActionApprove, genuine, genuineReceipt); err != nil {
		t.Fatalf("the genuine decision was refused: %v", err)
	}
	refused := func(name string, operation auth.Operation,
		request fleetevents.PublishRequest, receipt fleetevents.LifecycleReceipt) {
		t.Helper()
		if err := ledger.VerifyApprovalEvent(
			ctx, operation, request, receipt); !shoal.IsErrorCode(
			err, shoal.ErrorUnauthorized) {
			t.Fatalf("%s = %v, want refused", name, err)
		}
	}
	refused("under dispatch", auth.OperationDispatch, genuine, genuineReceipt)
	changed := genuine
	changed.Event.Kind = fleet.ApprovalEventRefused
	refused("as a refusal", auth.OperationActionApprove, changed, genuineReceipt)
	changed = genuine
	changed.Event.OccurredAt = changed.Event.OccurredAt.Add(time.Second)
	refused("at another time", auth.OperationActionApprove, changed, genuineReceipt)
	changed = genuine
	changed.Event.ProducerID = []byte("another-agent")
	refused("for another agent", auth.OperationActionApprove, changed, genuineReceipt)
	otherReceipt := genuineReceipt
	otherReceipt.RequestID = "someone-elses-request"
	refused("under another receipt", auth.OperationActionApprove, genuine, otherReceipt)
	// "approval.approved" for the request nobody approved: its own rows hold
	// no such transition, whatever the event says.
	forged := genuine
	forged.Event.ActionID = append([]byte(nil), unapproved.id...)
	refused("for an unapproved request", auth.OperationActionApprove,
		forged, genuineReceipt)

	// The real publisher, asked to publish the approver's row as the
	// requester: refused, and not "may".
	if err := probe.inner.PublishApprovalEvent(h.as(requester), decision); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) {
		t.Fatalf("the requester publishing the approver's row = %v", err)
	}
	if may, err := probe.inner.MayPublishApprovalEvent(
		h.as(requester), decision); err != nil || may {
		t.Fatalf("may the requester publish the approver's row = %v, %v", may, err)
	}
	if may, err := probe.inner.MayPublishApprovalEvent(
		h.as(approver), decision); err != nil || !may {
		t.Fatalf("may the approver publish its own row = %v, %v", may, err)
	}
	// Publishing it again as its approver is a repeat, not a second event.
	if err := probe.inner.PublishApprovalEvent(h.as(approver), decision); err != nil {
		t.Fatalf("the approver publishing its row again = %v", err)
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after the repeat", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventApproved)
}

// TestAnUnresolvableTargetDoesNotWedgeTheStream: after the host stops
// configuring the agent's executor ref, Status answers an approver 503 for
// the request (the binding cannot be resolved). Delivery must not. An error
// there would fail every pull of every approve-holder's subscription behind
// that one event, and would tell the agent's registrant, who may see nothing,
// that an approval event is there. Every such subscriber is answered with a
// page that simply lacks it.
func TestAnUnresolvableTargetDoesNotWedgeTheStream(t *testing.T) {
	h, _ := newEventHarness(t)
	request := h.held("events-unresolvable")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	h.close()
	h.executors = configuredFleetExecutors{
		"other": configuredFleetExecutor{reference: "other"},
	}
	if err := h.tryOpen(); err != nil {
		t.Fatalf("reopen without the agent's executor: %v", err)
	}
	owner := principal{subject: registrant.subject, actor: "owner-console",
		operations: approverSubscriber.operations}
	for name, who := range map[string]principal{
		"eligible approver": approverSubscriber, "registrant": owner,
		"co-tenant": coTenant,
	} {
		for _, event := range h.eventsFor(h.as(who)) {
			if fleetevents.IsApprovalKind(event.Kind) {
				t.Fatalf("%s was delivered %s for an unresolvable target",
					name, event.Kind)
			}
		}
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "requester", kinds, fleet.ApprovalEventRequested)
}

// TestAnotherApproverCannotPublishTheDecidersRow: carol holds approve on the
// scope and is eligible, so the operation check alone would admit her. The
// identity match against the row's stored publisher is what refuses her.
func TestAnotherApproverCannotPublishTheDecidersRow(t *testing.T) {
	h, probe := newEventHarness(t)
	request := h.held("events-other-approver")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	probe.loseBefore(fleet.ApprovalEventApproved, 1)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); !errors.Is(
		err, fleet.ErrApprovalEventUnpublished) {
		t.Fatalf("decision = %v", err)
	}
	rows, err := h.opened.approvalOutbox.ApprovalTransitions(
		context.Background(), request.id)
	if err != nil || len(rows) != 2 || rows[1].Outcome != "" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	decision := rows[1].Transition
	if may, err := probe.inner.MayPublishApprovalEvent(
		h.as(secondApprover), decision); err != nil || may {
		t.Fatalf("may another approver publish it = %v, %v", may, err)
	}
	if err := probe.inner.PublishApprovalEvent(
		h.as(secondApprover), decision); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("another approver publishing it = %v", err)
	}
	// Nor does carol's own decide replay (a conflict: she did not decide it)
	// drain it on bob's behalf.
	if _, err := h.decide(secondApprover, receipt, fleet.ApprovalVerdictApprove); !errors.Is(
		err, fleet.ErrApprovalConflict) {
		t.Fatalf("carol's decide = %v", err)
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "before bob returns", kinds, fleet.ApprovalEventRequested)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	kinds, _ = h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after bob returns", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventApproved)
}

// unreadableApprovals answers every read of the armed approval with a
// persistent internal error, as a record that no longer decodes, or a head
// whose committed cell never becomes visible, would.
type unreadableApprovals struct {
	fleet.ApprovalStore
	armed *atomic.Bool
	id    []byte
}

func (s unreadableApprovals) GetApproval(
	ctx context.Context, id []byte,
) (fleet.ApprovalRecord, error) {
	if s.armed.Load() && bytes.Equal(id, s.id) {
		return fleet.ApprovalRecord{}, shoal.NewError(
			shoal.ErrorInternal, "invalid committed fleet approval")
	}
	return s.ApprovalStore.GetApproval(ctx, id)
}

// TestAPermanentlyUnreadableApprovalNeverFailsAPage: the delivery gate reads
// the approval for every subscriber the scope join admits, co-tenants
// included. If that read fails for good, no subscriber's page may fail with
// it, or one bad record would stop the stream for the whole scope and tell
// every subscriber an approval event is there. The event is withheld, from
// everyone, and everything else is delivered.
//
// Since the guard head-ownership fix, another store can no longer make an
// approval unreadable by adopting its head. This is the second line.
func TestAPermanentlyUnreadableApprovalNeverFailsAPage(t *testing.T) {
	h, _ := newEventHarness(t)
	request := h.held("events-unreadable")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	armed := &atomic.Bool{}
	h.wrap = func(store fleet.ApprovalStore) fleet.ApprovalStore {
		return unreadableApprovals{ApprovalStore: store, armed: armed, id: request.id}
	}
	h.reopen()
	armed.Store(true)
	for name, who := range map[string]principal{
		"requester": requesterSubscriber, "approver": approverSubscriber,
		"co-tenant": coTenant,
	} {
		kinds, _ := h.approvalEventsOf(who, request.id)
		for _, kind := range kinds {
			if fleetevents.IsApprovalKind(kind) {
				t.Fatalf("%s was delivered %s for an unreadable approval", name, kind)
			}
		}
		if name != "approver" {
			assertKinds(t, name, kinds, "action.enqueued")
		}
	}
}
