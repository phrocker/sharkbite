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

package explorerfleetevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const Table = "_shoal_explorer_fleet_events"

var (
	eventPrefix         = []byte{1, 'E'}
	publicationPrefix   = []byte{1, 'P'}
	subPrefix           = []byte{1, 'S'}
	createReceiptPrefix = []byte{1, 'C'}
	deleteReceiptPrefix = []byte{1, 'D'}
	floorRow            = []byte{1, 'F'}
	publicationGCRow    = []byte{1, 'G'}
	recordFamily        = []byte("r")
	recordQualifier     = []byte("v1")
	streamEntity        = guard.Entity{Kind: 'E', ID: coordination.EntityID("fleet-event-stream-v1")}
	floorEntity         = guard.Entity{Kind: 'F', ID: coordination.EntityID("fleet-event-floor-v1")}
	publicationGCEntity = guard.Entity{Kind: 'G', ID: coordination.EntityID("fleet-event-publication-gc-v1")}
	logicalPolicy       = []byte("fleet-events/default")
)

const (
	DefaultRetainedEvents    = explorercoord.MaxCommittedScanLimit
	DefaultSubscriptionSlots = 4096
)

type Runtime interface {
	Publish(context.Context, explorercoord.Request) (explorercoord.Result, error)
	PruneCommitted(
		context.Context,
		explorercoord.PruneCommittedRequest,
	) (explorercoord.PruneCommittedResult, error)
	ReadEntity(context.Context, guard.Entity) (*guard.Head, *guard.Pending, error)
	ScanCommitted(context.Context, explorercoord.CommittedScanRequest) (explorercoord.CommittedPage, error)
}

type Adapter struct {
	appendMu      sync.Mutex
	runtime       Runtime
	domain        coordination.DomainID
	now           func() time.Time
	retained      uint64
	publicationGC []byte
}

type mutationAuditContextKey struct{}

func mutationAudit(ctx context.Context) fleetevents.LifecycleReceipt {
	value, _ := ctx.Value(mutationAuditContextKey{}).(fleetevents.LifecycleReceipt)
	return value
}

func New(runtime Runtime, domain coordination.DomainID) (*Adapter, error) {
	return NewWithRetention(runtime, domain, DefaultRetainedEvents)
}

func NewWithRetention(
	runtime Runtime, domain coordination.DomainID, retained uint64,
) (*Adapter, error) {
	if runtime == nil {
		return nil, errors.New("fleet events: runtime is required")
	}
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if retained == 0 || retained > explorercoord.MaxCommittedScanLimit {
		return nil, errors.New("fleet events: retention is outside its bound")
	}
	return &Adapter{
		runtime: runtime, domain: append(coordination.DomainID(nil), domain...),
		now: time.Now, retained: retained,
	}, nil
}

type subscriptionRecord struct {
	Subscription fleetevents.Subscription `json:"subscription"`
}

type eventRecord struct {
	Event         fleetevents.Event            `json:"event"`
	PublicationID []byte                       `json:"publication_id"`
	RetryUntil    time.Time                    `json:"retry_until"`
	Audit         fleetevents.LifecycleReceipt `json:"audit"`
}

type lifecycleReceiptWire struct {
	RequestID                []byte
	CorrelationID            []byte
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
}

func receiptToWire(value fleetevents.LifecycleReceipt) lifecycleReceiptWire {
	return lifecycleReceiptWire{
		RequestID: []byte(value.RequestID), CorrelationID: value.CorrelationID,
		AuthorizationFingerprint: value.AuthorizationFingerprint,
		AuthorizationExpiresAt:   value.AuthorizationExpiresAt,
	}
}

func (value lifecycleReceiptWire) domain() fleetevents.LifecycleReceipt {
	return fleetevents.LifecycleReceipt{
		RequestID: shoal.ID(value.RequestID), CorrelationID: value.CorrelationID,
		AuthorizationFingerprint: value.AuthorizationFingerprint,
		AuthorizationExpiresAt:   value.AuthorizationExpiresAt,
	}
}

type floorRecord struct {
	Sequence uint64 `json:"sequence"`
}

type publicationGCRecord struct {
	SweptAt time.Time `json:"swept_at"`
}

type subscriptionMutationReceipt struct {
	MutationID    []byte                       `json:"mutation_id"`
	RequestDigest []byte                       `json:"request_digest"`
	RetryUntil    time.Time                    `json:"retry_until"`
	Subscription  fleetevents.Subscription     `json:"subscription"`
	Audit         fleetevents.LifecycleReceipt `json:"audit"`
}

type subscriptionWire struct {
	ID                       []byte
	SubscriberID             []byte
	AgentID                  []byte
	AgentGeneration          int64
	AuthorizationFingerprint auth.Fingerprint
	PolicyGeneration         int64
	Filter                   fleetevents.Filter
	Generation               uint64
	CreatedAt                time.Time
	ExpiresAt                time.Time
	RevokedAt                time.Time
}

type evidenceWire struct {
	SourceID, PolicyID []byte
	ObjectID           []byte
	Reference          *evidenceReferenceWire
	NodeID, EdgeID     []byte
	AnchorID           []byte
	RevisionID         []byte
	StartOffset        int64
	EndOffset          int64
}

type evidenceReferenceWire struct {
	AnchorID   []byte
	Kind       interaction.EvidenceKind
	Citation   citationWire
	NodeIDs    [][]byte
	EdgeIDs    [][]byte
	Assertions []assertionReferenceWire
}

type citationWire struct {
	DocumentID, RevisionID, SectionID, SpanID []byte
	StartOffset, EndOffset                    int64
	StartPage, EndPage                        int32
}

type assertionReferenceWire struct {
	AssertionID, EdgeID []byte
	Origin              ontology.AssertionOrigin
}

type eventWire struct {
	Sequence                      uint64
	EventID, ProducerID, ActionID []byte
	TransitionID, CorrelationID   []byte
	Kind                          string
	ProducerGeneration            int64
	Reason                        interaction.Reason
	Evidence                      []evidenceWire
	ConsumedEvidence              []evidenceReferenceWire
	CitedEvidence                 []evidenceReferenceWire
	// Visibility expressions for the exact evidence groups, index for index
	// (#562). Absent on an event written before the field existed, which
	// decodes as an all-unlabelled group.
	ConsumedEvidenceVisibility [][]string `json:",omitempty"`
	CitedEvidenceVisibility    [][]string `json:",omitempty"`
	OccurredAt                 time.Time
}

func subscriptionToWire(value fleetevents.Subscription) subscriptionWire {
	return subscriptionWire{
		ID: value.ID, SubscriberID: []byte(value.SubscriberID),
		AgentID: []byte(value.AgentID), AgentGeneration: value.AgentGeneration,
		AuthorizationFingerprint: value.AuthorizationFingerprint,
		PolicyGeneration:         value.PolicyGeneration, Filter: value.Filter,
		Generation: value.Generation, CreatedAt: value.CreatedAt,
		ExpiresAt: value.ExpiresAt, RevokedAt: value.RevokedAt,
	}
}

func (value subscriptionWire) domain() fleetevents.Subscription {
	return fleetevents.Subscription{
		ID: value.ID, SubscriberID: shoal.ID(value.SubscriberID),
		AgentID: shoal.ID(value.AgentID), AgentGeneration: value.AgentGeneration,
		AuthorizationFingerprint: value.AuthorizationFingerprint,
		PolicyGeneration:         value.PolicyGeneration, Filter: value.Filter,
		Generation: value.Generation, CreatedAt: value.CreatedAt,
		ExpiresAt: value.ExpiresAt, RevokedAt: value.RevokedAt,
	}
}

