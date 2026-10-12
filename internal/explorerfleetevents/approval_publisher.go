// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ApprovalEventPublisher publishes approval transition events (#653) through
// the trusted lifecycle route, each under the identity of the principal that
// made the transition.
//
// It applies the gate ActionEventPublisher applies, which is the publisher
// identity match and the object authorization under the transition's own
// operation. The difference is where the identity comes from. An approval
// outbox row names its publisher explicitly (fleet.ApprovalPublisher, bound
// to the record by ApprovalTransition.Validate), so there is no per-kind
// inference from the record's fields to get wrong.
type ApprovalEventPublisher struct {
	service    *fleetevents.Service
	resolver   auth.Resolver
	capability explorerfleetcap.Capability
	now        func() time.Time
}

var _ fleet.ApprovalEventPublisher = (*ApprovalEventPublisher)(nil)

// Approvals returns the approval event publisher over the same event service,
// resolver, lifecycle capability and clock as this action publisher. The
// capability is private to composition, so this is how a host obtains one.
func (p *ActionEventPublisher) Approvals() *ApprovalEventPublisher {
	return &ApprovalEventPublisher{
		service: p.service, resolver: p.resolver,
		capability: p.reconcile, now: p.now,
	}
}

func (p *ApprovalEventPublisher) PublishApprovalEvent(
	ctx context.Context, transition fleet.ApprovalTransition,
) error {
	request, receipt, err := ApprovalPublication(transition)
	if err != nil {
		return err
	}
	now := p.now().UTC()
	if !now.Before(transition.LapsesAt()) {
		return fleet.ErrApprovalEventLapsed
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	if err := authorizeApprovalTransition(decision, transition, now); err != nil {
		return err
	}
	_, err = p.service.PublishLifecycle(
		ctx, p.capability, transition.Publisher.Operation, request, receipt)
	if errors.Is(err, fleetevents.ErrPublicationExpired) {
		// The window closed between the check above and the event service's
		// own. The same answer, so the drain closes the row.
		return errors.Join(fleet.ErrApprovalEventLapsed, err)
	}
	return err
}

func (p *ApprovalEventPublisher) MayPublishApprovalEvent(
	ctx context.Context, transition fleet.ApprovalTransition,
) (bool, error) {
	if err := transition.Validate(); err != nil {
		return false, err
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return false, err
	}
	if err := authorizeApprovalTransition(
		decision, transition, p.now().UTC()); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// authorizeApprovalTransition admits exactly the principal the row names,
// still holding the transition's operation on the request's scope.
func authorizeApprovalTransition(
	decision auth.Decision, transition fleet.ApprovalTransition, now time.Time,
) error {
	publisher := transition.Publisher
	if decision.Subject() != publisher.Subject ||
		decision.Actor() != publisher.Actor ||
		decision.ClientID() != publisher.ClientID ||
		!sameIDs(decision.OnBehalfOf(), publisher.OnBehalfOf) {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"fleet approval event authorization does not match durable transition")
	}
	request := transition.Record.Request
	return decision.AuthorizeObject(publisher.Operation, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            request.SourceID, PolicyID: request.PolicyID,
		ObjectID: request.ObjectID,
	}, now)
}

