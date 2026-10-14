// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetevents"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The approval sweep (#655), against the composition openService builds: the
// host's own sweep minter and binder, the real approval store and outbox, the
// real event service, ledger and interaction auditor.

func (h *approvalHarness) sweep() fleet.ApprovalSweepResult {
	h.t.Helper()
	if h.opened.approvalService == nil {
		h.t.Fatal("the composition holds no approval service to sweep")
	}
	total, err := sweepApprovals(context.Background(), h.opened.approvalService)
	if err != nil {
		h.t.Fatalf("sweep: %v", err)
	}
	return total
}

var sweeper = publication{
	kind: "", subject: fleet.ApprovalSweeperSubject, actor: fleet.ApprovalSweeperSubject,
}

// requestFor requests a held request against another agent.
func (h *approvalHarness) requestFor(
	agent shoal.ID, request heldRequest,
) fleet.ApprovalReceipt {
	h.t.Helper()
	enqueue := h.enqueue(request)
	enqueue.AgentID = agent
	enqueue.AgentGeneration = h.generationOf(agent)
	receipt, err := h.opened.approvals.Request(h.as(requester), enqueue)
	if err != nil || receipt.State != fleet.ApprovalPending {
		h.t.Fatalf("request %s on %s = %+v, %v", request.id, agent, receipt, err)
	}
	return receipt
}

