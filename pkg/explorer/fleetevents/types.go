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

// Package fleetevents defines durable, resumable product events for agent fleets.
package fleetevents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxIDBytes             = shoal.MaxIDBytes
	MaxKinds               = 64
	MaxKindBytes           = 128
	MaxEvidence            = 257
	MaxPageSize            = 256
	MaxSubscriptionTTL     = 30 * 24 * time.Hour
	DefaultSubscriptionTTL = 24 * time.Hour
	DefaultCursorTTL       = 15 * time.Minute
	// MaxMutationRetryWindow bounds durable publication and subscription
	// mutation receipts so their physical keyspaces remain finite.
	MaxMutationRetryWindow = 24 * time.Hour
	// MaxLongPollWait stays below the production HTTP server's 30-second
	// write timeout so an empty pull can complete before transport teardown.
	MaxLongPollWait = 25 * time.Second
)

var (
	ErrResyncRequired      = errors.New("fleet events: resync required")
	ErrCursorInvalid       = errors.New("fleet events: invalid cursor")
	ErrGenerationConflict  = errors.New("fleet events: subscription generation conflict")
	ErrActionCommitted     = errors.New("fleet events: action committed before authorization changed")
	ErrAuditOutcomeUnknown = errors.New("fleet events: action committed but audit outcome is unknown")
	ErrPublicationUnknown  = errors.New("fleet events: publication outcome is unknown")
	// ErrPublicationRefused is a definite refusal: nothing was published,
	// and retrying the same publication is refused the same way.
	ErrPublicationRefused   = errors.New("fleet events: publication refused")
	ErrPublicationExpired   = errors.New("fleet events: publication idempotency window expired")
	ErrMutationExpired      = errors.New("fleet events: subscription mutation retry window expired")
	ErrRetentionCapacity    = errors.New("fleet events: retained retry window is at capacity")
	ErrSubscriptionNotFound = errors.New("fleet events: subscription not found")
)

// Evidence is the authorization join for an event. Exact consumed and cited
// evidence remains grouped separately on Event.
type Evidence struct {
	SourceID   []byte
	PolicyID   []byte
	ObjectID   shoal.ID
	Reference  *interaction.EvidenceReference
	NodeID     shoal.ID
	EdgeID     shoal.ID
	AnchorID   shoal.ID
	RevisionID shoal.ID
	Start      int64
	End        int64
}

// Event is one committed event envelope. Sequence is assigned by the durable
// backend. EventID is stable across retries of the same publication token.
type Event struct {
	Sequence           uint64
	EventID            []byte
	Kind               string
	ProducerID         []byte
	ProducerGeneration int64
	ActionID           []byte
	TransitionID       []byte
	CorrelationID      []byte
	Reason             interaction.Reason
	Evidence           []Evidence
	ConsumedEvidence   []interaction.EvidenceReference
	CitedEvidence      []interaction.EvidenceReference
	// ConsumedEvidenceVisibility and CitedEvidenceVisibility carry, index for
	// index, the visibility expression each exact evidence reference was
	// recorded under. Empty means every reference in the group is
	// unlabelled; otherwise the length equals the group's, and an empty
	// entry marks one unlabelled reference.
	//
	// They are on the durable event so the delivery-time check can ask each
	// *subscriber* whether it holds a reference's labels (#562). The event
	// is the durable record of the transition and stays complete; delivery
	// drops, per subscriber, every reference whose labels the subscriber does
	// not hold, together with its expression here and any authorization join
	// entry that points at it.
	ConsumedEvidenceVisibility [][]string
	CitedEvidenceVisibility    [][]string
	OccurredAt                 time.Time
}

// PublishRequest contains the immutable event, an idempotency token, and the
// caller's bounded UTC deadline for retrying an ambiguous commit.
type PublishRequest struct {
	Token      []byte
	RetryUntil time.Time
	Event      Event
	Audit      LifecycleReceipt
}

type PublishResult struct {
	EventID  []byte
	Sequence uint64
	Repeated bool
	Audit    LifecycleReceipt
}

// Filter can only narrow delivery. Empty sets mean no additional narrowing.
type Filter struct {
	Kinds     []string
	SourceIDs [][]byte
	PolicyIDs [][]byte
}

