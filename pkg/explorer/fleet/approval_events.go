// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Approval lifecycle events (#653).
//
// Every compare-and-set on an approval record is a transition, and each one is
// published as one event, under the identity of the principal that made it.
// Before this, approval transitions were audited and published nothing.
// Publishing them was blocked on #480's mixed-identity wedge: dispatch
// publishes an action's pending transitions in order under one caller's
// decision, so an outbox holding rows from two principals could not be
// drained by either of them.
//
// Approvals avoid that in the shape of the outbox, not by changing who may
// publish. The outbox is **per transition**:
//
//   - The approval store writes one outbox row per transition (approval ID,
//     version) in the same coordination publish as the approval row itself.
//     A committed transition therefore always has its row. There is no window
//     between the two writes for a crash to fall into.
//   - The row names its publisher: the decision that made the transition,
//     with its operation, identity, request, correlation and fingerprint. Only
//     that principal may publish it, under its own live decision. That is the
//     same identity rule dispatch applies (publisherMatchesTransition), applied
//     to the row's stored publisher rather than inferred from the record. The
//     requester's rows are therefore never in the approver's way, and the
//     approver's rows are never in the requester's outbox.
//   - Rows are independent. A row that is not the caller's is skipped. A
//     failure on a row this call did not commit is never this call's error.
//     So a stranded row cannot block a later transition (the #480 item 3
//     shape) or its answer.
//   - No row can wedge forever (#650's review). An event's publication token
//     is deduplicated for ApprovalEventRetryWindow after the transition. A row
//     still pending past that window is marked lapsed by whoever next drains
//     the approval, whoever they are, because marking publishes nothing.
//     Publishing it then could duplicate an event whose first publication
//     landed and whose completion did not. A lapsed row is terminal and never
//     refused 409 forever.
var (
	// ErrApprovalEventUnpublished reports an approval transition that
	// committed but whose event the caller could not publish. Nothing
	// external happened, so it is not an indeterminate commit. A retry of
	// the same call replays the transition and publishes its event.
	ErrApprovalEventUnpublished = errors.New(
		"fleet approval: transition committed; its event is not yet published")
	// ErrApprovalEventLapsed reports an approval event row whose publication
	// window has closed. It is never returned to a caller: the drain marks
	// the row lapsed.
	ErrApprovalEventLapsed = errors.New(
		"fleet approval: event publication window has closed")
)

// approvalEventUnpublished reports a committed transition whose event did not
// publish. It wraps the sentinel and only the sentinel. The cause is named
// but deliberately not unwrappable, because callers branch on sentinels such
// as ErrApprovalConflict to mean "the transition lost a race and must be
// re-evaluated". A conflict closing an outbox row must never read as that:
// the transition has landed.
func approvalEventUnpublished(cause error) error {
	return shoal.WrapError(
		shoal.ErrorUnavailable,
		"approval transition committed; its event is not yet published: "+
			cause.Error(),
		ErrApprovalEventUnpublished)
}

// ApprovalEventRetryWindow is how long after its transition an approval event
// may still be published. The event backend deduplicates a publication token
// until its RetryUntil and refuses a RetryUntil more than
// fleetevents.MaxMutationRetryWindow (24h) ahead of its own clock.
//
// The window is an hour shorter than that bound, and the hour is clock skew.
// A transition written on a replica behind the one that wrote the row before
// it clamps its UpdatedAt forward (see notBefore). A window of the full 24h
// would then put RetryUntil past the bound on that replica's clock, and the
// event service would refuse the publication. The approver's committed
// decision would answer "not yet published" for as long as the skew lasted.
// Dispatch has exactly that property (docs/approval.md, "Clock skew"). Here
// up to an hour of skew is absorbed instead, at the cost of an hour of the
// window. A test in internal/explorerfleetevents holds the window within the
// bound.
const (
	ApprovalEventRetryWindow   = 23 * time.Hour
	ApprovalEventSkewAllowance = time.Hour
)

// Approval event kinds, one per stored state. The state an approval record
// is in after a transition names that transition exactly, so the kind is a
// function of the record and never chosen by the caller.
const (
	ApprovalEventRequested    = "approval.requested"
	ApprovalEventApproved     = "approval.approved"
	ApprovalEventRefused      = "approval.refused"
	ApprovalEventExpired      = "approval.expired"
	ApprovalEventMaterialized = "approval.materialized"
	// Withdrawal and stranding (#655).
	ApprovalEventWithdrawn = "approval.withdrawn"
	ApprovalEventStranded  = "approval.stranded"
)