func eventToWire(value fleetevents.Event) eventWire {
	evidence := make([]evidenceWire, len(value.Evidence))
	for i, item := range value.Evidence {
		evidence[i] = evidenceWire{
			SourceID: item.SourceID, PolicyID: item.PolicyID,
			ObjectID: []byte(item.ObjectID), NodeID: []byte(item.NodeID),
			EdgeID: []byte(item.EdgeID), AnchorID: []byte(item.AnchorID),
			RevisionID:  []byte(item.RevisionID),
			StartOffset: item.Start, EndOffset: item.End,
		}
		if item.Reference != nil {
			reference := evidenceReferencesToWire(
				[]interaction.EvidenceReference{*item.Reference})[0]
			evidence[i].Reference = &reference
		}
	}
	return eventWire{
		Sequence: value.Sequence, EventID: value.EventID, Kind: value.Kind,
		ProducerID: value.ProducerID, ProducerGeneration: value.ProducerGeneration,
		ActionID: value.ActionID, TransitionID: value.TransitionID,
		CorrelationID: value.CorrelationID, Reason: value.Reason,
		Evidence:         evidence,
		ConsumedEvidence: evidenceReferencesToWire(value.ConsumedEvidence),
		CitedEvidence:    evidenceReferencesToWire(value.CitedEvidence),
		ConsumedEvidenceVisibility: visibilityGroupCopy(
			value.ConsumedEvidenceVisibility),
		CitedEvidenceVisibility: visibilityGroupCopy(
			value.CitedEvidenceVisibility),
		OccurredAt: value.OccurredAt,
	}
}

func visibilityGroupCopy(values [][]string) [][]string {
	if len(values) == 0 {
		return nil
	}
	result := make([][]string, len(values))
	for i, value := range values {
		if len(value) > 0 {
			result[i] = append([]string(nil), value...)
		}
	}
	return result
}

func (value eventWire) domain() fleetevents.Event {
	evidence := make([]fleetevents.Evidence, len(value.Evidence))
	for i, item := range value.Evidence {
		evidence[i] = fleetevents.Evidence{
			SourceID: item.SourceID, PolicyID: item.PolicyID,
			ObjectID: shoal.ID(item.ObjectID), NodeID: shoal.ID(item.NodeID),
			EdgeID: shoal.ID(item.EdgeID), AnchorID: shoal.ID(item.AnchorID),
			RevisionID: shoal.ID(item.RevisionID),
			Start:      item.StartOffset, End: item.EndOffset,
		}
		if item.Reference != nil {
			reference := evidenceReferencesFromWire(
				[]evidenceReferenceWire{*item.Reference})[0]
			evidence[i].Reference = &reference
		}
	}
	return fleetevents.Event{
		Sequence: value.Sequence, EventID: value.EventID, Kind: value.Kind,
		ProducerID: value.ProducerID, ProducerGeneration: value.ProducerGeneration,
		ActionID: value.ActionID, TransitionID: value.TransitionID,
		CorrelationID: value.CorrelationID, Reason: value.Reason,
		Evidence:         evidence,
		ConsumedEvidence: evidenceReferencesFromWire(value.ConsumedEvidence),
		CitedEvidence:    evidenceReferencesFromWire(value.CitedEvidence),
		ConsumedEvidenceVisibility: visibilityGroupCopy(
			value.ConsumedEvidenceVisibility),
		CitedEvidenceVisibility: visibilityGroupCopy(
			value.CitedEvidenceVisibility),
		OccurredAt: value.OccurredAt,
	}
}

func evidenceReferencesToWire(
	values []interaction.EvidenceReference,
) []evidenceReferenceWire {
	if len(values) == 0 {
		return nil
	}
	result := make([]evidenceReferenceWire, len(values))
	for i, value := range values {
		result[i] = evidenceReferenceWire{
			AnchorID: []byte(value.AnchorID), Kind: value.Kind,
			Citation: citationWire{
				DocumentID:  []byte(value.Citation.DocumentID),
				RevisionID:  []byte(value.Citation.RevisionID),
				SectionID:   []byte(value.Citation.SectionID),
				SpanID:      []byte(value.Citation.SpanID),
				StartOffset: value.Citation.Range.Start.Offset,
				EndOffset:   value.Citation.Range.End.Offset,
				StartPage:   value.Citation.Range.Start.Page,
				EndPage:     value.Citation.Range.End.Page,
			},
			NodeIDs:    idsToWire(value.NodeIDs),
			EdgeIDs:    idsToWire(value.EdgeIDs),
			Assertions: make([]assertionReferenceWire, len(value.Assertions)),
		}
		for j, assertion := range value.Assertions {
			result[i].Assertions[j] = assertionReferenceWire{
				AssertionID: []byte(assertion.AssertionID),
				EdgeID:      []byte(assertion.EdgeID), Origin: assertion.Origin,
			}
		}
	}
	return result
}

func evidenceReferencesFromWire(
	values []evidenceReferenceWire,
) []interaction.EvidenceReference {
	if len(values) == 0 {
		return nil
	}
	result := make([]interaction.EvidenceReference, len(values))
	for i, value := range values {
		result[i] = interaction.EvidenceReference{
			AnchorID: shoal.ID(value.AnchorID), Kind: value.Kind,
			Citation: document.Citation{
				DocumentID: shoal.ID(value.Citation.DocumentID),
				RevisionID: shoal.ID(value.Citation.RevisionID),
				SectionID:  shoal.ID(value.Citation.SectionID),
				SpanID:     shoal.ID(value.Citation.SpanID),
				Range: document.SourceRange{
					Start: document.SourcePosition{
						Offset: value.Citation.StartOffset, Page: value.Citation.StartPage},
					End: document.SourcePosition{
						Offset: value.Citation.EndOffset, Page: value.Citation.EndPage},
				},
			},
			NodeIDs: idsFromWire(value.NodeIDs),
			EdgeIDs: idsFromWire(value.EdgeIDs),
		}
		if len(value.Assertions) > 0 {
			result[i].Assertions = make(
				[]interaction.AssertionReference, len(value.Assertions))
		}
		for j, assertion := range value.Assertions {
			result[i].Assertions[j] = interaction.AssertionReference{
				AssertionID: shoal.ID(assertion.AssertionID),
				EdgeID:      shoal.ID(assertion.EdgeID), Origin: assertion.Origin,
			}
		}
	}
	return result
}

func idsToWire(values []shoal.ID) [][]byte {
	if len(values) == 0 {
		return nil
	}
	result := make([][]byte, len(values))
	for i, value := range values {
		result[i] = []byte(value)
	}
	return result
}

func idsFromWire(values [][]byte) []shoal.ID {
	if len(values) == 0 {
		return nil
	}
	result := make([]shoal.ID, len(values))
	for i, value := range values {
		result[i] = shoal.ID(value)
	}
	return result
}

func (r subscriptionRecord) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct{ Subscription subscriptionWire }{
		Subscription: subscriptionToWire(r.Subscription),
	})
}

func (r *subscriptionRecord) UnmarshalJSON(value []byte) error {
	var wire struct{ Subscription subscriptionWire }
	if err := json.Unmarshal(value, &wire); err != nil {
		return err
	}
	r.Subscription = wire.Subscription.domain()
	return nil
}

