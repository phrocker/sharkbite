/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package fleetevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Config struct {
	Backend          Backend
	Resolver         auth.Resolver
	GenerationReader auth.GenerationReader
	LeaseValidator   LeaseValidator
	Auditor          Auditor
	CursorKey        []byte
	CursorTTL        time.Duration
	Clock            func() time.Time
	PollInterval     time.Duration
	MaxWait          time.Duration
	// EvidenceVisibility decides, at delivery, whether the subscriber
	// pulling an event may see an exact evidence reference that carries a
	// visibility expression (#562).
	//
	// The same seam as fleet.DispatchConfig.EvidenceVisibility — one type,
	// defined once in evidencelabels — and the same semantics: optional, and
	// nil means no subscriber may see labelled evidence, so labelled
	// references are withheld from every delivered envelope while unlabelled
	// ones are delivered unchanged.
	EvidenceVisibility evidencelabels.Visibility
	// EvidenceNodes decides, at delivery, a reference that names nodes by
	// those nodes' current access rules (#564), as the dispatch reads do.
	// Nil withholds every such reference from every subscriber.
	EvidenceNodes evidencelabels.NodeGate
	// Approvals confirms each trusted approval.* publication against the
	// durable approval transition it claims to describe (#653). Nil admits
	// no approval event at all.
	Approvals ApprovalLedger
	// ApprovalVisibility is the delivery-time concealment rule for
	// approval.* events (#653). Nil delivers them to nobody.
	ApprovalVisibility ApprovalVisibility
}

type Service struct {
	backend     Backend
	resolver    auth.Resolver
	generations auth.GenerationReader
	leases      LeaseValidator
	auditor     Auditor
	cursors     cursorCodec
	now         func() time.Time
	poll        time.Duration
	maxWait     time.Duration
	reconcile   explorerfleetcap.Capability
	// evidenceVisibility is asked under the subscriber's context at
	// delivery, never under the publisher's at publish.
	evidenceVisibility evidencelabels.Visibility
	evidenceNodes      evidencelabels.NodeGate
	approvals          ApprovalLedger
	approvalVisibility ApprovalVisibility
}

func New(config Config) (*Service, error) {
	return newService(config, explorerfleetcap.Capability{})
}

// NewWithLifecycleCapability constructs a service whose trusted lifecycle
// publication path is available only to the host that owns capability.
func NewWithLifecycleCapability(
	config Config, capability explorerfleetcap.Capability,
) (*Service, error) {
	if !capability.Valid() {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet lifecycle capability is required")
	}
	return newService(config, capability)
}