// ApprovalEventKind is the event a record in this state was written by.
func ApprovalEventKind(state ApprovalState) (string, bool) {
	switch state {
	case ApprovalPending:
		return ApprovalEventRequested, true
	case ApprovalApproved:
		return ApprovalEventApproved, true
	case ApprovalRefused:
		return ApprovalEventRefused, true
	case ApprovalExpired:
		return ApprovalEventExpired, true
	case ApprovalEnqueued:
		return ApprovalEventMaterialized, true
	case ApprovalWithdrawn:
		return ApprovalEventWithdrawn, true
	case ApprovalStranded:
		return ApprovalEventStranded, true
	default:
		return "", false
	}
}

// ApprovalObserver is which side's call wrote an expiry or a stranding.
//
// Neither has an acting principal. Nobody decides that a window has closed,
// or that a request's target has moved; it happened. Each is written by
// whichever call observed it: a requester's Request or Status, an approver's
// Decide or Status, or the host's sweep (#655). That writer is the
// transition's publisher and the row says which it was. The event carries it
// in its reason code, so a subscriber can tell an expiry observed on the
// requester's return from one observed by an approver or by the sweep.
type ApprovalObserver string

const (
	ApprovalObservedByRequester ApprovalObserver = "requester"
	ApprovalObservedByApprover  ApprovalObserver = "approver"
	// ApprovalObservedBySweeper is the host's system observer
	// (ApprovalSweeperSubject), which is neither party.
	ApprovalObservedBySweeper ApprovalObserver = "sweeper"
)

// ApprovalExpiryReasonCode is the event reason code for an expiry, by observer.
func ApprovalExpiryReasonCode(observer ApprovalObserver) string {
	return "approval.expiry.observed_by_" + string(observer)
}

// ApprovalStrandReasonCode is the event reason code for a stranding, by
// cause and observer (#655).
func ApprovalStrandReasonCode(
	cause ApprovalCondition, observer ApprovalObserver,
) string {
	return "approval.stranded." + string(cause) + ".observed_by_" + string(observer)
}

// ApprovalPublisher is the principal that made one approval transition, and
// the authorization it made it under. It is stored on the transition's outbox
// row, so the event is published by exactly this principal and audited with
// exactly this request's provenance.
type ApprovalPublisher struct {
	Operation                auth.Operation
	Subject                  shoal.ID
	Actor                    shoal.ID
	ClientID                 shoal.ID
	OnBehalfOf               []shoal.ID
	RequestID                shoal.ID
	CorrelationID            shoal.ID
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
	// Observer is set on an expiry and only there.
	Observer ApprovalObserver
}

// Set reports whether a publisher is named. A mutation without one writes no
// outbox row. Memory stores in tests, and every path that is not a
// transition, carry none.
func (p ApprovalPublisher) Set() bool {
	return p.Operation != ""
}

func cloneApprovalPublisher(p ApprovalPublisher) ApprovalPublisher {
	p.OnBehalfOf = append([]shoal.ID(nil), p.OnBehalfOf...)
	return p
}

// approvalPublisherFor names the decision that is making a transition.
func approvalPublisherFor(
	decision auth.Decision, operation auth.Operation, phase string,
) (ApprovalPublisher, error) {
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return ApprovalPublisher{}, err
	}
	publisher := ApprovalPublisher{
		Operation: operation, Subject: decision.Subject(),
		Actor: decision.Actor(), ClientID: decision.ClientID(),
		OnBehalfOf:               decision.OnBehalfOf(),
		RequestID:                decision.RequestID(),
		CorrelationID:            decision.CorrelationID(),
		AuthorizationFingerprint: fingerprint,
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	if phase == "approval_expiry" || phase == "approval_strand" {
		switch operation {
		case auth.OperationActionApprove:
			publisher.Observer = ApprovalObservedByApprover
		case auth.OperationApprovalSweep:
			publisher.Observer = ApprovalObservedBySweeper
		default:
			publisher.Observer = ApprovalObservedByRequester
		}
	}
	return publisher, nil
}

// ApprovalTransition is one approval transition's outbox row: the record as
// that transition wrote it, and who wrote it.
type ApprovalTransition struct {
	Kind      string
	Record    ApprovalRecord
	Publisher ApprovalPublisher
}