// TestTheSweepRecordsWhatNobodyWillTouchAgain: requests whose target moved
// are stranded while their window is open, and every request whose window
// has closed is expired. Each is published once, by the system observer, and
// nothing becomes work. A live request and a final one are left alone, and a
// second pass changes nothing.
func TestTheSweepRecordsWhatNobodyWillTouchAgain(t *testing.T) {
	t.Parallel()
	h, probe := newEventHarness(t)
	if _, err := h.register("relay", "relay-registration", 0, true, ""); err != nil {
		t.Fatal(err)
	}
	movedPending := h.held("sweep-moved-pending")
	movedApproved := h.held("sweep-moved-approved")
	h.requestFor("relay", movedPending)
	approvedReceipt := h.requestFor("relay", movedApproved)
	if _, err := h.decide(approver, approvedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	lapsedPending := h.held("sweep-lapsed-pending")
	lapsedApproved := h.held("sweep-lapsed-approved")
	refused := h.held("sweep-refused")
	h.mustRequest(requester, lapsedPending, fleet.ApprovalPending)
	lapsedReceipt := h.mustRequest(requester, lapsedApproved, fleet.ApprovalPending)
	refusedReceipt := h.mustRequest(requester, refused, fleet.ApprovalPending)
	if _, err := h.decide(approver, lapsedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	if _, err := h.decide(approver, refusedReceipt, fleet.ApprovalVerdictRefuse); err != nil {
		t.Fatal(err)
	}

	// Nothing has happened yet that any request could not survive.
	if total := h.sweep(); total.Examined != 5 || total.Expired+total.Stranded+
		total.Failed != 0 {
		t.Fatalf("first pass = %+v", total)
	}

	// The relay's generation moves, so its requests can never be
	// re-requested at the generation they name.
	if _, err := h.register("relay", "relay-regenerate", 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if total := h.sweep(); total.Stranded != 2 || total.Expired != 0 || total.Failed != 0 {
		t.Fatalf("pass after the target moved = %+v", total)
	}
	for _, request := range []heldRequest{movedPending, movedApproved} {
		status := h.status(request.id)
		if status.Approval.State != fleet.ApprovalStranded ||
			status.State != fleet.ApprovalStranded ||
			status.Condition != fleet.ApprovalConditionTargetMoved {
			t.Fatalf("%s = %q/%q stored %q", request.id, status.State,
				status.Condition, status.Approval.State)
		}
		_, events := h.approvalEventsOf(requesterSubscriber, request.id)
		event := eventOf(t, events, fleet.ApprovalEventStranded)
		if event.Reason.Code != fleet.ApprovalStrandReasonCode(
			fleet.ApprovalConditionTargetMoved, fleet.ApprovalObservedBySweeper) ||
			!event.OccurredAt.Equal(status.Approval.UpdatedAt) {
			t.Fatalf("%s stranded event = %+v", request.id, event)
		}
	}

	// Every window closes.
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	if total := h.sweep(); total.Expired != 2 || total.Stranded != 0 || total.Failed != 0 {
		t.Fatalf("pass after the windows closed = %+v", total)
	}
	for _, request := range []heldRequest{lapsedPending, lapsedApproved} {
		stored := h.status(request.id).Approval
		if stored.State != fleet.ApprovalExpired {
			t.Fatalf("%s = %q", request.id, stored.State)
		}
		_, events := h.approvalEventsOf(requesterSubscriber, request.id)
		event := eventOf(t, events, fleet.ApprovalEventExpired)
		if event.Reason.Code != fleet.ApprovalExpiryReasonCode(
			fleet.ApprovalObservedBySweeper) ||
			!event.OccurredAt.Equal(stored.ExpiresAt) {
			t.Fatalf("%s expired event = %+v", request.id, event)
		}
	}
	if stored := h.status(refused.id).Approval; stored.State != fleet.ApprovalRefused {
		t.Fatalf("the sweep moved a refused request to %q", stored.State)
	}
	if total := h.sweep(); total.Expired+total.Stranded+total.Failed+total.Raced != 0 {
		t.Fatalf("a second pass changed something: %+v", total)
	}

	// Published by the system observer, and by nobody else.
	var swept []publication
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventStranded ||
			published.kind == fleet.ApprovalEventExpired {
			swept = append(swept, published)
		}
	}
	if len(swept) != 4 {
		t.Fatalf("swept publications = %v", swept)
	}
	for _, published := range swept {
		if published.subject != sweeper.subject || published.actor != sweeper.actor {
			t.Fatalf("%s published by %s/%s, want the system observer",
				published.kind, published.subject, published.actor)
		}
	}
	// None of it is work, and none of it can become work.
	h.assertNotWork(lapsedPending)
	for _, request := range []heldRequest{lapsedApproved} {
		h.mustRequest(requester, request, fleet.ApprovalExpired)
	}
	for _, request := range []heldRequest{movedPending, movedApproved, lapsedApproved} {
		if _, err := h.opened.fleetDispatch.Status(h.as(requester), fleet.StatusRequest{
			ID: request.id, Context: h.context(h.now().Add(time.Minute)),
		}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("%s became an action: %v", request.id, err)
		}
	}
	// A co-tenant the scope join admits is shown none of it.
	for _, request := range []heldRequest{movedPending, lapsedPending} {
		kinds, _ := h.approvalEventsOf(coTenant, request.id)
		assertKinds(t, "co-tenant", kinds)
	}
}

// TestTheSweepStrandsAMovedPolicyOnlyForward: a request under a policy
// generation the authority has moved past is stranded. One under a
// generation ahead of the authority — what a replica still on the old
// configuration sees during a rollout — is left alone.
func TestTheSweepStrandsAMovedPolicyOnlyForward(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	older := h.held("sweep-policy-older")
	h.mustRequest(requester, older, fleet.ApprovalPending)
	h.reader.value.Store(workspacePolicyGeneration + 1)
	newer := h.held("sweep-policy-newer")
	newerRequester := requester
	newerRequester.generation = workspacePolicyGeneration + 1
	h.mustRequest(newerRequester, newer, fleet.ApprovalPending)

	if total := h.sweep(); total.Stranded != 1 || total.Failed != 0 {
		t.Fatalf("pass after the policy moved = %+v", total)
	}
	if status := h.statusAs(newerRequester, older.id); status.Approval.State != fleet.ApprovalStranded ||
		status.Condition != fleet.ApprovalConditionPolicyMoved {
		t.Fatalf("older request = %q/%q", status.Approval.State, status.Condition)
	}
	// A replica behind the new generation.
	h.reader.value.Store(workspacePolicyGeneration)
	if total := h.sweep(); total.Stranded+total.Expired != 0 {
		t.Fatalf("a replica behind the request's generation stranded it: %+v", total)
	}
	h.reader.value.Store(workspacePolicyGeneration + 1)
	if status := h.statusAs(newerRequester, newer.id); status.Approval.State != fleet.ApprovalPending {
		t.Fatalf("newer request = %q", status.Approval.State)
	}
}

// TestTheSweepWaitsForTheDeadlineOnEnqueuedWithoutAction: an approval that
// committed to become work and whose action was never written may still be
// written by a re-request in flight, so neither a moved target nor the
// deadline itself is enough. Only past the deadline by the grace, when no
// call that could write it can still be running, is it stranded.
func TestTheSweepWaitsForTheDeadlineOnEnqueuedWithoutAction(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	h := newCrashHarness(t, armed)
	request := h.held("sweep-enqueued")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, err := h.request(requester, request); err == nil {
		t.Fatal("the injected crash did not surface")
	}
	armed.Store(false)
	if _, err := h.register("gateway", "regenerate", 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if total := h.sweep(); total.Stranded+total.Expired != 0 {
		t.Fatalf("an enqueued approval was stranded on its target: %+v", total)
	}
	h.advance(request.deadline.Sub(h.now()))
	if total := h.sweep(); total.Stranded+total.Expired != 0 {
		t.Fatalf("an enqueued approval was stranded at its deadline: %+v", total)
	}
	if status := h.status(request.id); status.Approval.State != fleet.ApprovalEnqueued ||
		status.State != fleet.ApprovalUnresolvable {
		t.Fatalf("inside the grace = %q/%q stored %q", status.State,
			status.Condition, status.Approval.State)
	}
	h.advance(fleet.ApprovalStrandGrace)
	if total := h.sweep(); total.Stranded != 1 || total.Failed != 0 {
		t.Fatalf("past the grace = %+v", total)
	}
	status := h.status(request.id)
	if status.Approval.State != fleet.ApprovalStranded ||
		status.Condition != fleet.ApprovalConditionDeadlinePassed ||
		status.Approval.MaterializedAt.IsZero() {
		t.Fatalf("past the grace = %q/%q stored %+v", status.State,
			status.Condition, status.Approval)
	}
	if _, err := h.opened.fleetDispatch.Status(h.as(requester), fleet.StatusRequest{
		ID: request.id, Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("a stranded approval became an action: %v", err)
	}
}

// TestTheSweepStrandsATakenIdentity: another route's action took the
// identity between the pre-commit check and the commit, so the approval can
// never be written there. Nobody asks Status; the sweep records it.
func TestTheSweepStrandsATakenIdentity(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	request := heldRequest{}
	h := newHookedHarness(t, func(
		h *approvalHarness, mutation fleet.ApprovalMutation,
	) error {
		if mutation.Record.State != fleet.ApprovalEnqueued ||
			!armed.CompareAndSwap(true, false) {
			return nil
		}
		squat := h.enqueue(request)
		squat.Action = "status"
		_, err := h.opened.fleetDispatch.Enqueue(h.as(principal{
			subject: "mallory", actor: "mallory",
			operations: []auth.Operation{auth.OperationDispatch},
		}), squat)
		return err
	})
	request = h.held("sweep-taken")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, err := h.request(requester, request); !errors.Is(err, fleet.ErrActionConflict) {
		t.Fatalf("materialize onto a taken identity = %v", err)
	}
	if total := h.sweep(); total.Stranded != 1 {
		t.Fatalf("sweep = %+v", total)
	}
	stored := h.status(request.id)
	if stored.Approval.State != fleet.ApprovalStranded ||
		stored.Approval.StrandedCause != fleet.ApprovalConditionIdentityTaken {
		t.Fatalf("status = %+v", stored.Approval)
	}
}

// TestConcurrentSweepsWriteOneTransition: sweeps on several replicas at once.
// Every write is a compare-and-set from the version read, so each request
// gets exactly one expiry: one version, one outbox row, one delivered event.
// A sweep that loses either raced it or, here where every sweep reads the
// same clock, wrote the identical record, which the store answers as the
// same transition rather than a second one.
func TestConcurrentSweepsWriteOneTransition(t *testing.T) {
	// Not parallel (#710): pinned to one core this fails even run alone,
	// which is a defect of its own, and with the package's parallel tests
	// competing for a small runner's cores it would fail as often.
	h, probe := newEventHarness(t)
	const requests = 4
	for i := 0; i < requests; i++ {
		h.mustRequest(requester, h.held(fmt.Sprintf("sweep-concurrent-%d", i)),
			fleet.ApprovalPending)
	}
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	var wait sync.WaitGroup
	totals := make([]fleet.ApprovalSweepResult, 4)
	for i := range totals {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			totals[i], _ = sweepApprovals(context.Background(), h.opened.approvalService)
		}(i)
	}
	wait.Wait()
	for _, total := range totals {
		if total.Failed != 0 || total.Expired+total.Raced != requests {
			t.Fatalf("a concurrent sweep = %+v", total)
		}
	}
	for _, publication := range probe.publications() {
		if publication.kind == fleet.ApprovalEventExpired &&
			publication.subject != fleet.ApprovalSweeperSubject {
			t.Fatalf("an expiry was published by %s", publication.subject)
		}
	}
	for i := 0; i < requests; i++ {
		id := []byte(fmt.Sprintf("sweep-concurrent-%d", i))
		stored := h.status(id).Approval
		rows, err := h.opened.approvalOutbox.ApprovalTransitions(
			context.Background(), id)
		if err != nil || stored.State != fleet.ApprovalExpired ||
			stored.Version != 2 || len(rows) != 2 {
			t.Fatalf("%s = %q v%d with %d rows, %v",
				id, stored.State, stored.Version, len(rows), err)
		}
		kinds, _ := h.approvalEventsOf(requesterSubscriber, id)
		assertKinds(t, string(id), kinds,
			fleet.ApprovalEventRequested, fleet.ApprovalEventExpired)
	}
}

// TestASweepAndADecisionRaceAtomically: a decision has read a live request
// and is about to approve it when the target moves and a sweep strands it.
// The two writes are compare-and-sets from the same version, so exactly one
// commits. The decision is the loser here: it re-reads, finds the request
// stranded, is refused as such, and approves nothing.
func TestASweepAndADecisionRaceAtomically(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	var sweepTotal fleet.ApprovalSweepResult
	var injected error
	h := newHookedHarness(t, func(
		h *approvalHarness, mutation fleet.ApprovalMutation,
	) error {
		if mutation.Record.State != fleet.ApprovalApproved ||
			!armed.CompareAndSwap(true, false) {
			return nil
		}
		if _, err := h.register("relay", "relay-regenerate", 1, true, ""); err != nil {
			injected = err
			return nil
		}
		sweepTotal, injected = sweepApprovals(
			context.Background(), h.opened.approvalService)
		return nil
	})
	if _, err := h.register("relay", "relay-registration", 0, true, ""); err != nil {
		t.Fatal(err)
	}
	request := h.held("sweep-race")
	receipt := h.requestFor("relay", request)
	armed.Store(true)
	_, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove)
	if armed.Load() || injected != nil || sweepTotal.Stranded != 1 {
		t.Fatalf("the racing sweep did not land: %v, %+v", injected, sweepTotal)
	}
	if !errors.Is(err, fleet.ErrApprovalStranded) {
		t.Fatalf("a decision that lost to the sweep = %v", err)
	}
	stored := h.status(request.id).Approval
	if stored.State != fleet.ApprovalStranded || stored.Verdict != "" ||
		stored.Version != 2 ||
		stored.StrandedCause != fleet.ApprovalConditionTargetMoved {
		t.Fatalf("after the race = %+v", stored)
	}
}

// TestStatusRecordsWhatTheAskingPartyObserves: Status writes the final state
// it reports as the party asking, under that party's own operation, and the
// event says which party observed it. An approver's Status and a
// requester's Status of two lapsed requests are two observers.
func TestStatusRecordsWhatTheAskingPartyObserves(t *testing.T) {
	t.Parallel()
	h, probe := newEventHarness(t)
	byApprover := h.held("status-observed-approver")
	byRequester := h.held("status-observed-requester")
	h.mustRequest(requester, byApprover, fleet.ApprovalPending)
	h.mustRequest(requester, byRequester, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	if status := h.statusAs(approver, byApprover.id); status.Approval.State != fleet.ApprovalExpired {
		t.Fatalf("approver status = %q stored %q", status.State, status.Approval.State)
	}
	if status := h.status(byRequester.id); status.Approval.State != fleet.ApprovalExpired {
		t.Fatalf("requester status = %q stored %q", status.State, status.Approval.State)
	}
	for request, observer := range map[string]fleet.ApprovalObserver{
		string(byApprover.id):  fleet.ApprovalObservedByApprover,
		string(byRequester.id): fleet.ApprovalObservedByRequester,
	} {
		_, events := h.approvalEventsOf(requesterSubscriber, []byte(request))
		if event := eventOf(t, events, fleet.ApprovalEventExpired); event.Reason.Code !=
			fleet.ApprovalExpiryReasonCode(observer) {
			t.Fatalf("%s expiry observed as %q, want %s", request, event.Reason.Code, observer)
		}
	}
	var expiries []publication
	for _, published := range probe.publications() {
		if published.kind == fleet.ApprovalEventExpired {
			expiries = append(expiries, published)
		}
	}
	want := []publication{
		{fleet.ApprovalEventExpired, approver.subject, approver.actor},
		{fleet.ApprovalEventExpired, requester.subject, requester.actor},
	}
	if fmt.Sprint(expiries) != fmt.Sprint(want) {
		t.Fatalf("expiries published by %v, want %v", expiries, want)
	}
	// A co-tenant asking Status is answered as for a missing ID and writes
	// nothing.
	other := h.held("status-observed-cotenant")
	h.mustRequest(requester, other, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	if _, err := h.opened.approvals.Status(h.as(coTenant), fleet.ApprovalStatusRequest{
		ID: other.id, Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("co-tenant status = %v", err)
	}
	rows, err := h.opened.approvalOutbox.ApprovalTransitions(context.Background(), other.id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("a co-tenant's Status wrote %d rows, %v", len(rows), err)
	}
}

// TestTheSweepRetriesItsOwnUnpublishedEvent: a transition the sweep wrote
// whose event did not publish is published by the next pass, once.
func TestTheSweepRetriesItsOwnUnpublishedEvent(t *testing.T) {
	t.Parallel()
	h, probe := newEventHarness(t)
	request := h.held("sweep-unpublished")
	h.mustRequest(requester, request, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	probe.loseBefore(fleet.ApprovalEventExpired, 1)
	if total := h.sweep(); total.Expired != 1 || total.Unpublished != 1 {
		t.Fatalf("first pass = %+v", total)
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "before the retry", kinds, fleet.ApprovalEventRequested)
	h.sweep()
	h.sweep()
	kinds, _ = h.approvalEventsOf(requesterSubscriber, request.id)
	assertKinds(t, "after the retry", kinds,
		fleet.ApprovalEventRequested, fleet.ApprovalEventExpired)
}

// TestTheSweeperCannotPublishWhatNobodyWrote: the system observer is a
// publisher like any other. Its decision admits its own rows, and the ledger
// still refuses an approval event no durable row describes, whoever asks.
// A party cannot publish the observer's row either.
func TestTheSweeperCannotPublishWhatNobodyWrote(t *testing.T) {
	t.Parallel()
	h, probe := newEventHarness(t)
	held := h.held("sweep-forged")
	lapsed := h.held("sweep-forged-lapsed")
	h.mustRequest(requester, held, fleet.ApprovalPending)
	h.mustRequest(requester, lapsed, fleet.ApprovalPending)
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	// One genuine sweeper row, on the lapsed request only: the held one's
	// window closed too, so it is the fresh pending record below that
	// stands for "a request nobody stranded".
	fresh := h.held("sweep-forged-fresh")
	h.mustRequest(requester, fresh, fleet.ApprovalPending)
	h.sweep()
	rows, err := h.opened.approvalOutbox.ApprovalTransitions(
		context.Background(), lapsed.id)
	if err != nil || len(rows) != 2 {
		t.Fatalf("lapsed rows = %+v, %v", rows, err)
	}
	genuine := rows[1].Transition
	if genuine.Publisher.Observer != fleet.ApprovalObservedBySweeper ||
		genuine.Publisher.Operation != auth.OperationApprovalSweep ||
		genuine.Publisher.Subject != fleet.ApprovalSweeperSubject {
		t.Fatalf("the sweep's row names %+v", genuine.Publisher)
	}
	minter, err := newApprovalSweepMinter(
		h.authority.Binder(), h.reader, h.now)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := minter.bind(context.Background(), fleet.ApprovalSweepScope{
		SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The observer's own row: admitted, and a repeat rather than a second
	// event.
	if err := probe.inner.PublishApprovalEvent(observer, genuine); err != nil {
		t.Fatalf("the observer republishing its row = %v", err)
	}
	// Its row, as a party: refused.
	if err := probe.inner.PublishApprovalEvent(h.as(requester), genuine); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) {
		t.Fatalf("the requester publishing the observer's row = %v", err)
	}
	// A stranding nobody wrote, on the fresh pending request, built to look
	// like the observer's: refused by the ledger.
	freshRows, err := h.opened.approvalOutbox.ApprovalTransitions(
		context.Background(), fresh.id)
	if err != nil || len(freshRows) != 1 {
		t.Fatalf("fresh rows = %+v, %v", freshRows, err)
	}
	record := fleet.CloneApprovalRecord(freshRows[0].Transition.Record)
	record.Version++
	record.State = fleet.ApprovalStranded
	record.StrandedCause = fleet.ApprovalConditionTargetMoved
	record.UpdatedAt = h.now()
	forged, err := fleet.NewApprovalTransition(record, genuine.Publisher)
	if err != nil {
		t.Fatalf("build the forged row: %v", err)
	}
	if err := probe.inner.PublishApprovalEvent(observer, forged); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) {
		t.Fatalf("a stranding nobody wrote = %v, want refused", err)
	}
	ledger, err := explorerfleetevents.NewApprovalLedger(h.opened.approvalOutbox)
	if err != nil {
		t.Fatal(err)
	}
	request, receipt, err := explorerfleetevents.ApprovalPublication(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.VerifyApprovalEvent(
		context.Background(), auth.OperationApprovalSweep, request, receipt,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("the ledger on a stranding nobody wrote = %v", err)
	}
	// And the public route still refuses the kind outright.
	forger := principal{subject: "forger", actor: "forger",
		operations: []auth.Operation{auth.OperationEventPublish}}
	if _, err := h.opened.fleetEvents.Publish(h.as(forger), fleetevents.PublishRequest{
		Token: []byte("forged-stranded"), RetryUntil: h.now().Add(time.Hour),
		Event: fleetevents.Event{
			Kind: fleet.ApprovalEventStranded, ProducerID: []byte("gateway"),
			ProducerGeneration: 1, ActionID: fresh.id,
			TransitionID: []byte("forged"),
			Evidence: []fleetevents.Evidence{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
				ObjectID: "release-7",
			}},
			OccurredAt: h.now(),
		},
	}); err == nil {
		t.Fatal("the public route published approval.stranded")
	}
	kinds, _ := h.approvalEventsOf(requesterSubscriber, fresh.id)
	assertKinds(t, "fresh", kinds, fleet.ApprovalEventRequested)
}