func newService(
	config Config, capability explorerfleetcap.Capability,
) (*Service, error) {
	if nilDependency(config.Backend) || nilDependency(config.Resolver) ||
		nilDependency(config.GenerationReader) ||
		nilDependency(config.LeaseValidator) ||
		nilDependency(config.Auditor) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet event dependencies are required")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.CursorTTL == 0 {
		config.CursorTTL = DefaultCursorTTL
	}
	if config.PollInterval == 0 {
		config.PollInterval = 50 * time.Millisecond
	}
	if config.MaxWait == 0 {
		config.MaxWait = MaxLongPollWait
	}
	if config.PollInterval < time.Millisecond || config.MaxWait < config.PollInterval ||
		config.MaxWait > MaxLongPollWait {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet event wait bounds are invalid")
	}
	codec, err := newCursorCodec(config.CursorKey, config.CursorTTL)
	if err != nil {
		return nil, shoal.WrapError(shoal.ErrorInvalidArgument, "fleet event cursor configuration", err)
	}
	return &Service{
		backend: config.Backend, resolver: config.Resolver, generations: config.GenerationReader,
		leases: config.LeaseValidator, auditor: config.Auditor, cursors: codec,
		now: config.Clock, poll: config.PollInterval, maxWait: config.MaxWait,
		reconcile: capability, evidenceVisibility: config.EvidenceVisibility,
		evidenceNodes: config.EvidenceNodes, approvals: config.Approvals,
		approvalVisibility: config.ApprovalVisibility,
	}, nil
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Subscription, error) {
	now := s.now().UTC()
	if err := validateRetryUntil(request.RetryUntil, now, ErrMutationExpired); err != nil {
		return Subscription{}, err
	}
	decision, guard, err := s.authorize(ctx, auth.OperationSubscriptionCreate, nil, now)
	if err != nil {
		return Subscription{}, err
	}
	if request.SubscriberID == "" {
		request.SubscriberID = decision.Subject()
	}
	if request.SubscriberID != decision.Subject() {
		return Subscription{}, auth.ObjectNotFound()
	}
	if err := validateID("subscription token", request.Token, false); err != nil {
		return Subscription{}, err
	}
	if err := shoal.ValidateRequiredID("subscription agent ID", request.AgentID); err != nil {
		return Subscription{}, err
	}
	if request.AgentGeneration <= 0 {
		return Subscription{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "subscription agent generation must be positive")
	}
	request.Filter, err = normalizeFilter(request.Filter)
	if err != nil {
		return Subscription{}, err
	}
	for _, source := range request.Filter.SourceIDs {
		if !containsBytes(decision.PermittedSourceIDs(), source) {
			return Subscription{}, shoal.NewError(shoal.ErrorUnauthorized, "subscription filter exceeds authorization")
		}
	}
	for _, policy := range request.Filter.PolicyIDs {
		if !containsBytes(decision.PermittedPolicyIDs(), policy) {
			return Subscription{}, shoal.NewError(shoal.ErrorUnauthorized, "subscription filter exceeds authorization")
		}
	}
	if request.TTL == 0 {
		request.TTL = DefaultSubscriptionTTL
	}
	if request.TTL <= 0 || request.TTL > MaxSubscriptionTTL {
		return Subscription{}, shoal.NewError(shoal.ErrorInvalidArgument, "subscription TTL is outside its bound")
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return Subscription{}, err
	}
	request.Token = deriveID(
		"fleet-subscription-create-token-v2",
		[]byte(decision.Subject()), fingerprint.Bytes(), request.Token,
	)
	if err := s.leases.ValidateDelivery(
		ctx, request.AgentID, request.AgentGeneration,
	); err != nil {
		return Subscription{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return Subscription{}, err
	}
	audit := lifecycleReceiptForDecision(decision, fingerprint)
	var subscription Subscription
	var repeated bool
	if backend, ok := s.backend.(MutationReceiptBackend); ok {
		subscription, audit, repeated, err = backend.CreateWithAudit(
			ctx, request, fingerprint, decision.PolicyGeneration(), audit, now)
	} else {
		subscription, repeated, err = s.backend.Create(
			ctx, request, fingerprint, decision.PolicyGeneration(), now)
	}
	if err != nil {
		return Subscription{}, mapContextError(err)
	}
	if err := s.recordWithReceipt(
		ctx, audit, auth.OperationSubscriptionCreate, request.Token,
		subscription.ID, nil, nil, nil, subscription.CreatedAt, repeated,
	); err != nil {
		return Subscription{}, classifyAuditError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return Subscription{}, errors.Join(ErrActionCommitted, err)
	}
	return cloneSubscription(subscription), nil
}

func (s *Service) Delete(ctx context.Context, request DeleteRequest) error {
	now := s.now().UTC()
	if err := validateRetryUntil(request.RetryUntil, now, ErrMutationExpired); err != nil {
		return err
	}
	decision, guard, err := s.authorize(ctx, auth.OperationSubscriptionDelete, nil, now)
	if err != nil {
		return err
	}
	if err := validateID("subscription ID", request.SubscriptionID, false); err != nil ||
		request.ExpectedGeneration == 0 {
		if err != nil {
			return err
		}
		return shoal.NewError(shoal.ErrorInvalidArgument, "expected subscription generation is required")
	}
	if err := guard.Check(ctx); err != nil {
		return err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return err
	}
	audit := lifecycleReceiptForDecision(decision, fingerprint)
	var subscription Subscription
	var repeated bool
	if backend, ok := s.backend.(MutationReceiptBackend); ok {
		subscription, audit, repeated, err = backend.DeleteWithAudit(
			ctx, request.SubscriptionID, decision.Subject(),
			request.ExpectedGeneration, request.RetryUntil, audit, now)
	} else {
		subscription, repeated, err = s.backend.Delete(
			ctx, request.SubscriptionID, decision.Subject(),
			request.ExpectedGeneration, request.RetryUntil, now)
	}
	if err != nil {
		return nonDisclosingSubscriptionError(err)
	}
	if err := s.recordWithReceipt(
		ctx, audit, auth.OperationSubscriptionDelete, request.SubscriptionID,
		subscription.ID, nil, nil, nil, subscription.RevokedAt, repeated,
	); err != nil {
		return classifyAuditError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return errors.Join(ErrActionCommitted, err)
	}
	return nil
}

func (s *Service) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	if isReservedLifecycleKind(request.Event.Kind) {
		return PublishResult{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet lifecycle event kinds require trusted publication",
		)
	}
	return s.publish(ctx, auth.OperationEventPublish, request, true, nil)
}