func (r eventRecord) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Event         eventWire
		PublicationID []byte
		RetryUntil    time.Time
		Audit         lifecycleReceiptWire
	}{eventToWire(r.Event), r.PublicationID, r.RetryUntil, receiptToWire(r.Audit)})
}

func (r *eventRecord) UnmarshalJSON(value []byte) error {
	var wire struct {
		Event         eventWire
		PublicationID []byte
		RetryUntil    time.Time
		Audit         lifecycleReceiptWire
	}
	if err := json.Unmarshal(value, &wire); err != nil {
		return err
	}
	r.Event, r.PublicationID, r.RetryUntil =
		wire.Event.domain(), wire.PublicationID, wire.RetryUntil
	r.Audit = wire.Audit.domain()
	return nil
}

func (r subscriptionMutationReceipt) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		MutationID, RequestDigest []byte
		RetryUntil                time.Time
		Subscription              subscriptionWire
		Audit                     lifecycleReceiptWire
	}{
		r.MutationID, r.RequestDigest, r.RetryUntil,
		subscriptionToWire(r.Subscription), receiptToWire(r.Audit),
	})
}

func (r *subscriptionMutationReceipt) UnmarshalJSON(value []byte) error {
	var wire struct {
		MutationID, RequestDigest []byte
		RetryUntil                time.Time
		Subscription              subscriptionWire
		Audit                     lifecycleReceiptWire
	}
	if err := json.Unmarshal(value, &wire); err != nil {
		return err
	}
	r.MutationID, r.RequestDigest, r.RetryUntil =
		wire.MutationID, wire.RequestDigest, wire.RetryUntil
	r.Subscription = wire.Subscription.domain()
	r.Audit = wire.Audit.domain()
	return nil
}

func (a *Adapter) Create(
	ctx context.Context, request fleetevents.CreateRequest, fingerprint auth.Fingerprint,
	policyGeneration int64, now time.Time,
) (fleetevents.Subscription, bool, error) {
	id := digest("fleet-subscription-v1", []byte(request.SubscriberID), request.Token)
	mutationID := digest(
		"fleet-subscription-create-mutation-v1",
		[]byte(request.SubscriberID), request.Token,
	)
	requestDigest, err := createRequestDigest(
		request, fingerprint, policyGeneration)
	if err != nil {
		return fleetevents.Subscription{}, false, err
	}
	receiptRow := a.subscriptionReceiptRow(createReceiptPrefix, mutationID)
	receiptEntity := a.subscriptionReceiptEntity('C', receiptRow)
	receiptMode, receiptEpoch, receiptDigest, replay, receiptErr :=
		a.prepareSubscriptionReceipt(
			ctx, receiptRow, receiptEntity, mutationID, requestDigest,
			request.RetryUntil, now,
		)
	if receiptErr != nil {
		return fleetevents.Subscription{}, false, receiptErr
	}
	if replay != nil {
		return replay.Subscription, true, nil
	}
	entity := a.subscriptionEntity(id)
	mode := guard.ModeAbsentOrIdentical
	var expectedEpoch coordination.Epoch
	var expectedDigest coordination.Digest
	if existing, err := a.subscriptionSlotRecord(ctx, id); err == nil {
		subscription := existing.Subscription
		if bytes.Equal(subscription.ID, id) &&
			subscription.SubscriberID == request.SubscriberID &&
			subscription.AgentID == request.AgentID &&
			subscription.AgentGeneration == request.AgentGeneration &&
			subscription.AuthorizationFingerprint == fingerprint &&
			subscription.PolicyGeneration == policyGeneration &&
			reflect.DeepEqual(subscription.Filter, request.Filter) &&
			subscription.ExpiresAt.Sub(subscription.CreatedAt) == request.TTL {
			return fleetevents.Subscription{}, false, translate(transaction.ErrConflict)
		}
		if bytes.Equal(subscription.ID, id) ||
			(subscription.RevokedAt.IsZero() && now.Before(subscription.ExpiresAt)) {
			return fleetevents.Subscription{}, false, translate(transaction.ErrConflict)
		}
		head, _, readErr := a.runtime.ReadEntity(ctx, entity)
		if readErr != nil || head == nil {
			return fleetevents.Subscription{}, false, translate(readErr)
		}
		mode, expectedEpoch, expectedDigest =
			guard.ModeMutate, head.Epoch, head.LogicalDigest
	} else if !errors.Is(err, fleetevents.ErrSubscriptionNotFound) {
		return fleetevents.Subscription{}, false, err
	}
	subscription := fleetevents.Subscription{
		ID: id, SubscriberID: request.SubscriberID, AgentID: request.AgentID,
		AgentGeneration:          request.AgentGeneration,
		AuthorizationFingerprint: fingerprint, PolicyGeneration: policyGeneration,
		Filter: request.Filter, Generation: 1, CreatedAt: now,
		ExpiresAt: now.Add(request.TTL),
	}
	value, err := json.Marshal(subscriptionRecord{Subscription: subscription})
	if err != nil {
		return fleetevents.Subscription{}, false, err
	}
	receiptValue, err := json.Marshal(subscriptionMutationReceipt{
		MutationID: mutationID, RequestDigest: requestDigest,
		RetryUntil: request.RetryUntil, Subscription: subscription,
		Audit: mutationAudit(ctx),
	})
	if err != nil {
		return fleetevents.Subscription{}, false, err
	}
	row := a.subscriptionRow(id)
	result, err := a.publishSubscriptionMutation(
		ctx, []byte("fleet-subscription-create-v2"),
		digest("fleet-subscription-create-runtime-v2", request.Token, encodeTime(request.RetryUntil)),
		row, value, entity, mode, expectedEpoch, expectedDigest, id,
		receiptRow, receiptValue, receiptEntity, receiptMode,
		receiptEpoch, receiptDigest, mutationID,
	)
	if err != nil {
		return fleetevents.Subscription{}, false, translate(err)
	}
	return subscription, result.Unchanged, nil
}

func (a *Adapter) CreateWithAudit(
	ctx context.Context, request fleetevents.CreateRequest, fingerprint auth.Fingerprint,
	policyGeneration int64, audit fleetevents.LifecycleReceipt, now time.Time,
) (fleetevents.Subscription, fleetevents.LifecycleReceipt, bool, error) {
	ctx = context.WithValue(ctx, mutationAuditContextKey{}, audit)
	subscription, repeated, err := a.Create(
		ctx, request, fingerprint, policyGeneration, now)
	if err != nil {
		return fleetevents.Subscription{}, fleetevents.LifecycleReceipt{}, false, err
	}
	mutationID := digest(
		"fleet-subscription-create-mutation-v1",
		[]byte(request.SubscriberID), request.Token,
	)
	persisted, err := a.subscriptionMutationReceipt(
		ctx, a.subscriptionReceiptRow(createReceiptPrefix, mutationID))
	if err != nil {
		return fleetevents.Subscription{}, fleetevents.LifecycleReceipt{}, false, err
	}
	return subscription, persisted.Audit, repeated, nil
}

func (a *Adapter) Subscription(ctx context.Context, id []byte) (fleetevents.Subscription, error) {
	record, err := a.subscriptionRecord(ctx, id)
	if err != nil {
		return fleetevents.Subscription{}, err
	}
	return record.Subscription, nil
}