// NewApprovalTransition builds the outbox row for a record a transition is
// writing.
func NewApprovalTransition(
	record ApprovalRecord, publisher ApprovalPublisher,
) (ApprovalTransition, error) {
	kind, ok := ApprovalEventKind(record.State)
	if !ok {
		return ApprovalTransition{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval state is invalid")
	}
	transition := ApprovalTransition{
		Kind: kind, Record: CloneApprovalRecord(record),
		Publisher: cloneApprovalPublisher(publisher),
	}
	if err := transition.Validate(); err != nil {
		return ApprovalTransition{}, err
	}
	return transition, nil
}

// CloneApprovalTransition returns an independent copy.
func CloneApprovalTransition(t ApprovalTransition) ApprovalTransition {
	t.Record = CloneApprovalRecord(t.Record)
	t.Publisher = cloneApprovalPublisher(t.Publisher)
	return t
}

// RetryUntil is the event's publication deadline as the event backend sees
// it: the backend deduplicates its token until then.
func (t ApprovalTransition) RetryUntil() time.Time {
	return t.Record.UpdatedAt.Add(ApprovalEventRetryWindow)
}

// LapsesAt is when the row stops being published: the skew allowance before
// RetryUntil. The backend prunes a token's dedup record by the clock of
// whichever replica next appends. A replica running ahead could prune it
// before RetryUntil by this replica's clock, and a publication retried in
// between would be appended a second time. Stopping an allowance early means
// a replica up to that far ahead cannot have pruned it yet.
func (t ApprovalTransition) LapsesAt() time.Time {
	return t.RetryUntil().Add(-ApprovalEventSkewAllowance)
}

// Validate checks that a row describes a transition that could have been made,
// by the principal that could have made it.
//
// The publisher is bound to the record, not merely carried beside it. Each
// kind has exactly one principal it may be published by:
//
//   - requested and materialized: the requester, under dispatch, whose
//     identity is the stored request's;
//   - approved and refused: the approver the record names, under approve,
//     with the decision's own request ID;
//   - expired and stranded: whichever side observed it. The requester is
//     under dispatch, with the request's identity. An approver is under
//     approve, not on anybody's behalf. The sweep is the system observer,
//     under approval_sweep, as ApprovalSweeperSubject (#655);
//   - withdrawn: the requester, under dispatch (#655).
//
// A row that names any other publisher is refused by the store and by the
// publisher. That is what makes "published under the identity of the
// principal that made it" a property of the data. It does not depend on
// every call site getting it right.
func (t ApprovalTransition) Validate() error {
	if err := t.Record.Validate(); err != nil {
		return err
	}
	kind, ok := ApprovalEventKind(t.Record.State)
	if !ok || kind != t.Kind {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval event kind does not match the record's state")
	}
	p := t.Publisher
	if !p.Set() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval event publisher is required")
	}
	if err := shoal.ValidateRequiredID("approval event publisher", p.Subject); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"approval event publisher actor", p.Actor); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"approval event request ID", p.RequestID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"approval event correlation ID", p.CorrelationID); err != nil {
		return err
	}
	if p.AuthorizationFingerprint == (auth.Fingerprint{}) ||
		p.AuthorizationExpiresAt.IsZero() ||
		p.AuthorizationExpiresAt.Location() != time.UTC {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval event publisher authorization is incomplete")
	}
	requester := func() bool {
		request := t.Record.Request
		return p.Operation == auth.OperationDispatch &&
			p.Subject == request.Subject && p.Actor == request.Actor &&
			p.ClientID == request.ClientID &&
			sameIdentityChain(p.OnBehalfOf, request.OnBehalfOf)
	}
	var valid bool
	switch t.Kind {
	case ApprovalEventRequested, ApprovalEventMaterialized, ApprovalEventWithdrawn:
		valid = p.Observer == "" && requester()
	case ApprovalEventApproved, ApprovalEventRefused:
		valid = p.Observer == "" &&
			p.Operation == auth.OperationActionApprove &&
			p.Subject == t.Record.ApproverSubject &&
			p.Actor == t.Record.ApproverActor &&
			p.ClientID == t.Record.ApproverClientID &&
			len(p.OnBehalfOf) == 0 &&
			p.RequestID == t.Record.DecisionRequestID &&
			p.CorrelationID == t.Record.DecisionCorrelationID &&
			p.AuthorizationFingerprint == t.Record.ApproverFingerprint
	case ApprovalEventExpired, ApprovalEventStranded:
		switch p.Observer {
		case ApprovalObservedByRequester:
			valid = requester()
		case ApprovalObservedByApprover:
			// The record names no observer, so eligibility cannot be
			// rechecked from the data. What can be is that the approver is
			// not the requester under another operation, and not the
			// system observer under a party's operation.
			request := t.Record.Request
			valid = p.Operation == auth.OperationActionApprove &&
				len(p.OnBehalfOf) == 0 &&
				p.Subject != request.Subject && p.Subject != request.Actor &&
				p.Actor != request.Subject && p.Actor != request.Actor &&
				p.Subject != ApprovalSweeperSubject &&
				p.Actor != ApprovalSweeperSubject
		case ApprovalObservedBySweeper:
			// The system observer and nothing else: its identity, its
			// operation, on nobody's behalf (#655).
			valid = p.Operation == auth.OperationApprovalSweep &&
				p.Subject == ApprovalSweeperSubject &&
				p.Actor == ApprovalSweeperSubject &&
				p.ClientID == ApprovalSweeperClientID &&
				len(p.OnBehalfOf) == 0
		}
	}
	if !valid {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval event publisher is not the principal that made the transition")
	}
	return nil
}