// PublishLifecycle is the trusted dispatch lifecycle publication path. It
// accepts only the narrow dispatch operations and preserves the caller's
// canonical durable token unchanged across authorization refreshes.
func (s *Service) PublishLifecycle(
	ctx context.Context, capability explorerfleetcap.Capability,
	operation auth.Operation, request PublishRequest, receipt LifecycleReceipt,
) (PublishResult, error) {
	if !s.reconcile.Matches(capability) {
		return PublishResult{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"fleet lifecycle capability is invalid")
	}
	if !lifecyclePublicationPermits(request.Event.Kind, operation) {
		return PublishResult{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet lifecycle publication operation is invalid")
	}
	if receipt.RequestID == "" ||
		receipt.AuthorizationFingerprint == (auth.Fingerprint{}) ||
		receipt.AuthorizationExpiresAt.IsZero() ||
		receipt.AuthorizationExpiresAt.Location() != time.UTC ||
		!bytes.Equal(receipt.CorrelationID, request.Event.CorrelationID) {
		return PublishResult{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet lifecycle receipt provenance is invalid")
	}
	// An approval event is admitted only as the event of a durable
	// transition (#653). The capability proves the caller is host code, not
	// that the transition happened.
	if IsApprovalKind(request.Event.Kind) {
		if err := s.verifyApprovalPublication(
			ctx, operation, request, receipt); err != nil {
			return PublishResult{}, err
		}
	}
	return s.publish(ctx, operation, request, false, &receipt)
}

// isReservedLifecycleKind reports whether a kind is one the public
// Publish route refuses outright. Reserved and permitted are two questions:
// this one is about who may publish at all, the one below about which
// operation authorizes a particular kind.
func isReservedLifecycleKind(kind string) bool {
	switch kind {
	case "action.enqueued", "action.canceled",
		"action.claimed", "action.completed", "action.failed":
		return true
	}
	// Every approval kind, including ones no build publishes (#451, #653).
	// The public route may never carry one: an approval event is a statement
	// that a durable transition happened, and event_publish is not evidence
	// of that. Only the trusted lifecycle route publishes them, and only for
	// an event the approval ledger confirms (PublishLifecycle).
	return IsApprovalKind(kind)
}

// lifecyclePublicationPermits reports whether an operation may publish a
// lifecycle event of this kind.
//
// Each kind lists every operation that can legitimately have authorized the
// transition it describes. All three kinds now list more than one; this said
// "two of them", and the cancellation arm is why.
//
// An enqueue is authorized by dispatch or by invoke, because a synchronous
// invoke enqueues as a side effect of running the work.
//
// A claim or a completion is authorized by invoke or by execute. Execute is
// the one #437 added: a principal granted it on the action's descriptor may
// claim and complete work it did not enqueue, and need not hold invoke at all.
// This gate previously admitted invoke alone, which did not merely mislabel
// such an event — the publication was refused, and a refused publication is
// returned as ErrActionCommitted, so the transition was durably written and
// the worker was told to reconcile an outcome that had in fact been recorded.
//
// A cancellation is authorized by dispatch or by invoke. This said "dispatch
// only. It is the enqueuer's lever, and no execute-holder can reach Cancel."
// The second sentence is true and the first does not follow from it: no
// execute-holder reaches DispatchService.Cancel, and AdmissionService.deny is
// a second writer of this kind, reached through the admission surface under
// invoke. A property of one writer, asserted as a property of the event — in
// the same documentation that explains that exact mistake for the claim kinds
// two paragraphs above.
//
// The cost was that every admission denial committed a cancelled record and
// then failed to publish it, and a refused publication is ErrActionCommitted,
// so the caller was told a refusal that had granted nothing needed
// reconciliation. The retry answered "denied", which is why it survived three
// reviews.
//
// Still not execute: Cancel requires dispatch and refuses a live claim, and
// deny is reachable only under invoke.
//
// Written as an explicit set rather than one expected operation with an ad-hoc
// exception beside it, which is what it replaced: the exception for an
// invoke-authorized enqueue was a bare boolean at the call site, and adding a
// second one for execute would have left the real rule spread across two
// places.
func lifecyclePublicationPermits(kind string, operation auth.Operation) bool {
	var permitted []auth.Operation
	switch kind {
	case "action.enqueued":
		permitted = []auth.Operation{auth.OperationDispatch, auth.OperationInvoke}
	case "action.canceled":
		// Dispatch for DispatchService.Cancel, invoke for
		// AdmissionService.deny. Two legitimate writers, two operations.
		//
		// This said dispatch alone, justified as "a cancellation is dispatch
		// only. It is the enqueuer's lever, and no execute-holder can reach
		// Cancel." The first sentence is true of DispatchService.Cancel and
		// false of the kind: deny produces an action.canceled under invoke,
		// and the permitted set refused it — so every admission denial
		// committed and then reported a publication failure.
		permitted = []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke,
		}
	case "action.claimed", "action.completed", "action.failed":
		permitted = []auth.Operation{auth.OperationInvoke, auth.OperationExecute}
	default:
		// Approval transitions (#653) have their own table. It names the one
		// operation each transition is made under. An unknown approval
		// kind, or any other kind, is admitted by none.
		return approvalPublicationPermits(kind, operation)
	}
	for _, candidate := range permitted {
		if operation == candidate {
			return true
		}
	}
	return false
}

func (s *Service) publish(
	ctx context.Context, operation auth.Operation, request PublishRequest,
	scopePublicToken bool, lifecycleReceipt *LifecycleReceipt,
) (PublishResult, error) {
	now := s.now().UTC()
	if err := validateRetryUntil(
		request.RetryUntil, now, ErrPublicationExpired,
	); err != nil {
		return PublishResult{}, err
	}
	if err := validateID("event publication token", request.Token, false); err != nil {
		return PublishResult{}, err
	}
	request.Event.OccurredAt = request.Event.OccurredAt.UTC()
	var err error
	request.Event, err = normalizeEvent(request.Event, false)
	if err != nil {
		return PublishResult{}, err
	}
	decision, guard, err := s.authorize(ctx, operation, request.Event.Evidence, now)
	if err != nil {
		return PublishResult{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return PublishResult{}, err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return PublishResult{}, err
	}
	if scopePublicToken {
		request.Token = deriveID(
			"fleet-event-publication-token-v2",
			[]byte(decision.Subject()), fingerprint.Bytes(), request.Token,
		)
	}
	if lifecycleReceipt != nil {
		request.Audit = *lifecycleReceipt
	} else {
		request.Audit = LifecycleReceipt{
			RequestID:                decision.RequestID(),
			CorrelationID:            []byte(decision.CorrelationID()),
			AuthorizationFingerprint: fingerprint,
			AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		}
	}
	result, err := s.backend.Append(ctx, request, now)
	if err != nil {
		return PublishResult{}, mapContextError(err)
	}
	if result.Audit.AuthorizationFingerprint == (auth.Fingerprint{}) {
		result.Audit = request.Audit
	}
	auditTime := request.Event.OccurredAt
	// Note, for anyone tempted to pass a different operation here than the
	// one this publication was gated on: `operation` does two jobs at once.
	// It is the gate — lifecyclePublicationPermits and authorize above both
	// run against it — and it is *attribution*, because it is persisted on
	// the audit record below. So a caller publishing a transition it did not
	// perform cannot substitute its own operation to get past the gate: that
	// would record the wrong authority as having performed the transition,
	// which is #460's defect in a new place. #480 item 3 is drained by
	// skipping foreign rows instead, leaving them for the principal whose
	// authority they carry.
	record := AuditRecord{
		Operation: operation, ActionID: cloneBytes(request.Event.ActionID),
		ObjectID:         cloneBytes(result.EventID),
		Evidence:         cloneEvidence(request.Event.Evidence),
		ConsumedEvidence: cloneEvidenceReferences(request.Event.ConsumedEvidence),
		CitedEvidence:    cloneEvidenceReferences(request.Event.CitedEvidence),
		OccurredAt:       auditTime,
	}
	record.RequestID = result.Audit.RequestID
	record.CorrelationID = cloneBytes(result.Audit.CorrelationID)
	record.AuthorizationFingerprint = result.Audit.AuthorizationFingerprint
	record.AuthorizationExpiresAt = result.Audit.AuthorizationExpiresAt
	var recordErr error
	if lifecycleReceipt != nil {
		if reconciler, ok := s.auditor.(ReconciliationAuditor); ok {
			recordErr = reconciler.RecordFleetActionReconciliation(
				ctx, s.reconcile, record)
		} else if result.Repeated {
			if retryAuditor, retryOK := s.auditor.(RetryAuditor); retryOK {
				recordErr = retryAuditor.RecordFleetActionRetry(ctx, record)
			} else {
				recordErr = s.auditor.RecordFleetAction(ctx, record)
			}
		} else {
			recordErr = s.auditor.RecordFleetAction(ctx, record)
		}
	} else if result.Repeated {
		if retryAuditor, ok := s.auditor.(RetryAuditor); ok {
			recordErr = retryAuditor.RecordFleetActionRetry(ctx, record)
		} else {
			recordErr = s.auditor.RecordFleetAction(ctx, record)
		}
	} else {
		recordErr = s.auditor.RecordFleetAction(ctx, record)
	}
	if recordErr != nil {
		return PublishResult{}, classifyAuditError(recordErr)
	}
	if err := guard.Check(ctx); err != nil {
		return PublishResult{}, errors.Join(ErrActionCommitted, err)
	}
	result.EventID = cloneBytes(result.EventID)
	// The stream position is internal resume state. Returning it would reveal
	// the number of otherwise hidden events to an authorized publisher.
	result.Sequence = 0
	return result, nil
}

func (s *Service) Pull(ctx context.Context, request PullRequest) (Page, error) {
	if request.Limit == 0 {
		request.Limit = MaxPageSize
	}
	if request.Limit < 1 || request.Limit > MaxPageSize || request.Wait < 0 || request.Wait > s.maxWait {
		return Page{}, shoal.NewError(shoal.ErrorInvalidArgument, "event pull bounds are invalid")
	}
	subscription, decision, fingerprint, next, frontier, err :=
		s.deliveryState(ctx, request, s.now().UTC())
	if err != nil {
		return Page{}, err
	}
	deadline := s.now().UTC().Add(request.Wait)
	pollDelay := s.poll
	for {
		events, pinnedFrontier, scanErr := s.backend.Scan(ctx, next, frontier, request.Limit)
		if scanErr != nil {
			return Page{}, mapContextError(scanErr)
		}
		frontier = pinnedFrontier
		page := make([]Event, 0, len(events))
		for _, event := range events {
			if event.Sequence < next {
				return Page{}, shoal.NewError(shoal.ErrorInternal, "event backend returned an invalid sequence")
			}
			_, allowed, authErr := s.authorizeDelivery(ctx, subscription, event, decision)
			if authErr != nil {
				return Page{}, authErr
			}
			if allowed {
				// The stored event, not a projection of it: the final gate
				// below re-authorizes on the complete authorization join and
				// projects once, for the subscriber as it is then.
				page = append(page, cloneEvent(event))
			}
			next = event.Sequence + 1
		}
		if len(page) > 0 || request.Wait == 0 || !s.now().UTC().Before(deadline) {
			freshSubscription, freshDecision, freshFingerprint, _, _, refreshErr :=
				s.deliveryState(ctx, PullRequest{SubscriptionID: request.SubscriptionID}, s.now().UTC())
			if refreshErr != nil {
				return Page{}, shoal.WrapError(
					shoal.ErrorUnavailable,
					"subscription authorization changed", refreshErr)
			}
			if freshSubscription.Generation != subscription.Generation ||
				freshDecision.Subject() != decision.Subject() ||
				freshFingerprint != fingerprint {
				return Page{}, shoal.NewError(shoal.ErrorUnavailable, "subscription authorization changed")
			}
			authorizedPage := page[:0]
			for _, event := range page {
				delivered, allowed, authErr := s.authorizeDelivery(
					ctx, freshSubscription, event, freshDecision)
				if authErr != nil {
					return Page{}, authErr
				}
				if allowed {
					authorizedPage = append(authorizedPage, delivered)
				}
			}
			// The label check projects the authorized page in one batch, so
			// a page costs a bounded number of catalog reads however many
			// references it carries (#564).
			page, err = s.readableEvents(ctx, authorizedPage)
			if err != nil {
				return Page{}, err
			}
			// Global stream positions and the count of filtered events are
			// internal cursor state. Exposing them would reveal hidden objects.
			for i := range page {
				page[i].Sequence = 0
			}

			cursor, sealErr := s.cursors.seal(cursorState{
				SubscriptionID: subscription.ID, SubscriberID: string(subscription.SubscriberID),
				Fingerprint: fingerprint, Generation: subscription.Generation,
				NextSequence: next, Frontier: frontier,
				ExpiresAt: s.now().UTC().Add(s.cursors.ttl),
			})
			if sealErr != nil {
				return Page{}, sealErr
			}
			return Page{Events: page, NextCursor: cursor, HighWater: 0, AtLeastOnce: true}, nil
		}
		remaining := deadline.Sub(s.now().UTC())
		if pollDelay > remaining {
			pollDelay = remaining
		}
		timer := time.NewTimer(pollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Page{}, mapContextError(ctx.Err())
		case <-timer.C:
		}
		freshSubscription, freshDecision, freshFingerprint, _, _, refreshErr :=
			s.deliveryState(ctx, PullRequest{SubscriptionID: request.SubscriptionID}, s.now().UTC())
		if refreshErr != nil {
			return Page{}, shoal.WrapError(
				shoal.ErrorUnavailable,
				"subscription authorization changed", refreshErr)
		}
		if freshSubscription.Generation != subscription.Generation ||
			freshDecision.Subject() != decision.Subject() ||
			freshFingerprint != fingerprint {
			return Page{}, shoal.NewError(shoal.ErrorUnavailable, "subscription authorization changed")
		}
		subscription, decision, fingerprint =
			freshSubscription, freshDecision, freshFingerprint
		if pollDelay < time.Second {
			pollDelay *= 2
			if pollDelay > time.Second {
				pollDelay = time.Second
			}
		}
	}
}

func classifyAuditError(err error) error {
	if err == nil || interaction.IsCommittedRecord(err) {
		return err
	}
	return errors.Join(ErrAuditOutcomeUnknown, err)
}

func validateRetryUntil(
	retryUntil, now time.Time, expired error,
) error {
	if retryUntil.IsZero() || retryUntil.Location() != time.UTC {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "mutation retry deadline must be UTC")
	}
	if !now.Before(retryUntil) {
		return mapContextError(expired)
	}
	if retryUntil.After(now.Add(MaxMutationRetryWindow)) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "mutation retry window exceeds its bound")
	}
	return nil
}