type Subscription struct {
	ID                       []byte
	SubscriberID             shoal.ID
	AgentID                  shoal.ID
	AgentGeneration          int64
	AuthorizationFingerprint auth.Fingerprint
	PolicyGeneration         int64
	Filter                   Filter
	Generation               uint64
	CreatedAt                time.Time
	ExpiresAt                time.Time
	RevokedAt                time.Time
}

type CreateRequest struct {
	Token           []byte
	SubscriberID    shoal.ID
	AgentID         shoal.ID
	AgentGeneration int64
	Filter          Filter
	TTL             time.Duration
	RetryUntil      time.Time
}

type DeleteRequest struct {
	SubscriptionID     []byte
	ExpectedGeneration uint64
	RetryUntil         time.Time
}

type PullRequest struct {
	SubscriptionID []byte
	Cursor         string
	Limit          int
	Wait           time.Duration
}

type Page struct {
	Events      []Event
	NextCursor  string
	HighWater   uint64
	AtLeastOnce bool
}

// AuditRecord is a redacted privileged-action receipt. Implementations should
// use the existing durable interaction spine.
type AuditRecord struct {
	Operation                auth.Operation
	ActionID                 []byte
	RequestID                shoal.ID
	CorrelationID            []byte
	ObjectID                 []byte
	Evidence                 []Evidence
	ConsumedEvidence         []interaction.EvidenceReference
	CitedEvidence            []interaction.EvidenceReference
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
	OccurredAt               time.Time
}

// LifecycleReceipt pins the authorization provenance of the durable action
// transition whose event is being published. Fresh authorization is still
// evaluated independently for every publication attempt.
type LifecycleReceipt struct {
	RequestID                shoal.ID
	CorrelationID            []byte
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
}

type Auditor interface {
	RecordFleetAction(context.Context, AuditRecord) error
}

// RetryAuditor can reconcile an exact publication retry against an
// authoritative interaction receipt before attempting another write.
type RetryAuditor interface {
	RecordFleetActionRetry(context.Context, AuditRecord) error
}

// ReconciliationAuditor records the first audit receipt for a durable
// lifecycle transition under fresh authorization while preserving the
// transition's original authorization pins.
type ReconciliationAuditor interface {
	RecordFleetActionReconciliation(
		context.Context, explorerfleetcap.Capability, AuditRecord,
	) error
}

// LeaseValidator rechecks the target agent's durable lease and delegation
// immediately before delivery. Implementations must not cache a success.
type LeaseValidator interface {
	ValidateDelivery(context.Context, shoal.ID, int64) error
}

// Backend is the durable source of truth. Scan must return only committed,
// prefix-visible records and must return ErrResyncRequired below HistoryFloor.
type Backend interface {
	Create(context.Context, CreateRequest, auth.Fingerprint, int64, time.Time) (Subscription, bool, error)
	Subscription(context.Context, []byte) (Subscription, error)
	Delete(context.Context, []byte, shoal.ID, uint64, time.Time, time.Time) (Subscription, bool, error)
	Append(context.Context, PublishRequest, time.Time) (PublishResult, error)
	CurrentStart(context.Context) (uint64, error)
	Scan(context.Context, uint64, uint64, int) ([]Event, uint64, error)
}

// MutationReceiptBackend preserves the authorization receipt that accompanied
// the first committed subscription mutation so later exact retries can repair
// the interaction audit without adopting fresh request credentials.
type MutationReceiptBackend interface {
	CreateWithAudit(
		context.Context, CreateRequest, auth.Fingerprint, int64,
		LifecycleReceipt, time.Time,
	) (Subscription, LifecycleReceipt, bool, error)
	DeleteWithAudit(
		context.Context, []byte, shoal.ID, uint64, time.Time,
		LifecycleReceipt, time.Time,
	) (Subscription, LifecycleReceipt, bool, error)
}