func (a *Adapter) subscriptionRecord(
	ctx context.Context, id []byte,
) (subscriptionRecord, error) {
	record, err := a.subscriptionSlotRecord(ctx, id)
	if err != nil {
		return subscriptionRecord{}, err
	}
	if !bytes.Equal(record.Subscription.ID, id) {
		return subscriptionRecord{}, fleetevents.ErrSubscriptionNotFound
	}
	return record, nil
}

func (a *Adapter) subscriptionSlotRecord(
	ctx context.Context, id []byte,
) (subscriptionRecord, error) {
	row := a.subscriptionRow(id)
	page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: row,
		Family: recordFamily, Qualifier: recordQualifier, Limit: 1,
	})
	if err != nil {
		return subscriptionRecord{}, translate(err)
	}
	cells := page.Cells
	if len(cells) != 1 || !bytes.Equal(cells[0].Cell.Coordinate.Row, row) {
		return subscriptionRecord{}, fleetevents.ErrSubscriptionNotFound
	}
	var record subscriptionRecord
	if err := json.Unmarshal(cells[0].Cell.Value, &record); err != nil {
		return subscriptionRecord{}, fmt.Errorf("fleet events: decode subscription: %w", err)
	}
	return record, nil
}

func createRequestDigest(
	request fleetevents.CreateRequest, fingerprint auth.Fingerprint,
	policyGeneration int64,
) ([]byte, error) {
	value, err := json.Marshal(struct {
		SubscriberID     []byte             `json:"subscriber_id"`
		AgentID          []byte             `json:"agent_id"`
		AgentGeneration  int64              `json:"agent_generation"`
		Fingerprint      auth.Fingerprint   `json:"fingerprint"`
		PolicyGeneration int64              `json:"policy_generation"`
		Filter           fleetevents.Filter `json:"filter"`
		TTL              int64              `json:"ttl"`
		RetryUntil       time.Time          `json:"retry_until"`
	}{
		SubscriberID: []byte(request.SubscriberID),
		AgentID:      []byte(request.AgentID), AgentGeneration: request.AgentGeneration,
		Fingerprint: fingerprint, PolicyGeneration: policyGeneration,
		Filter: request.Filter, TTL: int64(request.TTL),
		RetryUntil: request.RetryUntil,
	})
	if err != nil {
		return nil, err
	}
	return digest("fleet-subscription-create-request-v2", value), nil
}

func (a *Adapter) prepareSubscriptionReceipt(
	ctx context.Context, row []byte, entity guard.Entity,
	mutationID, requestDigest []byte, retryUntil, now time.Time,
) (
	guard.Mode, coordination.Epoch, coordination.Digest,
	*subscriptionMutationReceipt, error,
) {
	if err := validateRetryWindow(
		retryUntil, now, fleetevents.ErrMutationExpired, "mutation",
	); err != nil {
		return 0, 0, coordination.Digest{}, nil, err
	}
	page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: row, Family: recordFamily,
		Qualifier: recordQualifier, Limit: 1,
	})
	if err != nil {
		return 0, 0, coordination.Digest{}, nil, translate(err)
	}
	mode := guard.ModeAbsentOrIdentical
	var expectedEpoch coordination.Epoch
	var expectedDigest coordination.Digest
	if len(page.Cells) == 1 && bytes.Equal(page.Cells[0].Cell.Coordinate.Row, row) {
		var receipt subscriptionMutationReceipt
		if err := json.Unmarshal(page.Cells[0].Cell.Value, &receipt); err != nil {
			return 0, 0, coordination.Digest{}, nil, err
		}
		if receipt.RetryUntil.IsZero() ||
			receipt.RetryUntil.Location() != time.UTC ||
			len(receipt.MutationID) == 0 || len(receipt.RequestDigest) == 0 {
			return 0, 0, coordination.Digest{}, nil, errors.New(
				"fleet events: corrupt subscription mutation receipt")
		}
		if bytes.Equal(receipt.MutationID, mutationID) {
			if !now.Before(receipt.RetryUntil) {
				return 0, 0, coordination.Digest{}, nil, fleetevents.ErrMutationExpired
			}
			if !bytes.Equal(receipt.RequestDigest, requestDigest) {
				return 0, 0, coordination.Digest{}, nil, translate(transaction.ErrConflict)
			}
			return 0, 0, coordination.Digest{}, &receipt, nil
		}
		if now.Before(receipt.RetryUntil) {
			return 0, 0, coordination.Digest{}, nil, fleetevents.ErrRetentionCapacity
		}
		head, _, readErr := a.runtime.ReadEntity(ctx, entity)
		if readErr != nil || head == nil {
			return 0, 0, coordination.Digest{}, nil, translate(readErr)
		}
		mode, expectedEpoch, expectedDigest =
			guard.ModeMutate, head.Epoch, head.LogicalDigest
	}
	return mode, expectedEpoch, expectedDigest, nil, nil
}

func (a *Adapter) Delete(
	ctx context.Context, id []byte, subscriberID shoal.ID, expected uint64,
	retryUntil, now time.Time,
) (fleetevents.Subscription, bool, error) {
	deletionToken := digest(
		"fleet-subscription-delete-token-v2",
		[]byte(subscriberID), id, encodeUint64(expected))
	requestDigest := digest(
		"fleet-subscription-delete-request-v3",
		[]byte(subscriberID), id, encodeUint64(expected), encodeTime(retryUntil),
	)
	receiptRow := a.subscriptionReceiptRow(deleteReceiptPrefix, deletionToken)
	receiptEntity := a.subscriptionReceiptEntity('D', receiptRow)
	receiptMode, receiptEpoch, receiptDigest, replay, receiptErr :=
		a.prepareSubscriptionReceipt(
			ctx, receiptRow, receiptEntity, deletionToken, requestDigest,
			retryUntil, now,
		)
	if receiptErr != nil {
		return fleetevents.Subscription{}, false, receiptErr
	}
	if replay != nil {
		if replay.Subscription.SubscriberID != subscriberID {
			return fleetevents.Subscription{}, false, fleetevents.ErrSubscriptionNotFound
		}
		return replay.Subscription, true, nil
	}
	record, err := a.subscriptionRecord(ctx, id)
	if err != nil {
		return fleetevents.Subscription{}, false, err
	}
	subscription := record.Subscription
	if subscription.SubscriberID != subscriberID {
		return fleetevents.Subscription{}, false, fleetevents.ErrSubscriptionNotFound
	}
	if !subscription.RevokedAt.IsZero() {
		return fleetevents.Subscription{}, false, fleetevents.ErrGenerationConflict
	}
	if subscription.Generation != expected {
		return fleetevents.Subscription{}, false, fleetevents.ErrGenerationConflict
	}
	head, _, err := a.runtime.ReadEntity(ctx, a.subscriptionEntity(id))
	if err != nil {
		return fleetevents.Subscription{}, false, translate(err)
	}
	if head == nil {
		return fleetevents.Subscription{}, false, fleetevents.ErrGenerationConflict
	}
	subscription.Generation++
	subscription.RevokedAt = now
	value, err := json.Marshal(subscriptionRecord{Subscription: subscription})
	if err != nil {
		return fleetevents.Subscription{}, false, err
	}
	receiptValue, err := json.Marshal(subscriptionMutationReceipt{
		MutationID: deletionToken, RequestDigest: requestDigest,
		RetryUntil: retryUntil, Subscription: subscription,
		Audit: mutationAudit(ctx),
	})
	if err != nil {
		return fleetevents.Subscription{}, false, err
	}
	row := a.subscriptionRow(id)
	_, err = a.publishSubscriptionMutation(
		ctx, []byte("fleet-subscription-delete-v2"),
		digest("fleet-subscription-delete-runtime-v2", deletionToken, encodeTime(retryUntil)),
		row, value, a.subscriptionEntity(id), guard.ModeMutate,
		head.Epoch, head.LogicalDigest, encodeUint64(subscription.Generation),
		receiptRow, receiptValue, receiptEntity, receiptMode,
		receiptEpoch, receiptDigest, deletionToken,
	)
	if err != nil {
		return fleetevents.Subscription{}, false, translate(err)
	}
	return subscription, false, nil
}