func (s *Service) deliveryState(
	ctx context.Context, request PullRequest, now time.Time,
) (Subscription, auth.Decision, auth.Fingerprint, uint64, uint64, error) {
	decision, guard, err := s.authorize(ctx, auth.OperationSubscriptionCreate, nil, now)
	if err != nil {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, err
	}
	subscription, err := s.backend.Subscription(ctx, request.SubscriptionID)
	if err != nil {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, nonDisclosingSubscriptionError(err)
	}
	if subscription.SubscriberID != decision.Subject() || !subscription.RevokedAt.IsZero() ||
		!now.Before(subscription.ExpiresAt) {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, auth.ObjectNotFound()
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, err
	}
	if fingerprint != subscription.AuthorizationFingerprint {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0,
			shoal.NewError(shoal.ErrorUnavailable, "subscription authorization changed")
	}
	next := uint64(1)
	var frontier uint64
	if request.Cursor != "" {
		state, openErr := s.cursors.open(request.Cursor, now)
		if openErr != nil || !bytes.Equal(state.SubscriptionID, subscription.ID) ||
			state.SubscriberID != string(decision.Subject()) ||
			state.Fingerprint != fingerprint || state.Generation != subscription.Generation {
			return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0,
				mapContextError(ErrCursorInvalid)
		}
		next = state.NextSequence
		frontier = state.Frontier
	} else {
		next, err = s.backend.CurrentStart(ctx)
		if err != nil {
			return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, err
		}
	}

	if err := s.leases.ValidateDelivery(
		ctx, subscription.AgentID, subscription.AgentGeneration,
	); err != nil {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, err
	}
	if err := guard.Check(ctx); err != nil {
		return Subscription{}, auth.Decision{}, auth.Fingerprint{}, 0, 0, err
	}
	return subscription, decision, fingerprint, next, frontier, nil
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// authorizeDelivery is the one gate every subscriber-facing read of an
// envelope passes through — live delivery, resume from a cursor, and the
// long-poll wait all go through Pull, and Pull delivers nothing that this has
// not returned. It answers whether the subscriber may receive the event and,
// if so, the event as that subscriber may see it.
//
// Authorization runs on the stored event's complete authorization join; the
// label check then projects it (#562). The (domain, source, policy, object)
// authorization is not a label check — evidence visibility is not consulted
// by it — so a subscriber authorized on the action's own tuple would
// otherwise receive citation identifiers, offsets, node and edge IDs and
// label expressions for evidence drawn from sources whose labels it does not
// hold.
func (s *Service) authorizeDelivery(
	ctx context.Context, subscription Subscription, event Event, decision auth.Decision,
) (Event, bool, error) {
	now := s.now().UTC()
	fresh, guard, err := s.authorize(
		ctx, auth.OperationSubscriptionCreate, event.Evidence, now)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) || shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return Event{}, false, nil
		}
		return Event{}, false, err
	}
	fingerprint, err := auth.AuthorizationFingerprint(fresh)
	if err != nil {
		return Event{}, false, err
	}
	if fresh.Subject() != decision.Subject() ||
		fingerprint != subscription.AuthorizationFingerprint {
		return Event{}, false, shoal.NewError(shoal.ErrorUnavailable, "subscription authorization changed")
	}
	if err := s.leases.ValidateDelivery(
		ctx, subscription.AgentID, subscription.AgentGeneration,
	); err != nil {
		return Event{}, false, err
	}
	if !matchesFilter(subscription.Filter, event) {
		return Event{}, false, nil
	}
	// An approval event is also subject to the approval's own concealment
	// rule, asked under this subscriber's context (#653). The join above is
	// the scope's; this is the request's.
	if deliverable, err := s.approvalDeliverable(ctx, event); err != nil {
		return Event{}, false, err
	} else if !deliverable {
		return Event{}, false, nil
	}
	if err := guard.Check(ctx); err != nil {
		return Event{}, false, err
	}
	return event, true, nil
}