func cloneEvent(event Event) Event {
	result := event
	result.EventID = cloneBytes(event.EventID)
	result.ProducerID = cloneBytes(event.ProducerID)
	result.ActionID = cloneBytes(event.ActionID)
	result.TransitionID = cloneBytes(event.TransitionID)
	result.CorrelationID = cloneBytes(event.CorrelationID)
	result.Evidence = cloneEvidence(event.Evidence)
	result.ConsumedEvidence = cloneEvidenceReferences(event.ConsumedEvidence)
	result.CitedEvidence = cloneEvidenceReferences(event.CitedEvidence)
	result.ConsumedEvidenceVisibility = cloneVisibilityGroup(
		event.ConsumedEvidenceVisibility)
	result.CitedEvidenceVisibility = cloneVisibilityGroup(
		event.CitedEvidenceVisibility)
	return result
}

func cloneVisibilityGroup(values [][]string) [][]string {
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

func cloneEvidence(evidence []Evidence) []Evidence {
	result := make([]Evidence, len(evidence))
	for i := range evidence {
		result[i] = Evidence{
			SourceID: cloneBytes(evidence[i].SourceID),
			PolicyID: cloneBytes(evidence[i].PolicyID),
			ObjectID: evidence[i].ObjectID,
			NodeID:   evidence[i].NodeID, EdgeID: evidence[i].EdgeID,
			AnchorID: evidence[i].AnchorID, RevisionID: evidence[i].RevisionID,
			Start: evidence[i].Start, End: evidence[i].End,
		}
		if evidence[i].Reference != nil {
			reference := cloneEvidenceReferences(
				[]interaction.EvidenceReference{*evidence[i].Reference})[0]
			result[i].Reference = &reference
		}
	}
	return result
}

func cloneEvidenceReferences(
	references []interaction.EvidenceReference,
) []interaction.EvidenceReference {
	if len(references) == 0 {
		return nil
	}
	result := make([]interaction.EvidenceReference, len(references))
	for i, reference := range references {
		result[i] = reference
		result[i].NodeIDs = append([]shoal.ID(nil), reference.NodeIDs...)
		result[i].EdgeIDs = append([]shoal.ID(nil), reference.EdgeIDs...)
		result[i].Assertions = append(
			[]interaction.AssertionReference(nil), reference.Assertions...)
	}
	return result
}

func cloneSubscription(subscription Subscription) Subscription {
	result := subscription
	result.ID = cloneBytes(subscription.ID)
	result.Filter = cloneFilter(subscription.Filter)
	return result
}

func cloneFilter(filter Filter) Filter {
	return Filter{
		Kinds:     append([]string(nil), filter.Kinds...),
		SourceIDs: cloneByteSlices(filter.SourceIDs),
		PolicyIDs: cloneByteSlices(filter.PolicyIDs),
	}
}

func normalizeFilter(filter Filter) (Filter, error) {
	if len(filter.Kinds) > MaxKinds || len(filter.SourceIDs) > auth.MaxDecisionGrantIDs ||
		len(filter.PolicyIDs) > auth.MaxDecisionGrantIDs {
		return Filter{}, shoal.NewError(shoal.ErrorInvalidArgument, "event filter exceeds its bound")
	}
	result := cloneFilter(filter)
	for _, kind := range result.Kinds {
		if err := validateKind(kind); err != nil {
			return Filter{}, err
		}
	}
	for _, values := range [][][]byte{result.SourceIDs, result.PolicyIDs} {
		for _, value := range values {
			if err := validateID("event filter identity", value, false); err != nil {
				return Filter{}, err
			}
		}
	}
	sort.Strings(result.Kinds)
	result.Kinds = uniqueStrings(result.Kinds)
	sort.Slice(result.SourceIDs, func(i, j int) bool { return bytes.Compare(result.SourceIDs[i], result.SourceIDs[j]) < 0 })
	sort.Slice(result.PolicyIDs, func(i, j int) bool { return bytes.Compare(result.PolicyIDs[i], result.PolicyIDs[j]) < 0 })
	result.SourceIDs = uniqueBytes(result.SourceIDs)
	result.PolicyIDs = uniqueBytes(result.PolicyIDs)
	return result, nil
}

func normalizeEvent(event Event, requireSequence bool) (Event, error) {
	result := cloneEvent(event)
	if requireSequence && result.Sequence == 0 {
		return Event{}, shoal.NewError(shoal.ErrorInvalidArgument, "event sequence is required")
	}
	if err := validateID("event ID", result.EventID, !requireSequence); err != nil {
		return Event{}, err
	}
	if err := validateKind(result.Kind); err != nil {
		return Event{}, err
	}
	if result.ProducerGeneration <= 0 {
		return Event{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "event producer generation is required")
	}
	for name, value := range map[string][]byte{
		"event producer ID":    result.ProducerID,
		"event action ID":      result.ActionID,
		"event transition ID":  result.TransitionID,
		"event correlation ID": result.CorrelationID,
	} {
		if err := validateID(name, value, name == "event correlation ID"); err != nil {
			return Event{}, err
		}
	}
	if err := result.Reason.Validate(); err != nil {
		return Event{}, err
	}
	if len(result.Evidence) == 0 || len(result.Evidence) > MaxEvidence {
		return Event{}, shoal.NewError(shoal.ErrorInvalidArgument, "event evidence is outside its bound")
	}
	for i := range result.Evidence {
		if err := validateID("event source ID", result.Evidence[i].SourceID, false); err != nil {
			return Event{}, err
		}
		if err := validateID("event policy ID", result.Evidence[i].PolicyID, false); err != nil {
			return Event{}, err
		}
		if err := shoal.ValidateRequiredID("event object ID", result.Evidence[i].ObjectID); err != nil {
			return Event{}, err
		}
		if err := normalizeEventEvidence(&result.Evidence[i]); err != nil {
			return Event{}, err
		}
	}
	sort.Slice(result.Evidence, func(i, j int) bool {
		return compareEvidence(result.Evidence[i], result.Evidence[j]) < 0
	})
	for i := 1; i < len(result.Evidence); i++ {
		if compareEvidence(result.Evidence[i-1], result.Evidence[i]) == 0 {
			return Event{}, shoal.NewError(shoal.ErrorInvalidArgument, "event evidence contains a duplicate")
		}
	}
	consumed, cited, err := canonicalEvidenceGroups(
		result.ConsumedEvidence, result.CitedEvidence)
	if err != nil {
		return Event{}, err
	}
	result.ConsumedEvidence, result.CitedEvidence = consumed, cited
	// An all-unlabelled group is stored as nil, so an event's shape never
	// depends on whether its author spelled "no labels" as nil or as empty
	// entries, and delivery can return the same shape after withholding.
	result.ConsumedEvidenceVisibility = canonicalVisibilityGroup(
		result.ConsumedEvidenceVisibility)
	result.CitedEvidenceVisibility = canonicalVisibilityGroup(
		result.CitedEvidenceVisibility)
	for _, group := range []struct {
		references []interaction.EvidenceReference
		visibility [][]string
	}{
		{consumed, result.ConsumedEvidenceVisibility},
		{cited, result.CitedEvidenceVisibility},
	} {
		if err := validateEvidenceVisibility(
			group.references, group.visibility); err != nil {
			return Event{}, err
		}
	}
	if err := validateAuthorizationReferences(
		result.Evidence, consumed, cited); err != nil {
		return Event{}, err
	}
	if err := validateLabelledJoinNamesItsReference(result); err != nil {
		return Event{}, err
	}
	if err := validateExactEvidenceCoverage(
		result.Evidence, consumed, cited); err != nil {
		return Event{}, err
	}
	if result.OccurredAt.IsZero() || result.OccurredAt.Location() != time.UTC {
		return Event{}, shoal.NewError(shoal.ErrorInvalidArgument, "event occurrence time must be UTC")
	}
	return result, nil
}

// validateEvidenceVisibility checks that a group's visibility expressions
// align with its references and are each canonical, the same rule
// fleet.validateEvidence applies to an action's evidence.
func validateEvidenceVisibility(
	references []interaction.EvidenceReference, visibility [][]string,
) error {
	if len(visibility) == 0 {
		return nil
	}
	if len(visibility) != len(references) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"event evidence visibility does not align with its references")
	}
	for _, expression := range visibility {
		if len(expression) == 0 {
			continue
		}
		normalized, err := interaction.Conjoin(expression)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(normalized, expression) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"event evidence visibility must be canonical")
		}
	}
	return nil
}