func (a *Adapter) DeleteWithAudit(
	ctx context.Context, id []byte, subscriberID shoal.ID, expected uint64,
	retryUntil time.Time, audit fleetevents.LifecycleReceipt, now time.Time,
) (fleetevents.Subscription, fleetevents.LifecycleReceipt, bool, error) {
	ctx = context.WithValue(ctx, mutationAuditContextKey{}, audit)
	subscription, repeated, err := a.Delete(
		ctx, id, subscriberID, expected, retryUntil, now)
	if err != nil {
		return fleetevents.Subscription{}, fleetevents.LifecycleReceipt{}, false, err
	}
	deletionToken := digest(
		"fleet-subscription-delete-token-v2",
		[]byte(subscriberID), id, encodeUint64(expected))
	persisted, err := a.subscriptionMutationReceipt(
		ctx, a.subscriptionReceiptRow(deleteReceiptPrefix, deletionToken))
	if err != nil {
		return fleetevents.Subscription{}, fleetevents.LifecycleReceipt{}, false, err
	}
	return subscription, persisted.Audit, repeated, nil
}

func (a *Adapter) subscriptionMutationReceipt(
	ctx context.Context, row []byte,
) (subscriptionMutationReceipt, error) {
	page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: row, Family: recordFamily,
		Qualifier: recordQualifier, Limit: 1,
	})
	if err != nil {
		return subscriptionMutationReceipt{}, translate(err)
	}
	if len(page.Cells) != 1 ||
		!bytes.Equal(page.Cells[0].Cell.Coordinate.Row, row) {
		return subscriptionMutationReceipt{}, errors.New(
			"fleet events: committed subscription receipt is missing")
	}
	var receipt subscriptionMutationReceipt
	if err := json.Unmarshal(page.Cells[0].Cell.Value, &receipt); err != nil {
		return subscriptionMutationReceipt{}, err
	}
	return receipt, nil
}

func (a *Adapter) Append(
	ctx context.Context, request fleetevents.PublishRequest, now time.Time,
) (fleetevents.PublishResult, error) {
	a.appendMu.Lock()
	defer a.appendMu.Unlock()
	if err := validateRetryWindow(
		request.RetryUntil, now, fleetevents.ErrPublicationExpired, "publication",
	); err != nil {
		return fleetevents.PublishResult{}, err
	}
	publicationID := digest("fleet-event-publication-id-v2", request.Token)
	eventID := digest(
		"fleet-event-id-v2", publicationID, encodeTime(request.RetryUntil))
	if err := a.pruneExpiredPublications(ctx, now); err != nil &&
		!errors.Is(err, transaction.ErrConflict) {
		return fleetevents.PublishResult{}, translate(err)
	}
	for attempt := 0; attempt < 32; attempt++ {
		if repeated, found, err := a.repeatedPublication(
			ctx, publicationID, eventID, request, now,
		); err != nil {
			return fleetevents.PublishResult{}, translate(err)
		} else if found {
			return repeated, nil
		}
		head, _, err := a.runtime.ReadEntity(ctx, streamEntity)
		var sequence uint64 = 1
		var expectedEpoch coordination.Epoch
		var expectedDigest coordination.Digest
		mode := guard.ModeAbsentOrIdentical
		if err == nil && head != nil {
			if len(head.WinnerID) != 8 {
				return fleetevents.PublishResult{}, errors.New("fleet events: corrupt stream head")
			}
			sequence = binary.BigEndian.Uint64(head.WinnerID) + 1
			expectedEpoch, expectedDigest, mode = head.Epoch, head.LogicalDigest, guard.ModeMutate
		} else if err != nil && !errors.Is(err, guard.ErrNotFound) && !errors.Is(err, transaction.ErrNotFound) {
			return fleetevents.PublishResult{}, translate(err)
		}
		event := request.Event
		event.Sequence, event.EventID = sequence, eventID
		value, marshalErr := json.Marshal(eventRecord{
			Event: event, PublicationID: publicationID,
			RetryUntil: request.RetryUntil, Audit: request.Audit,
		})
		if marshalErr != nil {
			return fleetevents.PublishResult{}, marshalErr
		}
		row := a.eventRow(sequence)
		var result explorercoord.Result
		var publishErr error
		for retry := 0; retry < 100; retry++ {
			result, publishErr = a.publishEvent(
				ctx, digest(
					"fleet-event-runtime-token-v2",
					request.Token, encodeTime(request.RetryUntil),
				),
				publicationID, row, value, eventID, sequence, now,
				mode, expectedEpoch, expectedDigest,
			)
			if publishErr == nil || !errors.Is(publishErr, explorercoord.ErrIndeterminatePublication) {
				break
			}
			delay := time.Duration(retry+1) * time.Millisecond
			if delay > 10*time.Millisecond {
				delay = 10 * time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fleetevents.PublishResult{}, ctx.Err()
			case <-timer.C:
			}
		}
		if publishErr == nil {
			return fleetevents.PublishResult{
				EventID: eventID, Sequence: sequence,
				Repeated: result.Unchanged, Audit: request.Audit,
			}, nil
		}
		// A guard entity of this publication belongs to another store (its
		// 'P' entity, hex(eventID), is an approval's). That is no race: the
		// head stays the other store's, so every retry here, and every retry
		// by the caller, would be refused the same way. It is a definite
		// refusal, not an unknown outcome; nothing was published.
		if errors.Is(publishErr, guard.ErrForeignHead) {
			return fleetevents.PublishResult{}, refusedPublication(publishErr)
		}
		if !errors.Is(publishErr, transaction.ErrConflict) {
			return fleetevents.PublishResult{}, translate(publishErr)
		}
	}
	return fleetevents.PublishResult{}, fleetevents.ErrPublicationUnknown
}

func refusedPublication(err error) error {
	return errors.Join(
		fleetevents.ErrPublicationRefused,
		shoal.NewError(shoal.ErrorConflict,
			"fleet event publication refused: its event entity belongs to another store"),
		err,
	)
}

func validateRetryWindow(
	retryUntil, now time.Time, expired error, kind string,
) error {
	if retryUntil.IsZero() || retryUntil.Location() != time.UTC {
		return fmt.Errorf("fleet events: %s retry deadline must be UTC", kind)
	}
	if !now.Before(retryUntil) {
		return expired
	}
	if retryUntil.After(now.Add(fleetevents.MaxMutationRetryWindow)) {
		return fmt.Errorf(
			"fleet events: %s retry deadline exceeds maximum window", kind)
	}
	return nil
}