// readableEvents returns each event as the subscriber behind ctx may see it:
// every exact evidence reference the subscriber may not see is removed whole
// (identifiers, anchor, citation and label expression) along with any
// authorization join entry that points at it (#562).
//
// The decision is evidencelabels.Verdicts, the same one the dispatch read
// paths use (#369), taken once for every reference of every event. No count
// of what was withheld is added anywhere (#398), so a partial evidence list
// is not a completeness claim.
//
// events are not modified: they may be the durable records' own values, and
// the durable record stays complete for the same reason the action record
// does.
func (s *Service) readableEvents(ctx context.Context, events []Event) ([]Event, error) {
	var labelled []labelledReference
	for _, event := range events {
		for _, group := range []struct {
			references []interaction.EvidenceReference
			visibility [][]string
		}{
			{event.ConsumedEvidence, event.ConsumedEvidenceVisibility},
			{event.CitedEvidence, event.CitedEvidenceVisibility},
		} {
			if len(group.visibility) != 0 && len(group.visibility) != len(group.references) {
				// normalizeEvent refuses this at publish; a stored event
				// that carries it is corrupt, and guessing an alignment
				// could hand a reference the wrong label.
				return nil, shoal.NewError(
					shoal.ErrorInternal,
					"stored event evidence visibility does not align with its references")
			}
			for i := range group.references {
				value := labelledReference{reference: group.references[i]}
				if len(group.visibility) != 0 {
					value.visibility = group.visibility[i]
				}
				labelled = append(labelled, value)
			}
		}
	}
	verdicts, err := evidencelabels.Verdicts(
		ctx, s.evidenceVisibility, s.evidenceNodes, labelled,
		func(value labelledReference) []string { return value.visibility },
		func(value labelledReference) evidencelabels.Graph {
			// Every assertion names one of the reference's EdgeIDs
			// (interaction.EvidenceReference.Validate), so the edges cover
			// them; a document reference also names its cited revision.
			graph := evidencelabels.Graph{
				NodeIDs: value.reference.NodeIDs, EdgeIDs: value.reference.EdgeIDs,
			}
			if value.reference.Kind == interaction.EvidenceDocument {
				graph.DocumentID = value.reference.Citation.DocumentID
				graph.RevisionID = value.reference.Citation.RevisionID
			}
			return graph
		})
	if err != nil {
		return nil, err
	}
	result := make([]Event, len(events))
	offset := 0
	take := func(references []interaction.EvidenceReference, visibility [][]string) (
		[]interaction.EvidenceReference, [][]string, bool, error,
	) {
		count := len(references)
		group := labelled[offset : offset+count]
		decided := verdicts[offset : offset+count]
		offset += count
		return readableEvidenceGroup(references, visibility, group, decided)
	}
	for index, event := range events {
		consumed, consumedVisibility, consumedWithheld, err := take(
			event.ConsumedEvidence, event.ConsumedEvidenceVisibility)
		if err != nil {
			return nil, err
		}
		cited, citedVisibility, citedWithheld, err := take(
			event.CitedEvidence, event.CitedEvidenceVisibility)
		if err != nil {
			return nil, err
		}
		if !consumedWithheld && !citedWithheld {
			result[index] = event
			continue
		}
		projected := cloneEvent(event)
		projected.ConsumedEvidence = cloneEvidenceReferences(consumed)
		projected.ConsumedEvidenceVisibility = cloneVisibilityGroup(consumedVisibility)
		projected.CitedEvidence = cloneEvidenceReferences(cited)
		projected.CitedEvidenceVisibility = cloneVisibilityGroup(citedVisibility)
		join := make([]Evidence, 0, len(projected.Evidence))
		for _, item := range projected.Evidence {
			// An entry naming a withheld reference names material in a
			// source the subscriber may not see (its object ID was chosen
			// from that reference's own identifiers), so it goes with the
			// reference.
			if item.Reference != nil &&
				!containsReference(*item.Reference, consumed, cited) {
				continue
			}
			join = append(join, item)
		}
		projected.Evidence = join
		result[index] = projected
	}
	return result, nil
}