// ApprovalPublication derives the one publication an approval transition
// produces. It is a function of the durable row alone, so the publisher and
// the ledger cannot disagree about what the event of a transition is.
//
// The event carries less than the approval routes show the parties who may
// receive it: no input, request digest, approver identity, grant provenance,
// correlation ID or request reason. Its authorization join is the request's source, policy and
// target object, as the action events of the work it may become are. That
// join is the scope's, which is why delivery also asks the approval's own
// concealment rule (fleetevents.ApprovalVisibility).
//
//   - Token and TransitionID name (kind, approval, version), so one transition
//     is one publication, whatever drain delivers it and however often.
//   - OccurredAt is when the transition took effect. For an expiry that is
//     ExpiresAt, the moment the window closed. The row's UpdatedAt is the
//     later moment someone observed it, and the reason code says who
//     (fleet.ApprovalExpiryReasonCode). The publication audit names the
//     observer's request.
//   - RetryUntil is the row's UpdatedAt plus the backend's dedup window,
//     fixed by the row. Past it the row lapses rather than republishing.
func ApprovalPublication(
	transition fleet.ApprovalTransition,
) (fleetevents.PublishRequest, fleetevents.LifecycleReceipt, error) {
	if err := transition.Validate(); err != nil {
		return fleetevents.PublishRequest{}, fleetevents.LifecycleReceipt{}, err
	}
	record := transition.Record
	request := record.Request
	var version [8]byte
	binary.BigEndian.PutUint64(version[:], record.Version)

	tokenHash := sha256.New()
	_, _ = tokenHash.Write([]byte("shoal-fleet-approval-event-publication-v1"))
	writeTokenField(tokenHash, []byte(transition.Kind))
	writeTokenField(tokenHash, record.ID)
	writeTokenField(tokenHash, version[:])

	transitionHash := sha256.New()
	_, _ = transitionHash.Write([]byte("shoal-fleet-approval-transition-id-v1"))
	writeTokenField(transitionHash, []byte(transition.Kind))
	writeTokenField(transitionHash, record.ID)
	writeTokenField(transitionHash, version[:])
	writeTokenField(transitionHash, record.RequestDigest)

	// No reason, except the observer's code on an expiry and a stranding.
	// The request's reason (a code and a digest of its free-form detail) is
	// not on the approval wire, and its digest would let an approver confirm
	// a guessed detail. The decision's reason is not stored. A withdrawal's
	// reason is the requester's and is withheld for the same reason (#655).
	// A stranding's cause is what Status reports to the same audience as
	// its condition, so the code names it.
	occurred := record.UpdatedAt
	var reason interaction.Reason
	switch transition.Kind {
	case fleet.ApprovalEventExpired:
		occurred = record.ExpiresAt
		reason = interaction.Reason{
			Code: fleet.ApprovalExpiryReasonCode(transition.Publisher.Observer),
		}
	case fleet.ApprovalEventStranded:
		reason = interaction.Reason{
			Code: fleet.ApprovalStrandReasonCode(
				record.StrandedCause, transition.Publisher.Observer),
		}
	}
	publisher := transition.Publisher
	publication := fleetevents.PublishRequest{
		Token:      tokenHash.Sum(nil),
		RetryUntil: transition.RetryUntil(),
		Event: fleetevents.Event{
			Kind:               transition.Kind,
			ProducerID:         []byte(request.AgentID),
			ProducerGeneration: request.AgentGeneration,
			ActionID:           append([]byte(nil), record.ID...),
			TransitionID:       transitionHash.Sum(nil),
			// No correlation ID. The publisher's is the other party's to
			// approvers and requesters alike, and the approval wire shows
			// neither side the other's. The publication audit still carries
			// the publisher's, stamped by the trusted sink from its live
			// decision (#532).
			Reason: reason,
			Evidence: []fleetevents.Evidence{{
				SourceID: append([]byte(nil), request.SourceID...),
				PolicyID: append([]byte(nil), request.PolicyID...),
				ObjectID: request.ObjectID,
			}},
			OccurredAt: occurred,
		},
	}
	receipt := fleetevents.LifecycleReceipt{
		RequestID:                publisher.RequestID,
		AuthorizationFingerprint: publisher.AuthorizationFingerprint,
		AuthorizationExpiresAt:   publisher.AuthorizationExpiresAt,
	}
	return publication, receipt, nil
}

// ApprovalLedger confirms trusted approval publications against the durable
// approval outbox (fleetevents.ApprovalLedger).
type ApprovalLedger struct {
	transitions fleet.ApprovalTransitionStore
}

var _ fleetevents.ApprovalLedger = (*ApprovalLedger)(nil)

func NewApprovalLedger(
	transitions fleet.ApprovalTransitionStore,
) (*ApprovalLedger, error) {
	if transitions == nil {
		return nil, errors.New("fleet events: approval transition store is required")
	}
	return &ApprovalLedger{transitions: transitions}, nil
}

// VerifyApprovalEvent admits a publication only if it is, field for field,
// the publication of a committed transition of the approval it names, under
// that transition's operation and receipt. A closed row still verifies: a
// repeat of a publication that landed is the same publication.
func (l *ApprovalLedger) VerifyApprovalEvent(
	ctx context.Context, operation auth.Operation,
	request fleetevents.PublishRequest, receipt fleetevents.LifecycleReceipt,
) error {
	if len(request.Event.ActionID) == 0 ||
		len(request.Event.ActionID) > fleet.MaxActionIDBytes {
		return approvalEventNotDurable()
	}
	rows, err := l.transitions.ApprovalTransitions(ctx, request.Event.ActionID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		derived, derivedReceipt, err := ApprovalPublication(row.Transition)
		if err != nil {
			return err
		}
		if !bytes.Equal(derived.Token, request.Token) {
			continue
		}
		if operation != row.Transition.Publisher.Operation ||
			!reflect.DeepEqual(derived, request) ||
			!reflect.DeepEqual(derivedReceipt, receipt) {
			return approvalEventNotDurable()
		}
		return nil
	}
	return approvalEventNotDurable()
}

func approvalEventNotDurable() error {
	return shoal.NewError(
		shoal.ErrorUnauthorized,
		"fleet approval event does not describe a durable transition")
}