func sameIdentityChain(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !bytes.Equal([]byte(left[i]), []byte(right[i])) {
			return false
		}
	}
	return true
}

// ApprovalPublicationOutcome is how an approval event row was closed.
type ApprovalPublicationOutcome string

const (
	// ApprovalEventPublished: the event was published (or its publication
	// was confirmed as a repeat) and the row acknowledged.
	ApprovalEventPublished ApprovalPublicationOutcome = "published"
	// ApprovalEventLapsedOutcome: the row was still pending when its
	// publication window closed. It is not necessarily lost. A publication
	// that landed and whose acknowledgement did not also lapses. It is no
	// longer publishable without risking a duplicate.
	ApprovalEventLapsedOutcome ApprovalPublicationOutcome = "lapsed"
)

// ApprovalTransitionStore is the approval outbox. The approval store
// implements it over the same rows it writes in ApplyApproval.
type ApprovalTransitionStore interface {
	// ApprovalTransitions returns every outbox row of one approval, in
	// version order, with the outcome each was closed with, if any.
	ApprovalTransitions(
		context.Context, []byte,
	) ([]StoredApprovalTransition, error)
	// CompleteApprovalTransition closes a pending row with an outcome. It is
	// idempotent, and closing an already-closed row is not an error.
	CompleteApprovalTransition(
		context.Context, ApprovalTransition, ApprovalPublicationOutcome,
	) error
}

// StoredApprovalTransition is an outbox row with its outcome.
type StoredApprovalTransition struct {
	Transition ApprovalTransition
	Outcome    ApprovalPublicationOutcome
	ClosedAt   time.Time
}

// ApprovalEventPublisher publishes approval transition events.
type ApprovalEventPublisher interface {
	// PublishApprovalEvent publishes one row under the caller's decision. It
	// refuses a caller that is not the row's publisher. Past the row's window
	// it returns ErrApprovalEventLapsed without publishing.
	PublishApprovalEvent(context.Context, ApprovalTransition) error
	// MayPublishApprovalEvent reports whether this caller is the row's
	// publisher and still holds its operation on the scope. A false answer
	// means "not yours"; an error means the question failed.
	MayPublishApprovalEvent(context.Context, ApprovalTransition) (bool, error)
}

// approvalEvents is the service's optional event wiring.
type approvalEvents struct {
	outbox    ApprovalTransitionStore
	publisher ApprovalEventPublisher
	// skipped and lapsed count rows a drain left for their own publisher and
	// rows it closed as lapsed. Counted, not logged, as dispatch's
	// skippedPublications is (#642).
	skipped atomic.Uint64
	lapsed  atomic.Uint64
	failed  atomic.Uint64
}

// ApprovalEventCounters reports how many approval event rows drains have
// skipped as another principal's, closed as lapsed, and failed to publish
// without that failure being the caller's own.
func (s *ApprovalService) ApprovalEventCounters() (skipped, lapsed, failed uint64) {
	if s.events == nil {
		return 0, 0, 0
	}
	return s.events.skipped.Load(), s.events.lapsed.Load(), s.events.failed.Load()
}

