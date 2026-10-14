// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func requesterPublisher(record ApprovalRecord) ApprovalPublisher {
	return ApprovalPublisher{
		Operation: auth.OperationDispatch, Subject: record.Request.Subject,
		Actor: record.Request.Actor, ClientID: record.Request.ClientID,
		OnBehalfOf: append([]shoal.ID(nil), record.Request.OnBehalfOf...),
		RequestID:  "request", CorrelationID: "correlation",
		AuthorizationFingerprint: auth.Fingerprint{3},
		AuthorizationExpiresAt:   record.RequestedAt.Add(time.Hour),
	}
}

func approverPublisher(record ApprovalRecord) ApprovalPublisher {
	return ApprovalPublisher{
		Operation: auth.OperationActionApprove, Subject: record.ApproverSubject,
		Actor: record.ApproverActor, ClientID: record.ApproverClientID,
		RequestID:                record.DecisionRequestID,
		CorrelationID:            record.DecisionCorrelationID,
		AuthorizationFingerprint: record.ApproverFingerprint,
		AuthorizationExpiresAt:   record.RequestedAt.Add(time.Hour),
	}
}

// TestApprovalTransitionBindsItsPublisher: an outbox row names one principal
// it may be published by, and that principal is fixed by the record. A row
// naming anyone else is refused at construction, so "published under the
// identity of the principal that made it" is a property of the data. It does
// not depend on every call site.
func TestApprovalTransitionBindsItsPublisher(t *testing.T) {
	pending := validApprovalRecord(t)
	approved := decided(pending, ApprovalApproved, ApprovalVerdictApprove)
	approved.DecisionCorrelationID = "decision-correlation"
	expired := pending
	expired.Version, expired.State = 2, ApprovalExpired
	expired.UpdatedAt = pending.ExpiresAt.Add(time.Minute)

	observedBy := func(p ApprovalPublisher, observer ApprovalObserver) ApprovalPublisher {
		p.Observer = observer
		return p
	}
	stranger := func(p ApprovalPublisher) ApprovalPublisher {
		p.Subject = "mallory"
		return p
	}
	onBehalf := func(p ApprovalPublisher) ApprovalPublisher {
		p.OnBehalfOf = []shoal.ID{"dave"}
		return p
	}
	otherRequest := func(p ApprovalPublisher) ApprovalPublisher {
		p.RequestID = "someone-elses-request"
		return p
	}
	asApprove := func(p ApprovalPublisher) ApprovalPublisher {
		p.Operation = auth.OperationActionApprove
		return p
	}
	anApprover := ApprovalPublisher{
		Operation: auth.OperationActionApprove, Subject: "carol", Actor: "carol",
		RequestID: "carol-request", CorrelationID: "correlation",
		AuthorizationFingerprint: auth.Fingerprint{5},
		AuthorizationExpiresAt:   pending.RequestedAt.Add(time.Hour),
	}
	for _, probe := range []struct {
		name      string
		record    ApprovalRecord
		publisher ApprovalPublisher
		ok        bool
	}{
		{"requested by the requester", pending, requesterPublisher(pending), true},
		{"requested by a stranger", pending, stranger(requesterPublisher(pending)), false},
		{"requested under approve", pending, asApprove(requesterPublisher(pending)), false},
		{"requested with an observer", pending,
			observedBy(requesterPublisher(pending), ApprovalObservedByRequester), false},
		{"approved by its approver", approved, approverPublisher(approved), true},
		{"approved by the requester", approved, requesterPublisher(approved), false},
		{"approved by another approver", approved, stranger(approverPublisher(approved)), false},
		{"approved under another request", approved,
			otherRequest(approverPublisher(approved)), false},
		{"approved on someone's behalf", approved,
			onBehalf(approverPublisher(approved)), false},
		{"approved under another correlation", approved, func() ApprovalPublisher {
			p := approverPublisher(approved)
			p.CorrelationID = "other"
			return p
		}(), false},
		{"approved under another credential", approved, func() ApprovalPublisher {
			p := approverPublisher(approved)
			p.AuthorizationFingerprint = auth.Fingerprint{7}
			return p
		}(), false},
		{"expiry observed by the requester posing as an approver", expired,
			observedBy(asApprove(requesterPublisher(expired)),
				ApprovalObservedByApprover), false},
		{"expiry observed by the requester", expired,
			observedBy(requesterPublisher(expired), ApprovalObservedByRequester), true},
		{"expiry observed by an approver", expired,
			observedBy(anApprover, ApprovalObservedByApprover), true},
		{"expiry with no observer", expired, requesterPublisher(expired), false},
		{"expiry observed by an approver under dispatch", expired,
			observedBy(requesterPublisher(expired), ApprovalObservedByApprover), false},
		{"expiry observed by a delegated approver", expired,
			observedBy(onBehalf(anApprover), ApprovalObservedByApprover), false},
		{"expiry observed by a stranger as the requester", expired,
			observedBy(stranger(requesterPublisher(expired)),
				ApprovalObservedByRequester), false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, err := NewApprovalTransition(probe.record, probe.publisher)
			if (err == nil) != probe.ok {
				t.Fatalf("NewApprovalTransition = %v, want ok=%v", err, probe.ok)
			}
		})
	}
	// The kind is the record's, never the caller's.
	transition, err := NewApprovalTransition(approved, approverPublisher(approved))
	if err != nil {
		t.Fatal(err)
	}
	transition.Kind = ApprovalEventRefused
	if transition.Validate() == nil {
		t.Fatal("a row whose kind is not its record's state validated")
	}
}