type labelledReference struct {
	reference  interaction.EvidenceReference
	visibility []string
}

// readableEvidenceGroup applies one group's verdicts. Verdicts that do not
// align with the group are the question failing, and fail the delivery
// rather than delivering the group unfiltered.
func readableEvidenceGroup(
	references []interaction.EvidenceReference, visibility [][]string,
	labelled []labelledReference, verdicts []bool,
) ([]interaction.EvidenceReference, [][]string, bool, error) {
	kept, withheld, err := evidencelabels.Apply(labelled, verdicts)
	if err != nil {
		return nil, nil, false, err
	}
	if !withheld {
		return references, visibility, false, nil
	}
	if len(kept) == 0 {
		return nil, nil, true, nil
	}
	keptReferences := make([]interaction.EvidenceReference, len(kept))
	keptVisibility := make([][]string, len(kept))
	for i, value := range kept {
		keptReferences[i] = value.reference
		keptVisibility[i] = value.visibility
	}
	// The visibility group's shape must not outlive what it described. A
	// group whose every remaining entry is empty is returned as nil (the
	// exact shape of an event that never carried labelled evidence) because
	// a non-nil array of empty entries would itself say "a labelled
	// reference was here and was withheld" (#398).
	return keptReferences, canonicalVisibilityGroup(keptVisibility), true, nil
}