// drainApprovalEvents publishes this approval's pending event rows that the
// caller made, and closes those whose window has passed.
//
// own is the version this call has just committed, or zero for a call that
// committed nothing (a replay, which is how a principal reconciles its own
// earlier transition). Which rows are the caller's is decided by the row's
// stored publisher, compared with the caller's decision, before anything is
// asked of the publisher. "Not yours" is therefore an identity fact, never
// an inference from an error. The error rule is what keeps a stranded row
// from blocking anything:
//
//   - A call that committed a transition answers only for that transition.
//     A failure on any earlier row, even one of the caller's own, is counted
//     and left pending. The new transition has landed and its answer must
//     say so. If the row it just wrote cannot be read yet, that is answered
//     too, because a committed event must not go silently unpublished.
//   - A replay answers for every failure on the caller's own pending rows,
//     including a failed outbox read or a publisher that could not answer.
//     A replay is the retry a failed publication asked for, and answering
//     success while the row stays pending would lose the event: nobody else
//     may publish it. A replay never answers for anyone else's row.
//
// Either way every row is attempted. A row is never published under anyone
// but its publisher.
func (s *ApprovalService) drainApprovalEvents(
	ctx context.Context, id []byte, own uint64,
) error {
	if s.events == nil {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	decision, err := s.dispatch.resolver.Resolve(ctx)
	if err != nil {
		return approvalEventUnpublished(err)
	}
	rows, err := s.events.outbox.ApprovalTransitions(ctx, id)
	if err != nil {
		return approvalEventUnpublished(err)
	}
	now := s.dispatch.clock().UTC()
	var answer error
	ownSeen := own == 0
	for _, row := range rows {
		if own != 0 && row.Transition.Record.Version == own {
			ownSeen = true
		}
		if row.Outcome != "" {
			continue
		}
		transition := row.Transition
		if !now.Before(transition.LapsesAt()) {
			// Anyone may close a lapsed row: closing it publishes nothing
			// and attributes nothing, so the identity rule has nothing to
			// protect here, and requiring the publisher would recreate
			// #650's wedge for a principal who never comes back.
			if err := s.events.outbox.CompleteApprovalTransition(
				ctx, transition, ApprovalEventLapsedOutcome); err == nil {
				s.events.lapsed.Add(1)
			}
			continue
		}
		if !publishedBy(decision, transition.Publisher) {
			// Another principal's row. That includes a row at this call's own
			// version whose transition another caller wrote byte for byte
			// first (two observers of one expiry): the transition is theirs.
			s.events.skipped.Add(1)
			continue
		}
		mine := own != 0 && transition.Record.Version == own
		answerable := mine || own == 0
		may, err := s.events.publisher.MayPublishApprovalEvent(ctx, transition)
		if err == nil && !may {
			// The caller made it but no longer holds its operation on the
			// scope. No retry by this caller can publish it, so it is not an
			// answer; it lapses.
			s.events.skipped.Add(1)
			continue
		}
		if err == nil {
			err = s.events.publisher.PublishApprovalEvent(ctx, transition)
		}
		if errors.Is(err, ErrApprovalEventLapsed) {
			if completeErr := s.events.outbox.CompleteApprovalTransition(
				ctx, transition, ApprovalEventLapsedOutcome); completeErr == nil {
				s.events.lapsed.Add(1)
			}
			continue
		}
		if err == nil {
			err = s.events.outbox.CompleteApprovalTransition(
				ctx, transition, ApprovalEventPublished)
		}
		if err == nil {
			continue
		}
		if answerable {
			if answer == nil {
				answer = approvalEventUnpublished(err)
			}
			continue
		}
		s.events.failed.Add(1)
	}
	if answer == nil && !ownSeen {
		// The transition committed with its row in the same write, so a row
		// this read cannot see is a read that has not caught up, not an
		// absent row. Saying nothing would let a committed event go
		// unpublished until some later call. The retry this asks for
		// replays the transition and finds it.
		answer = approvalEventUnpublished(errors.New(
			"the transition's event row is not yet readable"))
	}
	return answer
}

// drainApprovalEventsQuietly is a drain that answers for nothing: the caller
// is not the call that owes any row's event, so a failure is counted and the
// row left for its own retry or its lapse (#655).
func (s *ApprovalService) drainApprovalEventsQuietly(
	ctx context.Context, id []byte,
) {
	if err := s.drainApprovalEvents(ctx, id, 0); err != nil && s.events != nil {
		s.events.failed.Add(1)
	}
}

// publishedBy reports whether a decision is the principal a row names.
func publishedBy(decision auth.Decision, publisher ApprovalPublisher) bool {
	return decision.Subject() == publisher.Subject &&
		decision.Actor() == publisher.Actor &&
		decision.ClientID() == publisher.ClientID &&
		sameIdentityChain(decision.OnBehalfOf(), publisher.OnBehalfOf)
}