type memoryOutbox struct {
	mu   sync.Mutex
	rows []StoredApprovalTransition
}

func (o *memoryOutbox) ApprovalTransitions(
	context.Context, []byte,
) ([]StoredApprovalTransition, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]StoredApprovalTransition(nil), o.rows...), nil
}

func (o *memoryOutbox) CompleteApprovalTransition(
	_ context.Context, transition ApprovalTransition,
	outcome ApprovalPublicationOutcome,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.rows {
		if o.rows[i].Transition.Record.Version == transition.Record.Version &&
			o.rows[i].Outcome == "" {
			o.rows[i].Outcome = outcome
		}
	}
	return nil
}

func (o *memoryOutbox) outcome(version uint64) ApprovalPublicationOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, row := range o.rows {
		if row.Transition.Record.Version == version {
			return row.Outcome
		}
	}
	return "absent"
}

// scriptedEvents answers MayPublish by publisher subject and fails Publish
// for the versions it is told to.
type scriptedEvents struct {
	caller    shoal.ID
	fail      map[uint64]bool
	published []uint64
}

func (e *scriptedEvents) PublishApprovalEvent(
	_ context.Context, transition ApprovalTransition,
) error {
	if e.fail[transition.Record.Version] {
		return shoal.NewError(shoal.ErrorUnavailable, "event backend down")
	}
	e.published = append(e.published, transition.Record.Version)
	return nil
}

func (e *scriptedEvents) MayPublishApprovalEvent(
	_ context.Context, transition ApprovalTransition,
) (bool, error) {
	return transition.Publisher.Subject == e.caller, nil
}