func containsReference(
	reference interaction.EvidenceReference,
	groups ...[]interaction.EvidenceReference,
) bool {
	for _, group := range groups {
		for _, candidate := range group {
			if reflect.DeepEqual(reference, candidate) {
				return true
			}
		}
	}
	return false
}

func (s *Service) authorize(
	ctx context.Context, operation auth.Operation, evidence []Evidence, now time.Time,
) (auth.Decision, auth.GenerationGuard, error) {
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, err
	}
	if len(evidence) == 0 {
		if err := decision.Authorize(operation, auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
		}, now); err != nil {
			return auth.Decision{}, auth.GenerationGuard{}, err
		}
	} else {
		for _, item := range evidence {
			objectIDs := []shoal.ID{item.ObjectID}
			if item.Reference != nil {
				objectIDs = append(objectIDs, ExactEvidenceReferenceIDs(*item.Reference)...)
			}
			seen := make(map[shoal.ID]struct{}, len(objectIDs))
			for _, objectID := range objectIDs {
				if objectID == "" {
					continue
				}
				if _, ok := seen[objectID]; ok {
					continue
				}
				seen[objectID] = struct{}{}
				if err := decision.AuthorizeObject(operation, auth.ResourceRequest{
					AuthorizationDomain: decision.AuthorizationDomain(),
					SourceID:            item.SourceID, PolicyID: item.PolicyID, ObjectID: objectID,
				}, now); err != nil {
					return auth.Decision{}, auth.GenerationGuard{}, err
				}
			}
		}
	}
	guard, err := auth.NewGenerationGuard(decision, s.generations)
	return decision, guard, err
}