func (a *Adapter) repeatedPublication(
	ctx context.Context, publicationID, eventID []byte,
	request fleetevents.PublishRequest, now time.Time,
) (fleetevents.PublishResult, bool, error) {
	existing, _, found, err := a.readPublicationEvent(ctx, publicationID)
	if err != nil {
		return fleetevents.PublishResult{}, false, err
	}
	if !found {
		return fleetevents.PublishResult{}, false, nil
	}
	if existing.RetryUntil.IsZero() ||
		existing.RetryUntil.Location() != time.UTC {
		return fleetevents.PublishResult{}, false, errors.New(
			"fleet events: corrupt publication receipt")
	}
	if !now.Before(existing.RetryUntil) {
		return fleetevents.PublishResult{}, false, nil
	}
	if !bytes.Equal(existing.Event.EventID, eventID) ||
		!existing.RetryUntil.Equal(request.RetryUntil) {
		return fleetevents.PublishResult{}, false, transaction.ErrConflict
	}
	requested := request.Event
	requested.Sequence = existing.Event.Sequence
	requested.EventID = append([]byte(nil), eventID...)
	expected, err := json.Marshal(eventRecord{
		Event: requested, PublicationID: publicationID,
		RetryUntil: request.RetryUntil, Audit: existing.Audit,
	})
	if err != nil {
		return fleetevents.PublishResult{}, false, err
	}
	actual, err := json.Marshal(existing)
	if err != nil {
		return fleetevents.PublishResult{}, false, err
	}
	if !bytes.Equal(expected, actual) {
		return fleetevents.PublishResult{}, false, transaction.ErrConflict
	}
	return fleetevents.PublishResult{
		EventID: append([]byte(nil), eventID...), Sequence: existing.Event.Sequence,
		Repeated: true, Audit: existing.Audit,
	}, true, nil
}

func (a *Adapter) publishEvent(
	ctx context.Context,
	token, publicationID, row, value, eventID []byte,
	sequence uint64,
	now time.Time,
	streamMode guard.Mode, expectedEpoch coordination.Epoch,
	expectedDigest coordination.Digest,
) (explorercoord.Result, error) {
	lpart, err := explorercoord.Partition(a.domain, streamEntity.ID)
	if err != nil {
		return explorercoord.Result{}, err
	}
	sequenceID := encodeUint64(sequence)
	floor := uint64(1)
	if sequence > a.retained {
		floor = sequence - a.retained + 1
	}
	if sequence > a.retained {
		if retired, committed, found, readErr := a.readSlotEvent(ctx, row); readErr != nil {
			return explorercoord.Result{}, readErr
		} else if found {
			if retired.RetryUntil.IsZero() ||
				retired.RetryUntil.Location() != time.UTC {
				return explorercoord.Result{}, errors.New(
					"fleet events: corrupt retained publication receipt")
			}
			if retired.Event.Sequence != sequence-a.retained {
				return explorercoord.Result{}, errors.New(
					"fleet events: corrupt retained event sequence")
			}
			if err := a.pruneEvent(
				ctx, retired, committed, floor, lpart,
			); err != nil {
				return explorercoord.Result{}, err
			}
		}
	}
	slotEntity := a.eventSlotEntity(sequence)
	slotGuard, err := a.mutationGuard(ctx, slotEntity, eventID, lpart)
	if err != nil {
		return explorercoord.Result{}, err
	}
	publicationEntity := publicationEntity(eventID)
	publicationGuard, err := a.mutationGuard(
		ctx, publicationEntity, eventID, lpart)
	if err != nil {
		return explorercoord.Result{}, err
	}

	cells := []explorercoord.Cell{
		{
			Table: Table, Row: append([]byte(nil), row...), Family: recordFamily,
			Qualifier: recordQualifier, Value: append([]byte(nil), value...),
			EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
		},
		{
			Table: Table, Row: publicationRow(publicationID), Family: recordFamily,
			Qualifier: recordQualifier, Value: append([]byte(nil), value...),
			EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
		},
	}
	guards := []explorercoord.GuardIntent{
		{
			Entity: streamEntity, Mode: streamMode, ExpectedEpoch: expectedEpoch,
			ExpectedDigest: expectedDigest, DesiredState: guard.StateLive,
			DesiredWinnerID: sequenceID, LPART: lpart,
			LogicalPolicyID: logicalPolicy, RetirementGeneration: 1,
		},
		slotGuard, publicationGuard,
	}
	if sequence <= a.retained {
		floorValue, marshalErr := json.Marshal(floorRecord{Sequence: floor})
		if marshalErr != nil {
			return explorercoord.Result{}, marshalErr
		}
		floorGuard, guardErr := a.mutationGuard(
			ctx, floorEntity, encodeUint64(floor), lpart)
		if guardErr != nil {
			return explorercoord.Result{}, guardErr
		}
		cells = append(cells, explorercoord.Cell{
			Table: Table, Row: append([]byte(nil), floorRow...), Family: recordFamily,
			Qualifier: recordQualifier, Value: floorValue,
			EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
		})
		guards = append(guards, floorGuard)
	}
	return a.runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: []byte("fleet-event-publish-v1"), Token: append([]byte(nil), token...),
		Cells:  cells,
		Guards: guards,
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-event-publish-v1"), ID: append([]byte(nil), eventID...),
		}},
	}})
}

func (a *Adapter) pruneExpiredPublications(
	ctx context.Context, now time.Time,
) error {
	page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: publicationPrefix, Family: recordFamily,
		Qualifier: recordQualifier, Limit: explorercoord.MaxPruneTargets,
		StartAfterRow: a.publicationGC,
		MaxScanned:    explorercoord.MaxCommittedScanCells,
	})
	if err != nil {
		return err
	}
	if len(page.NextRow) == 0 {
		a.publicationGC = nil
	} else {
		a.publicationGC = append(a.publicationGC[:0], page.NextRow...)
	}
	targets := make([]explorercoord.PruneTarget, 0, len(page.Cells))
	tokenParts := make([][]byte, 0, len(page.Cells)+1)
	tokenParts = append(tokenParts, encodeTime(now))
	for _, cell := range page.Cells {
		var record eventRecord
		if err := json.Unmarshal(cell.Cell.Value, &record); err != nil {
			return err
		}
		if record.RetryUntil.IsZero() ||
			record.RetryUntil.Location() != time.UTC {
			return errors.New("fleet events: corrupt publication receipt")
		}
		if now.Before(record.RetryUntil) {
			continue
		}
		targets = append(targets, explorercoord.PruneTarget{
			Table: Table, Cell: cell,
			Entity: publicationEntity(record.Event.EventID),
		})
		tokenParts = append(tokenParts, record.Event.EventID)
	}
	if len(targets) == 0 {
		return nil
	}
	lpart, err := explorercoord.Partition(a.domain, streamEntity.ID)
	if err != nil {
		return err
	}
	checkpointValue, err := json.Marshal(publicationGCRecord{SweptAt: now})
	if err != nil {
		return err
	}
	checkpointGuard, err := a.mutationGuard(
		ctx, publicationGCEntity, encodeTime(now), lpart)
	if err != nil {
		return err
	}
	_, err = a.runtime.PruneCommitted(ctx, explorercoord.PruneCommittedRequest{
		Operation: []byte("fleet-event-publication-gc-v1"),
		Token:     digest("fleet-event-publication-gc-token-v1", tokenParts...),
		Targets:   targets,
		Checkpoint: explorercoord.PruneCheckpoint{
			Cell: explorercoord.Cell{
				Table: Table, Row: publicationGCRow, Family: recordFamily,
				Qualifier: recordQualifier, Value: checkpointValue,
				EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
			},
			Guard: checkpointGuard,
		},
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-event-publication-gc-v1"),
			ID:   encodeTime(now),
		}},
	})
	return err
}