// TestDrainNeverBlocksOnAnotherRowAndLapsesPastTheWindow pins the drain's
// rules against a scripted outbox.
//
//   - Another principal's row is skipped and stays pending, and is never
//     published by the caller.
//   - A failure on an earlier row, even the caller's own, is not the answer
//     of a call that committed a new transition. That transition has landed.
//   - A replay answers for the caller's own failing row, because a replay is
//     the retry its failure asked for.
//   - Past its window, a row is closed lapsed by whoever drains, without
//     publishing and without error. That holds whoever's row it is, so a
//     principal that never returns cannot leave a row that fails forever.
func TestDrainNeverBlocksOnAnotherRowAndLapsesPastTheWindow(t *testing.T) {
	pending := validApprovalRecord(t)
	approved := decided(pending, ApprovalApproved, ApprovalVerdictApprove)
	approved.DecisionCorrelationID = "decision-correlation"
	materialized := approved
	materialized.Version, materialized.State = 3, ApprovalEnqueued
	materialized.MaterializedAt = approved.DecidedAt.Add(time.Minute)
	materialized.UpdatedAt = materialized.MaterializedAt
	row := func(record ApprovalRecord, publisher ApprovalPublisher) StoredApprovalTransition {
		transition, err := NewApprovalTransition(record, publisher)
		if err != nil {
			t.Fatal(err)
		}
		return StoredApprovalTransition{Transition: transition}
	}
	resolverFor := func(caller principalOf) auth.Resolver {
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject: caller.subject, Actor: caller.actor, ClientID: caller.client,
			OnBehalfOf: caller.onBehalfOf, RequestID: "drain",
			AuthorizationDomain:   []byte("domain"),
			AllowedOperations:     []auth.Operation{auth.OperationDispatch},
			PolicyGeneration:      3,
			AuthenticationExpires: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		resolver, err := auth.NewStaticResolver(decision)
		if err != nil {
			t.Fatal(err)
		}
		return resolver
	}
	alice := principalOf{subject: pending.Request.Subject,
		actor: pending.Request.Actor, client: pending.Request.ClientID,
		onBehalfOf: pending.Request.OnBehalfOf}
	bob := principalOf{subject: approved.ApproverSubject, actor: approved.ApproverActor}
	someoneElse := principalOf{subject: "someone-else", actor: "someone-else"}
	caller := alice
	build := func(now time.Time, events *scriptedEvents) (*ApprovalService, *memoryOutbox) {
		outbox := &memoryOutbox{rows: []StoredApprovalTransition{
			row(pending, requesterPublisher(pending)),
			row(approved, approverPublisher(approved)),
			row(materialized, requesterPublisher(materialized)),
		}}
		return &ApprovalService{
			dispatch: &DispatchService{
				clock:    func() time.Time { return now },
				resolver: resolverFor(caller),
			},
			events: &approvalEvents{outbox: outbox, publisher: events},
		}, outbox
	}
	inside := materialized.UpdatedAt.Add(time.Minute)

	// The requester's materialization commits v3 while the approver's v2 is
	// stranded and the requester's own v1 fails.
	events := &scriptedEvents{caller: "alice", fail: map[uint64]bool{1: true}}
	service, outbox := build(inside, events)
	if err := service.drainApprovalEvents(context.Background(), pending.ID, 3); err != nil {
		t.Fatalf("a committed transition answered for earlier rows: %v", err)
	}
	if len(events.published) != 1 || events.published[0] != 3 {
		t.Fatalf("published %v, want only the caller's own v3", events.published)
	}
	if outbox.outcome(2) != "" || outbox.outcome(1) != "" ||
		outbox.outcome(3) != ApprovalEventPublished {
		t.Fatalf("outcomes v1=%q v2=%q v3=%q", outbox.outcome(1),
			outbox.outcome(2), outbox.outcome(3))
	}
	if skipped, _, failed := service.ApprovalEventCounters(); skipped != 1 || failed != 1 {
		t.Fatalf("skipped %d, failed %d", skipped, failed)
	}
	// A replay answers for the caller's own failure, and never for another's.
	if err := service.drainApprovalEvents(
		context.Background(), pending.ID, 0,
	); !errors.Is(err, ErrApprovalEventUnpublished) {
		t.Fatalf("a replay over the caller's own failing row = %v", err)
	}
	caller = bob
	approverEvents := &scriptedEvents{caller: "bob", fail: map[uint64]bool{1: true}}
	service, _ = build(inside, approverEvents)
	if err := service.drainApprovalEvents(context.Background(), pending.ID, 0); err != nil {
		t.Fatalf("a replay answered for another principal's row: %v", err)
	}
	// And a failure on the caller's own new row is its answer, without
	// unwrapping into a sentinel the caller would read as a lost race.
	caller = alice
	events.fail[3] = true
	service, _ = build(inside, events)
	err := service.drainApprovalEvents(context.Background(), pending.ID, 3)
	if !errors.Is(err, ErrApprovalEventUnpublished) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("the caller's own failed row = %v", err)
	}

	// Past the window every pending row lapses, whoever drains it, with
	// nothing published and no error.
	past := pending.UpdatedAt.Add(ApprovalEventRetryWindow)
	caller = someoneElse
	nobody := &scriptedEvents{caller: "someone-else"}
	service, outbox = build(past.Add(ApprovalEventRetryWindow), nobody)
	if err := service.drainApprovalEvents(context.Background(), pending.ID, 0); err != nil {
		t.Fatalf("lapsing = %v", err)
	}
	for version := uint64(1); version <= 3; version++ {
		if outbox.outcome(version) != ApprovalEventLapsedOutcome {
			t.Fatalf("v%d = %q, want lapsed", version, outbox.outcome(version))
		}
	}
	if len(nobody.published) != 0 {
		t.Fatalf("a lapsed row was published: %v", nobody.published)
	}
	// At LapsesAt, a skew allowance before RetryUntil, a row is lapsed and
	// not published: a replica that far ahead may already have pruned the
	// token's dedup record, and publishing could duplicate the event.
	caller = alice
	edge := &scriptedEvents{caller: "alice"}
	service, outbox = build(pending.UpdatedAt.Add(
		ApprovalEventRetryWindow-ApprovalEventSkewAllowance), edge)
	if err := service.drainApprovalEvents(context.Background(), pending.ID, 0); err != nil {
		t.Fatal(err)
	}
	if outbox.outcome(1) != ApprovalEventLapsedOutcome {
		t.Fatalf("at the boundary v1 = %q", outbox.outcome(1))
	}
}