// canonicalVisibilityGroup returns nil for a group with no labelled entry,
// and the group unchanged otherwise.
func canonicalVisibilityGroup(values [][]string) [][]string {
	for _, value := range values {
		if len(value) > 0 {
			return values
		}
	}
	return nil
}

// validateLabelledJoinNamesItsReference refuses an authorization join entry
// that covers an exact identifier of a *labelled* evidence reference by
// ObjectID alone, without naming the reference (#562).
//
// Delivery withholds a labelled reference together with the join entries
// that name it. An entry with no Reference names nothing, so it would be
// delivered — carrying the hidden identifier as its ObjectID. Requiring the
// author to name the reference keeps delivery's drop rule a single, simple
// one: an entry goes with the reference it names.
func validateLabelledJoinNamesItsReference(event Event) error {
	protected := make(map[shoal.ID]struct{})
	for _, group := range []struct {
		references []interaction.EvidenceReference
		visibility [][]string
	}{
		{event.ConsumedEvidence, event.ConsumedEvidenceVisibility},
		{event.CitedEvidence, event.CitedEvidenceVisibility},
	} {
		for i, expression := range group.visibility {
			if len(expression) == 0 {
				continue
			}
			for _, id := range ExactEvidenceReferenceIDs(group.references[i]) {
				if id != "" {
					protected[id] = struct{}{}
				}
			}
		}
	}
	if len(protected) == 0 {
		return nil
	}
	for _, item := range event.Evidence {
		if item.Reference != nil {
			continue
		}
		if _, ok := protected[item.ObjectID]; ok {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"event authorization for labelled evidence must name its reference")
		}
	}
	return nil
}