func (s *Service) record(
	ctx context.Context, decision auth.Decision, operation auth.Operation,
	actionID, objectID []byte, evidence []Evidence,
	consumed, cited []interaction.EvidenceReference, now time.Time,
) error {
	return s.recordWithRetry(
		ctx, decision, operation, actionID, objectID, evidence,
		consumed, cited, now, false)
}

func (s *Service) recordWithRetry(
	ctx context.Context, decision auth.Decision, operation auth.Operation,
	actionID, objectID []byte, evidence []Evidence,
	consumed, cited []interaction.EvidenceReference, now time.Time,
	repeated bool,
) error {
	record := AuditRecord{
		Operation: operation, ActionID: cloneBytes(actionID),
		RequestID: decision.RequestID(), CorrelationID: []byte(decision.CorrelationID()),
		AuthorizationFingerprint: func() auth.Fingerprint {
			value, _ := auth.AuthorizationFingerprint(decision)
			return value
		}(),
		AuthorizationExpiresAt: decision.AuthenticationExpires(),
		ObjectID:               cloneBytes(objectID),
		Evidence:               cloneEvidence(evidence),
		ConsumedEvidence:       cloneEvidenceReferences(consumed),
		CitedEvidence:          cloneEvidenceReferences(cited),
		OccurredAt:             now,
	}
	if repeated {
		if retryAuditor, ok := s.auditor.(RetryAuditor); ok {
			return retryAuditor.RecordFleetActionRetry(ctx, record)
		}
	}
	return s.auditor.RecordFleetAction(ctx, record)
}

func (s *Service) recordWithReceipt(
	ctx context.Context, receipt LifecycleReceipt, operation auth.Operation,
	actionID, objectID []byte, evidence []Evidence,
	consumed, cited []interaction.EvidenceReference, now time.Time,
	repeated bool,
) error {
	record := AuditRecord{
		Operation: operation, ActionID: cloneBytes(actionID),
		RequestID: receipt.RequestID, CorrelationID: cloneBytes(receipt.CorrelationID),
		AuthorizationFingerprint: receipt.AuthorizationFingerprint,
		AuthorizationExpiresAt:   receipt.AuthorizationExpiresAt,
		ObjectID:                 cloneBytes(objectID), Evidence: cloneEvidence(evidence),
		ConsumedEvidence: cloneEvidenceReferences(consumed),
		CitedEvidence:    cloneEvidenceReferences(cited), OccurredAt: now,
	}
	if repeated {
		if retryAuditor, ok := s.auditor.(RetryAuditor); ok {
			return retryAuditor.RecordFleetActionRetry(ctx, record)
		}
	}
	return s.auditor.RecordFleetAction(ctx, record)
}

func lifecycleReceiptForDecision(
	decision auth.Decision, fingerprint auth.Fingerprint,
) LifecycleReceipt {
	return LifecycleReceipt{
		RequestID:                decision.RequestID(),
		CorrelationID:            []byte(decision.CorrelationID()),
		AuthorizationFingerprint: fingerprint,
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
}

func matchesFilter(filter Filter, event Event) bool {
	if len(filter.Kinds) > 0 && !containsString(filter.Kinds, event.Kind) {
		return false
	}
	for _, evidence := range event.Evidence {
		if len(filter.SourceIDs) > 0 && !containsBytes(filter.SourceIDs, evidence.SourceID) {
			return false
		}
		if len(filter.PolicyIDs) > 0 && !containsBytes(filter.PolicyIDs, evidence.PolicyID) {
			return false
		}
	}
	return true
}

func nonDisclosingSubscriptionError(err error) error {
	if errors.Is(err, ErrSubscriptionNotFound) {
		return auth.ObjectNotFound()
	}
	if errors.Is(err, ErrGenerationConflict) {
		return shoal.NewError(shoal.ErrorConflict, "subscription generation conflict")
	}
	return mapContextError(err)
}

func deriveID(tag string, parts ...[]byte) []byte {
	hash := sha256.New()
	writeHashField(hash, []byte(tag))
	for _, part := range parts {
		writeHashField(hash, part)
	}
	return hash.Sum(nil)
}

func writeHashField(hash hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(value)
}