func (a *Adapter) pruneEvent(
	ctx context.Context, retired eventRecord,
	committed explorercoord.CommittedCell, floor uint64,
	lpart coordination.LPART,
) error {
	floorValue, err := json.Marshal(floorRecord{Sequence: floor})
	if err != nil {
		return err
	}
	floorGuard, err := a.mutationGuard(
		ctx, floorEntity, encodeUint64(floor), lpart)
	if err != nil {
		return err
	}
	_, err = a.runtime.PruneCommitted(ctx, explorercoord.PruneCommittedRequest{
		Operation: []byte("fleet-event-prune-v1"),
		Token: digest(
			"fleet-event-prune-token-v1",
			encodeUint64(retired.Event.Sequence),
			retired.Event.EventID,
			encodeUint64(floor),
		),
		Targets: []explorercoord.PruneTarget{
			{
				Table:  Table,
				Cell:   committed,
				Entity: a.eventSlotEntity(retired.Event.Sequence),
			},
		},
		Checkpoint: explorercoord.PruneCheckpoint{
			Cell: explorercoord.Cell{
				Table: Table, Row: append([]byte(nil), floorRow...),
				Family: recordFamily, Qualifier: recordQualifier,
				Value: floorValue, EpochTimestamp: true,
				LPART: lpart, CopyGeneration: 1,
			},
			Guard: floorGuard,
		},
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-event-prune-v1"),
			ID:   encodeUint64(floor),
		}},
	})
	return translate(err)
}

func (a *Adapter) Scan(
	ctx context.Context, next, frontier uint64, limit int,
) ([]fleetevents.Event, uint64, error) {
	if next == 0 {
		next = 1
	}

	floor, _, err := a.readFloor(ctx)
	if err != nil {
		return nil, 0, err
	}
	if next < floor {
		return nil, 0, fleetevents.ErrResyncRequired
	}
	var highWater uint64
	head, _, headErr := a.runtime.ReadEntity(ctx, streamEntity)
	if headErr == nil && head != nil && len(head.WinnerID) == 8 {
		highWater = binary.BigEndian.Uint64(head.WinnerID)
	}
	events, usedFrontier, err := a.scanEventsAt(
		ctx, next, coordination.Epoch(frontier), limit)
	if err != nil {
		return nil, 0, err
	}
	if len(events) == 0 && frontier != 0 {
		events, usedFrontier, err = a.scanEventsAt(ctx, next, 0, limit)
		if err != nil {
			return nil, 0, err
		}
	}
	if len(events) > 0 && events[0].Sequence > next {
		return nil, 0, fleetevents.ErrResyncRequired
	} else if len(events) == 0 && next <= highWater {
		return nil, 0, fleetevents.ErrResyncRequired
	}
	return events, uint64(usedFrontier), nil
}

func (a *Adapter) CurrentStart(ctx context.Context) (uint64, error) {
	floor, _, err := a.readFloor(ctx)
	return floor, err
}

func (a *Adapter) scanEventsAt(
	ctx context.Context, next uint64, frontier coordination.Epoch, limit int,
) ([]fleetevents.Event, coordination.Epoch, error) {
	events := make([]fleetevents.Event, 0, limit)
	for sequence := next; len(events) < limit; sequence++ {
		row := a.eventRow(sequence)
		page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
			Table: Table, RowPrefix: eventPrefix, StartRow: row,
			Family: recordFamily, Qualifier: recordQualifier,
			Frontier: frontier, Limit: 1, MaxScanned: 4096,
		})
		if err != nil {
			if errors.Is(err, transaction.ErrConflict) {
				return nil, 0, fleetevents.ErrResyncRequired
			}
			return nil, 0, translate(err)
		}
		if frontier == 0 {
			frontier = page.Frontier
		}
		if len(page.Cells) == 0 ||
			!bytes.Equal(page.Cells[0].Cell.Coordinate.Row, row) {
			break
		}
		cell := page.Cells[0]
		var record eventRecord
		if err := json.Unmarshal(cell.Cell.Value, &record); err != nil {
			return nil, 0, fmt.Errorf("fleet events: decode event: %w", err)
		}
		if record.Event.Sequence < sequence {
			break
		}
		if record.Event.Sequence > sequence {
			return nil, 0, fleetevents.ErrResyncRequired
		}
		events = append(events, record.Event)
	}
	return events, frontier, nil
}

func decodeSortedEvents(
	cells []explorercoord.CommittedCell, next uint64, limit int,
) ([]fleetevents.Event, error) {
	events := make([]fleetevents.Event, 0, len(cells))
	for _, cell := range cells {
		var record eventRecord
		if err := json.Unmarshal(cell.Cell.Value, &record); err != nil {
			return nil, fmt.Errorf("fleet events: decode event: %w", err)
		}
		if record.Event.Sequence >= next {
			events = append(events, record.Event)
		}
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].Sequence < events[j].Sequence
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (a *Adapter) readFloor(ctx context.Context) (uint64, time.Time, error) {
	page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: floorRow, Family: recordFamily,
		Qualifier: recordQualifier, Limit: 1,
	})
	if err != nil {
		return 0, time.Time{}, translate(err)
	}
	if len(page.Cells) == 0 {
		return 1, time.Time{}, nil
	}
	var record floorRecord
	if err := json.Unmarshal(page.Cells[0].Cell.Value, &record); err != nil ||
		record.Sequence == 0 {
		return 0, time.Time{}, errors.New("fleet events: corrupt retention floor")
	}
	return record.Sequence, time.Time{}, nil
}

func (a *Adapter) readSlotEvent(
	ctx context.Context, row []byte,
) (eventRecord, explorercoord.CommittedCell, bool, error) {
	page, err := a.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: row, Family: recordFamily,
		Qualifier: recordQualifier, Limit: 1,
	})
	if err != nil {
		return eventRecord{}, explorercoord.CommittedCell{}, false, translate(err)
	}
	if len(page.Cells) == 0 ||
		!bytes.Equal(page.Cells[0].Cell.Coordinate.Row, row) {
		return eventRecord{}, explorercoord.CommittedCell{}, false, nil
	}
	var record eventRecord
	if err := json.Unmarshal(page.Cells[0].Cell.Value, &record); err != nil {
		return eventRecord{}, explorercoord.CommittedCell{}, false, err
	}
	return record, page.Cells[0], true, nil
}

func (a *Adapter) readPublicationEvent(
	ctx context.Context, publicationID []byte,
) (eventRecord, explorercoord.CommittedCell, bool, error) {
	return a.readSlotEvent(ctx, publicationRow(publicationID))
}

func (a *Adapter) mutationGuard(
	ctx context.Context, entity guard.Entity, winner []byte,
	lpart coordination.LPART,
) (explorercoord.GuardIntent, error) {
	result := explorercoord.GuardIntent{
		Entity: entity, Mode: guard.ModeAbsentOrIdentical,
		DesiredState: guard.StateLive, DesiredWinnerID: append([]byte(nil), winner...),
		LPART: lpart, LogicalPolicyID: logicalPolicy, RetirementGeneration: 1,
	}
	head, _, err := a.runtime.ReadEntity(ctx, entity)
	if err == nil && head != nil && !bytes.Equal(head.LogicalPolicyID, logicalPolicy) {
		// Another store's head (kind 'P' is shared with approvals). The
		// guard would refuse the mutate anyway; refusing here, from the
		// stored head, gives the same answer to every retry, where the
		// guard's own refusal is seen only by the attempt that made it
		// (its transaction is conflicted after that).
		return explorercoord.GuardIntent{}, errors.Join(
			transaction.ErrConflict, guard.ErrForeignHead)
	}
	if err == nil && head != nil {
		result.Mode = guard.ModeMutate
		result.ExpectedEpoch = head.Epoch
		result.ExpectedDigest = head.LogicalDigest
		return result, nil
	}
	if errors.Is(err, guard.ErrNotFound) || errors.Is(err, transaction.ErrNotFound) ||
		(err == nil && head == nil) {
		return result, nil
	}
	return explorercoord.GuardIntent{}, translate(err)
}