func compareEvidence(left, right Evidence) int {
	for _, pair := range [][2][]byte{
		{left.SourceID, right.SourceID},
		{left.PolicyID, right.PolicyID},
		{[]byte(left.ObjectID), []byte(right.ObjectID)},
	} {
		if comparison := bytes.Compare(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	return 0
}

func validateAuthorizationReferences(
	authorization []Evidence,
	groups ...[]interaction.EvidenceReference,
) error {
	for _, evidence := range authorization {
		if evidence.Reference == nil {
			continue
		}
		found := false
		for _, group := range groups {
			for _, reference := range group {
				if reflect.DeepEqual(*evidence.Reference, reference) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"event authorization reference is not present in an evidence group")
		}
	}
	return nil
}

func validateExactEvidenceCoverage(
	authorization []Evidence,
	groups ...[]interaction.EvidenceReference,
) error {
	covered := make(map[shoal.ID]struct{}, len(authorization))
	for _, evidence := range authorization {
		covered[evidence.ObjectID] = struct{}{}
		if evidence.Reference != nil {
			for _, id := range ExactEvidenceReferenceIDs(*evidence.Reference) {
				if id != "" {
					covered[id] = struct{}{}
				}
			}
		}
	}
	for _, group := range groups {
		for _, reference := range group {
			for _, id := range ExactEvidenceReferenceIDs(reference) {
				if id == "" {
					continue
				}
				if _, ok := covered[id]; !ok {
					return shoal.NewError(
						shoal.ErrorInvalidArgument,
						fmt.Sprintf(
							"event exact evidence is missing authorization coverage for object %x",
							[]byte(id)))
				}
			}
		}
	}
	return nil
}

func ExactEvidenceReferenceIDs(reference interaction.EvidenceReference) []shoal.ID {
	ids := []shoal.ID{
		reference.AnchorID,
		reference.Citation.DocumentID,
		reference.Citation.RevisionID,
		reference.Citation.SectionID,
		reference.Citation.SpanID,
	}
	ids = append(ids, reference.NodeIDs...)
	ids = append(ids, reference.EdgeIDs...)
	for _, assertion := range reference.Assertions {
		ids = append(ids, assertion.AssertionID, assertion.EdgeID)
	}
	return ids
}

func canonicalEvidenceGroups(
	consumed, cited []interaction.EvidenceReference,
) ([]interaction.EvidenceReference, []interaction.EvidenceReference, error) {
	if len(consumed)+len(cited) > MaxEvidence {
		return nil, nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "event exact evidence exceeds its bound")
	}
	canonicalize := func(
		values []interaction.EvidenceReference,
	) ([]interaction.EvidenceReference, error) {
		result := make([]interaction.EvidenceReference, len(values))
		for i, value := range values {
			canonical, err := value.Canonical()
			if err != nil {
				return nil, err
			}
			result[i] = canonical
		}
		return result, nil
	}
	canonicalConsumed, err := canonicalize(consumed)
	if err != nil {
		return nil, nil, err
	}
	canonicalCited, err := canonicalize(cited)
	if err != nil {
		return nil, nil, err
	}
	session := interaction.Session{
		Turns: []interaction.Turn{{
			Index: 0,
			ToolCall: &interaction.ToolCall{
				Kind:              "fleet.evidence",
				RetrievedEvidence: canonicalConsumed,
			},
		}},
		CitedEvidence: canonicalCited,
	}
	if _, err := session.EvidenceReferences(); err != nil {
		return nil, nil, err
	}
	return canonicalConsumed, canonicalCited, nil
}

func normalizeEventEvidence(value *Evidence) error {
	hasLegacy := value.NodeID != "" || value.EdgeID != "" ||
		value.AnchorID != "" || value.RevisionID != "" ||
		value.Start != 0 || value.End != 0
	if hasLegacy {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"legacy flattened event evidence is not accepted")
	}
	if value.Reference == nil {
		return nil
	}
	canonical, err := value.Reference.Canonical()
	if err != nil {
		return err
	}
	value.Reference = &canonical
	return nil
}