// TestApprovalEventWindowFitsTheBackendBound: a RetryUntil the event backend
// refuses would leave every row on a lagging replica unpublishable, so the
// window plus the skew it absorbs must be the backend's bound exactly. It is
// 24h, restated here because this package does not import fleetevents; the
// explorerfleetevents test holds it to the constant itself.
func TestApprovalEventWindowFitsTheBackendBound(t *testing.T) {
	if ApprovalEventRetryWindow+ApprovalEventSkewAllowance != 24*time.Hour {
		t.Fatalf("window %v + skew %v != 24h",
			ApprovalEventRetryWindow, ApprovalEventSkewAllowance)
	}
}

type principalOf struct {
	subject, actor, client shoal.ID
	onBehalfOf             []shoal.ID
}

// TestAReplayAnswersForTheCallersOwnRowWhateverFails: a replay is the retry
// an unpublished event asked for, so any failure on the caller's own row,
// including the publisher failing to answer whether it may publish, is the
// replay's answer. Answering success would leave the row pending with nobody
// else allowed to publish it. A row at the caller's own version that another
// principal wrote (two observers of one expiry) is not the caller's, and
// is never its answer.
func TestAReplayAnswersForTheCallersOwnRowWhateverFails(t *testing.T) {
	pending := validApprovalRecord(t)
	transition, err := NewApprovalTransition(pending, requesterPublisher(pending))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: pending.Request.Subject, Actor: pending.Request.Actor,
		ClientID: pending.Request.ClientID, OnBehalfOf: pending.Request.OnBehalfOf,
		RequestID: "drain", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{auth.OperationDispatch},
		PolicyGeneration:  3, AuthenticationExpires: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := auth.NewStaticResolver(decision)
	if err != nil {
		t.Fatal(err)
	}
	service := &ApprovalService{
		dispatch: &DispatchService{
			clock:    func() time.Time { return pending.UpdatedAt.Add(time.Minute) },
			resolver: resolver,
		},
		events: &approvalEvents{
			outbox:    &memoryOutbox{rows: []StoredApprovalTransition{{Transition: transition}}},
			publisher: undecidedEvents{},
		},
	}
	if err := service.drainApprovalEvents(
		context.Background(), pending.ID, 0,
	); !errors.Is(err, ErrApprovalEventUnpublished) {
		t.Fatalf("a replay whose own row could not be asked about = %v", err)
	}
	// The same expiry at the caller's own version, written first, byte for
	// byte, by an approver who observed it too: the transition is theirs.
	expired := pending
	expired.Version, expired.State = 2, ApprovalExpired
	expired.UpdatedAt = pending.ExpiresAt.Add(time.Minute)
	theirs, err := NewApprovalTransition(expired, ApprovalPublisher{
		Operation: auth.OperationActionApprove, Subject: "carol", Actor: "carol",
		RequestID: "carol-request", CorrelationID: "correlation",
		AuthorizationFingerprint: auth.Fingerprint{5},
		AuthorizationExpiresAt:   pending.RequestedAt.Add(time.Hour),
		Observer:                 ApprovalObservedByApprover,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.events.outbox = &memoryOutbox{rows: []StoredApprovalTransition{{Transition: theirs}}}
	if err := service.drainApprovalEvents(context.Background(), pending.ID, 2); err != nil {
		t.Fatalf("another principal's row at the caller's version = %v", err)
	}
	// A committed version whose row this read cannot see yet is answered,
	// not reported as published: only its maker may publish it.
	if err := service.drainApprovalEvents(
		context.Background(), pending.ID, 3,
	); !errors.Is(err, ErrApprovalEventUnpublished) {
		t.Fatalf("a committed transition whose row is not readable = %v", err)
	}
}

type undecidedEvents struct{}

func (undecidedEvents) PublishApprovalEvent(context.Context, ApprovalTransition) error {
	return nil
}

func (undecidedEvents) MayPublishApprovalEvent(
	context.Context, ApprovalTransition,
) (bool, error) {
	return false, shoal.NewError(shoal.ErrorUnavailable, "authorization lookup failed")
}