func (a *Adapter) publish(
	ctx context.Context, operation, token, row, value []byte,
	entity guard.Entity, mode guard.Mode, expectedEpoch coordination.Epoch,
	expectedDigest coordination.Digest, winnerID, resultID []byte,
) (explorercoord.Result, error) {
	lpart, err := explorercoord.Partition(a.domain, entity.ID)
	if err != nil {
		return explorercoord.Result{}, err
	}
	return a.runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: operation, Token: append([]byte(nil), token...),
		Cells: []explorercoord.Cell{{
			Table: Table, Row: append([]byte(nil), row...), Family: recordFamily,
			Qualifier: recordQualifier, Value: append([]byte(nil), value...),
			EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []explorercoord.GuardIntent{{
			Entity: entity, Mode: mode, ExpectedEpoch: expectedEpoch,
			ExpectedDigest: expectedDigest, DesiredState: guard.StateLive,
			DesiredWinnerID: append([]byte(nil), winnerID...), LPART: lpart,
			LogicalPolicyID: logicalPolicy, RetirementGeneration: 1,
		}},
		Results: []explorercoord.ResultIdentity{{Kind: operation, ID: append([]byte(nil), resultID...)}},
	}})
}

func (a *Adapter) publishSubscriptionMutation(
	ctx context.Context, operation, token,
	subscriptionRow, subscriptionValue []byte,
	subscriptionEntity guard.Entity, subscriptionMode guard.Mode,
	subscriptionEpoch coordination.Epoch,
	subscriptionDigest coordination.Digest, subscriptionWinner []byte,
	receiptRow, receiptValue []byte, receiptEntity guard.Entity,
	receiptMode guard.Mode, receiptEpoch coordination.Epoch,
	receiptDigest coordination.Digest, receiptWinner []byte,
) (explorercoord.Result, error) {
	lpart, err := explorercoord.Partition(a.domain, subscriptionEntity.ID)
	if err != nil {
		return explorercoord.Result{}, err
	}
	return a.runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: operation, Token: append([]byte(nil), token...),
		Cells: []explorercoord.Cell{
			{
				Table: Table, Row: append([]byte(nil), subscriptionRow...),
				Family: recordFamily, Qualifier: recordQualifier,
				Value:          append([]byte(nil), subscriptionValue...),
				EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
			},
			{
				Table: Table, Row: append([]byte(nil), receiptRow...),
				Family: recordFamily, Qualifier: recordQualifier,
				Value:          append([]byte(nil), receiptValue...),
				EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
			},
		},
		Guards: []explorercoord.GuardIntent{
			{
				Entity: subscriptionEntity, Mode: subscriptionMode,
				ExpectedEpoch:   subscriptionEpoch,
				ExpectedDigest:  subscriptionDigest,
				DesiredState:    guard.StateLive,
				DesiredWinnerID: append([]byte(nil), subscriptionWinner...),
				LPART:           lpart, LogicalPolicyID: logicalPolicy,
				RetirementGeneration: 1,
			},
			{
				Entity: receiptEntity, Mode: receiptMode,
				ExpectedEpoch: receiptEpoch, ExpectedDigest: receiptDigest,
				DesiredState:    guard.StateLive,
				DesiredWinnerID: append([]byte(nil), receiptWinner...),
				LPART:           lpart, LogicalPolicyID: logicalPolicy,
				RetirementGeneration: 1,
			},
		},
		Results: []explorercoord.ResultIdentity{{
			Kind: operation, ID: append([]byte(nil), receiptWinner...),
		}},
	}})
}

func eventRow(sequence uint64) []byte {
	row := make([]byte, len(eventPrefix)+8)
	copy(row, eventPrefix)
	binary.BigEndian.PutUint64(row[len(eventPrefix):], sequence)
	return row
}

func publicationRow(publicationID []byte) []byte {
	row := make([]byte, 0, len(publicationPrefix)+len(publicationID))
	row = append(row, publicationPrefix...)
	return append(row, publicationID...)
}

func (a *Adapter) eventRow(sequence uint64) []byte {
	return eventRow((sequence-1)%a.retained + 1)
}

func publicationEntity(publicationID []byte) guard.Entity {
	return guard.Entity{
		Kind: 'P',
		ID:   coordination.EntityID(hex.EncodeToString(publicationID)),
	}
}

func (a *Adapter) eventSlotEntity(sequence uint64) guard.Entity {
	return guard.Entity{
		Kind: 'e',
		ID: coordination.EntityID(digest(
			"fleet-event-slot-v2", encodeUint64(sequence))),
	}
}

func (a *Adapter) subscriptionRow(id []byte) []byte {
	slot := binary.BigEndian.Uint64(digest("fleet-subscription-slot-v1", id)[:8]) %
		DefaultSubscriptionSlots
	row := make([]byte, len(subPrefix)+8)
	copy(row, subPrefix)
	binary.BigEndian.PutUint64(row[len(subPrefix):], slot)
	return row
}

func (a *Adapter) subscriptionEntity(id []byte) guard.Entity {
	row := a.subscriptionRow(id)
	return guard.Entity{
		Kind: 'S',
		ID:   coordination.EntityID(digest("fleet-subscription-slot-entity-v1", row)),
	}
}

func (a *Adapter) subscriptionReceiptRow(prefix, mutationID []byte) []byte {
	slot := binary.BigEndian.Uint64(
		digest("fleet-subscription-receipt-slot-v1", prefix, mutationID)[:8],
	) % DefaultSubscriptionSlots
	row := make([]byte, len(prefix)+8)
	copy(row, prefix)
	binary.BigEndian.PutUint64(row[len(prefix):], slot)
	return row
}

func (a *Adapter) subscriptionReceiptEntity(
	kind byte, row []byte,
) guard.Entity {
	return guard.Entity{
		Kind: kind,
		ID: coordination.EntityID(digest(
			"fleet-subscription-receipt-entity-v1", row)),
	}
}

func encodeUint64(value uint64) []byte {
	result := make([]byte, 8)
	binary.BigEndian.PutUint64(result, value)
	return result
}

func encodeTime(value time.Time) []byte {
	return []byte(value.UTC().Format(time.RFC3339Nano))
}

func digest(tag string, parts ...[]byte) []byte {
	hash := sha256.New()
	writeDigestField(hash, []byte(tag))
	for _, part := range parts {
		writeDigestField(hash, part)
	}
	return hash.Sum(nil)
}

func writeDigestField(hash hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(value)
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, explorercoord.ErrIndeterminatePublication) {
		return errors.Join(fleetevents.ErrPublicationUnknown, err)
	}
	if errors.Is(err, transaction.ErrConflict) {
		return errors.Join(
			shoal.NewError(shoal.ErrorConflict, "fleet event transaction conflict"),
			err,
		)
	}
	return err
}