func validateID(name string, value []byte, optional bool) error {
	if len(value) == 0 && optional {
		return nil
	}
	if len(value) == 0 || len(value) > MaxIDBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument, name+" is outside its byte bound")
	}
	return nil
}

func validateKind(kind string) error {
	if kind == "" || len(kind) > MaxKindBytes || !utf8.ValidString(kind) ||
		strings.TrimSpace(kind) != kind {
		return shoal.NewError(shoal.ErrorInvalidArgument, "event kind is invalid")
	}
	for _, r := range kind {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') &&
			r != '.' && r != '_' && r != '-' {
			return shoal.NewError(shoal.ErrorInvalidArgument, "event kind is invalid")
		}
	}
	return nil
}

func cloneBytes(value []byte) []byte { return append([]byte(nil), value...) }

func cloneByteSlices(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for i := range values {
		result[i] = cloneBytes(values[i])
	}
	return result
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func uniqueBytes(values [][]byte) [][]byte {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if !bytes.Equal(value, out[len(out)-1]) {
			out = append(out, value)
		}
	}
	return out
}

func containsBytes(values [][]byte, wanted []byte) bool {
	index := sort.Search(len(values), func(i int) bool { return bytes.Compare(values[i], wanted) >= 0 })
	return index < len(values) && bytes.Equal(values[index], wanted)
}

func containsString(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

func mapContextError(err error) error {
	if errors.Is(err, ErrResyncRequired) {
		return errors.Join(
			ErrResyncRequired,
			shoal.NewError(shoal.ErrorConflict, "event cursor requires resynchronization"),
		)
	}
	if errors.Is(err, ErrCursorInvalid) {
		return errors.Join(
			ErrCursorInvalid,
			shoal.NewError(shoal.ErrorInvalidArgument, "event cursor is invalid"),
		)
	}
	if errors.Is(err, ErrPublicationExpired) ||
		errors.Is(err, ErrMutationExpired) {
		return errors.Join(
			err,
			shoal.NewError(shoal.ErrorConflict, "fleet event retry window expired"),
		)
	}
	if errors.Is(err, ErrRetentionCapacity) {
		return errors.Join(
			ErrRetentionCapacity,
			shoal.NewError(
				shoal.ErrorUnavailable,
				"fleet event retry retention is temporarily at capacity",
			),
		)
	}
	if errors.Is(err, ErrPublicationUnknown) {
		return explorer.MarkIndeterminateCommit(errors.Join(
			ErrPublicationUnknown,
			shoal.NewError(
				shoal.ErrorUnavailable,
				"fleet event publication outcome is unknown",
			),
		))
	}
	if errors.Is(err, context.Canceled) {
		return shoal.WrapError(shoal.ErrorCanceled, "event operation canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return shoal.WrapError(shoal.ErrorDeadline, "event operation deadline exceeded", err)
	}
	return err
}
