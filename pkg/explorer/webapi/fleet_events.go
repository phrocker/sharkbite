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

package webapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type FleetEventService interface {
	Create(context.Context, fleetevents.CreateRequest) (fleetevents.Subscription, error)
	Delete(context.Context, fleetevents.DeleteRequest) error
	Publish(context.Context, fleetevents.PublishRequest) (fleetevents.PublishResult, error)
	Pull(context.Context, fleetevents.PullRequest) (fleetevents.Page, error)
}

// FleetEventsRoutePrefix is the authenticated subtree reserved for fleet
// event publication and subscription delivery.
const FleetEventsRoutePrefix = "/api/v1/fleet/events/"

const maxFleetEventRequestBytes int64 = 512 << 10

// NewFleetEventsHandler constructs the fleet-event subtree. The returned
// handler performs no authentication; it must be mounted through
// Handler.MountAuthenticated so it consumes only the identity and decision
// already bound to the request context.
func NewFleetEventsHandler(service FleetEventService) (http.Handler, error) {
	if isAbsentInterface(service) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet event service is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+FleetEventsRoutePrefix+"subscriptions", func(w http.ResponseWriter, r *http.Request) {
		var input fleetSubscriptionCreateRequest
		if err := decodeFleetEventRequest(w, r, &input); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		request, err := input.domain()
		if err != nil {
			writeError(w, err)
			return
		}
		subscription, err := service.Create(r.Context(), request)
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, http.StatusCreated, fleetSubscriptionResponseFrom(subscription))
	})
	mux.HandleFunc("DELETE "+FleetEventsRoutePrefix+"subscriptions/{subscription}", func(w http.ResponseWriter, r *http.Request) {
		id, err := decodeFleetOpaqueID(r.PathValue("subscription"))
		if err != nil {
			writeError(w, err)
			return
		}
		var input fleetSubscriptionDeleteRequest
		if err := decodeFleetEventRequest(w, r, &input); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		if err := service.Delete(r.Context(), fleetevents.DeleteRequest{
			SubscriptionID: id, ExpectedGeneration: input.ExpectedGeneration,
			RetryUntil: input.RetryUntil,
		}); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+FleetEventsRoutePrefix+"publish", func(w http.ResponseWriter, r *http.Request) {
		var input fleetEventPublishRequest
		if err := decodeFleetEventRequest(w, r, &input); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		request, err := input.domain()
		if err != nil {
			writeError(w, err)
			return
		}
		result, err := service.Publish(r.Context(), request)
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, http.StatusCreated, fleetEventPublishResponse{
			EventID: encodeFleetOpaqueID(result.EventID), Sequence: result.Sequence, Repeated: result.Repeated,
		})
	})
	mux.HandleFunc("POST "+FleetEventsRoutePrefix+"subscriptions/{subscription}/pull", func(w http.ResponseWriter, r *http.Request) {
		id, err := decodeFleetOpaqueID(r.PathValue("subscription"))
		if err != nil {
			writeError(w, err)
			return
		}
		var input fleetEventPullRequest
		if err := decodeFleetEventRequest(w, r, &input); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		if input.WaitMilliseconds < 0 ||
			input.WaitMilliseconds > int64(fleetevents.MaxLongPollWait/time.Millisecond) {
			writeError(w, shoal.NewError(
				shoal.ErrorInvalidArgument, "event pull wait is outside its bound"))
			return
		}
		page, err := service.Pull(r.Context(), fleetevents.PullRequest{
			SubscriptionID: id, Cursor: input.Cursor, Limit: input.Limit,
			Wait: time.Duration(input.WaitMilliseconds) * time.Millisecond,
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeResponse(w, http.StatusOK, fleetEventPageFrom(page))
	})
	return mux, nil
}

func decodeFleetEventRequest(writer http.ResponseWriter, request *http.Request, value any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxFleetEventRequestBytes)
	return decodeRequest(writer, request, value)
}

// MountFleetEvents mounts the event subtree once behind the Handler's host and
// authentication gates.
func (h *Handler) MountFleetEvents(service FleetEventService) error {
	handler, err := NewFleetEventsHandler(service)
	if err != nil {
		return err
	}
	return h.MountAuthenticated(FleetEventsRoutePrefix, handler)
}

type fleetFilter struct {
	Kinds     []string `json:"kinds,omitempty"`
	SourceIDs []string `json:"source_ids,omitempty"`
	PolicyIDs []string `json:"policy_ids,omitempty"`
}

type fleetSubscriptionCreateRequest struct {
	Token           string      `json:"token"`
	SubscriberID    string      `json:"subscriber_id,omitempty"`
	AgentID         string      `json:"agent_id"`
	AgentGeneration int64       `json:"agent_generation"`
	Filter          fleetFilter `json:"filter,omitempty"`
	TTLSeconds      int64       `json:"ttl_seconds,omitempty"`
	RetryUntil      time.Time   `json:"retry_until"`
}

func (r fleetSubscriptionCreateRequest) domain() (fleetevents.CreateRequest, error) {
	if r.TTLSeconds < 0 ||
		r.TTLSeconds > int64(fleetevents.MaxSubscriptionTTL/time.Second) {
		return fleetevents.CreateRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "subscription TTL is outside its bound")
	}
	token, err := decodeFleetOpaqueID(r.Token)
	if err != nil {
		return fleetevents.CreateRequest{}, err
	}
	agentID, err := decodeID(r.AgentID)
	if err != nil {
		return fleetevents.CreateRequest{}, err
	}
	filter, err := r.Filter.domain()
	if err != nil {
		return fleetevents.CreateRequest{}, err
	}
	var subscriberID shoal.ID
	if r.SubscriberID != "" {
		subscriberID, err = decodeID(r.SubscriberID)
		if err != nil {
			return fleetevents.CreateRequest{}, err
		}
	}
	return fleetevents.CreateRequest{
		Token: token, SubscriberID: subscriberID, AgentID: agentID,
		AgentGeneration: r.AgentGeneration,
		Filter:          filter, TTL: time.Duration(r.TTLSeconds) * time.Second,
		RetryUntil: r.RetryUntil,
	}, nil
}

func (f fleetFilter) domain() (fleetevents.Filter, error) {
	sources, err := decodeFleetOpaqueIDs(f.SourceIDs)
	if err != nil {
		return fleetevents.Filter{}, err
	}
	policies, err := decodeFleetOpaqueIDs(f.PolicyIDs)
	if err != nil {
		return fleetevents.Filter{}, err
	}
	return fleetevents.Filter{Kinds: f.Kinds, SourceIDs: sources, PolicyIDs: policies}, nil
}

type fleetSubscriptionDeleteRequest struct {
	ExpectedGeneration uint64    `json:"expected_generation"`
	RetryUntil         time.Time `json:"retry_until"`
}

type fleetEvidence struct {
	SourceID   string                  `json:"source_id"`
	PolicyID   string                  `json:"policy_id"`
	ObjectID   string                  `json:"object_id"`
	Reference  *fleetEvidenceReference `json:"reference,omitempty"`
	NodeID     string                  `json:"node_id,omitempty"`
	EdgeID     string                  `json:"edge_id,omitempty"`
	AnchorID   string                  `json:"anchor_id,omitempty"`
	RevisionID string                  `json:"revision_id,omitempty"`
	Start      int64                   `json:"start,omitempty"`
	End        int64                   `json:"end,omitempty"`
}

type fleetEvidenceReference struct {
	AnchorID   string                    `json:"anchor_id"`
	Kind       interaction.EvidenceKind  `json:"kind"`
	Citation   fleetCitation             `json:"citation"`
	NodeIDs    []string                  `json:"node_ids,omitempty"`
	EdgeIDs    []string                  `json:"edge_ids,omitempty"`
	Assertions []fleetAssertionReference `json:"assertions,omitempty"`
}

type fleetCitation struct {
	DocumentID string `json:"document_id,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	SectionID  string `json:"section_id,omitempty"`
	SpanID     string `json:"span_id,omitempty"`
	Start      int64  `json:"start,omitempty"`
	End        int64  `json:"end,omitempty"`
	StartPage  int32  `json:"start_page,omitempty"`
	EndPage    int32  `json:"end_page,omitempty"`
}

type fleetAssertionReference struct {
	AssertionID string                   `json:"assertion_id"`
	EdgeID      string                   `json:"edge_id"`
	Origin      ontology.AssertionOrigin `json:"origin"`
}

type fleetEventPublishRequest struct {
	Token              string                   `json:"token"`
	Kind               string                   `json:"kind"`
	ProducerID         string                   `json:"producer_id"`
	ProducerGeneration int64                    `json:"producer_generation"`
	ActionID           string                   `json:"action_id"`
	TransitionID       string                   `json:"transition_id"`
	CorrelationID      string                   `json:"correlation_id,omitempty"`
	ReasonCode         string                   `json:"reason_code,omitempty"`
	ReasonDigest       string                   `json:"reason_digest,omitempty"`
	Evidence           []fleetEvidence          `json:"evidence"`
	ConsumedEvidence   []fleetEvidenceReference `json:"consumed_evidence,omitempty"`
	CitedEvidence      []fleetEvidenceReference `json:"cited_evidence,omitempty"`
	OccurredAt         time.Time                `json:"occurred_at"`
	RetryUntil         time.Time                `json:"retry_until"`
}

func (r fleetEventPublishRequest) domain() (fleetevents.PublishRequest, error) {
	token, err := decodeFleetOpaqueID(r.Token)
	if err != nil {
		return fleetevents.PublishRequest{}, err
	}
	producer, err := decodeFleetOpaqueID(r.ProducerID)
	if err != nil {
		return fleetevents.PublishRequest{}, err
	}
	action, err := decodeFleetOpaqueID(r.ActionID)
	if err != nil {
		return fleetevents.PublishRequest{}, err
	}
	transition, err := decodeFleetOpaqueID(r.TransitionID)
	if err != nil {
		return fleetevents.PublishRequest{}, err
	}
	var correlation []byte
	if r.CorrelationID != "" {
		correlation, err = decodeFleetOpaqueID(r.CorrelationID)
		if err != nil {
			return fleetevents.PublishRequest{}, err
		}
	}
	evidence := make([]fleetevents.Evidence, len(r.Evidence))
	for i := range r.Evidence {
		evidence[i].SourceID, err = decodeFleetOpaqueID(r.Evidence[i].SourceID)
		if err != nil {
			return fleetevents.PublishRequest{}, err
		}
		evidence[i].PolicyID, err = decodeFleetOpaqueID(r.Evidence[i].PolicyID)
		if err != nil {
			return fleetevents.PublishRequest{}, err
		}
		evidence[i].ObjectID, err = decodeID(r.Evidence[i].ObjectID)
		if err != nil {
			return fleetevents.PublishRequest{}, err
		}
		if r.Evidence[i].Reference != nil {
			references, decodeErr := fleetEvidenceReferencesDomain(
				[]fleetEvidenceReference{*r.Evidence[i].Reference})
			if decodeErr != nil {
				return fleetevents.PublishRequest{}, decodeErr
			}
			evidence[i].Reference = &references[0]
		}
		for _, field := range []struct {
			input  string
			target *shoal.ID
		}{
			{r.Evidence[i].NodeID, &evidence[i].NodeID},
			{r.Evidence[i].EdgeID, &evidence[i].EdgeID},
			{r.Evidence[i].AnchorID, &evidence[i].AnchorID},
			{r.Evidence[i].RevisionID, &evidence[i].RevisionID},
		} {
			if field.input == "" {
				continue
			}
			*field.target, err = decodeID(field.input)
			if err != nil {
				return fleetevents.PublishRequest{}, err
			}
		}
		evidence[i].Start = r.Evidence[i].Start
		evidence[i].End = r.Evidence[i].End
	}
	consumed, err := fleetEvidenceReferencesDomain(r.ConsumedEvidence)
	if err != nil {
		return fleetevents.PublishRequest{}, err
	}
	cited, err := fleetEvidenceReferencesDomain(r.CitedEvidence)
	if err != nil {
		return fleetevents.PublishRequest{}, err
	}
	return fleetevents.PublishRequest{Token: token, RetryUntil: r.RetryUntil, Event: fleetevents.Event{
		Kind: r.Kind, ProducerID: producer, ProducerGeneration: r.ProducerGeneration,
		ActionID: action, TransitionID: transition, CorrelationID: correlation,
		Reason:   interaction.Reason{Code: r.ReasonCode, Digest: r.ReasonDigest},
		Evidence: evidence, ConsumedEvidence: consumed, CitedEvidence: cited,
		OccurredAt: r.OccurredAt,
	}}, nil
}

type fleetEventPullRequest struct {
	Cursor           string `json:"cursor,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	WaitMilliseconds int64  `json:"wait_milliseconds,omitempty"`
}

type fleetSubscriptionResponse struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agent_id"`
	AgentGeneration int64     `json:"agent_generation"`
	Generation      uint64    `json:"generation"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func fleetSubscriptionResponseFrom(value fleetevents.Subscription) fleetSubscriptionResponse {
	return fleetSubscriptionResponse{
		ID: encodeFleetOpaqueID(value.ID), AgentID: encodeID(value.AgentID),
		AgentGeneration: value.AgentGeneration, Generation: value.Generation,
		CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt,
	}
}

type fleetEventPublishResponse struct {
	EventID  string `json:"event_id"`
	Sequence uint64 `json:"sequence"`
	Repeated bool   `json:"repeated"`
}

type fleetEventResponse struct {
	Sequence           uint64                   `json:"sequence"`
	EventID            string                   `json:"event_id"`
	Kind               string                   `json:"kind"`
	ProducerID         string                   `json:"producer_id"`
	ProducerGeneration int64                    `json:"producer_generation"`
	ActionID           string                   `json:"action_id"`
	TransitionID       string                   `json:"transition_id"`
	CorrelationID      string                   `json:"correlation_id,omitempty"`
	ReasonCode         string                   `json:"reason_code,omitempty"`
	ReasonDigest       string                   `json:"reason_digest,omitempty"`
	Evidence           []fleetEvidence          `json:"evidence"`
	ConsumedEvidence   []fleetEvidenceReference `json:"consumed_evidence,omitempty"`
	CitedEvidence      []fleetEvidenceReference `json:"cited_evidence,omitempty"`
	OccurredAt         time.Time                `json:"occurred_at"`
}

type fleetEventPage struct {
	Events      []fleetEventResponse `json:"events"`
	NextCursor  string               `json:"next_cursor"`
	HighWater   uint64               `json:"high_water"`
	AtLeastOnce bool                 `json:"at_least_once"`
}

func fleetEventPageFrom(page fleetevents.Page) fleetEventPage {
	result := fleetEventPage{
		Events: make([]fleetEventResponse, len(page.Events)), NextCursor: page.NextCursor,
		HighWater: page.HighWater, AtLeastOnce: page.AtLeastOnce,
	}
	for i, event := range page.Events {
		item := fleetEventResponse{
			Sequence: event.Sequence, EventID: encodeFleetOpaqueID(event.EventID), Kind: event.Kind,
			ProducerID:         encodeFleetOpaqueID(event.ProducerID),
			ProducerGeneration: event.ProducerGeneration,
			ActionID:           encodeFleetOpaqueID(event.ActionID),
			TransitionID:       encodeFleetOpaqueID(event.TransitionID),
			ReasonCode:         event.Reason.Code, ReasonDigest: event.Reason.Digest,
			Evidence: make([]fleetEvidence, len(event.Evidence)), OccurredAt: event.OccurredAt,
			ConsumedEvidence: fleetEvidenceReferencesFrom(event.ConsumedEvidence),
			CitedEvidence:    fleetEvidenceReferencesFrom(event.CitedEvidence),
		}
		if len(event.CorrelationID) > 0 {
			item.CorrelationID = encodeFleetOpaqueID(event.CorrelationID)
		}
		for j, evidence := range event.Evidence {
			item.Evidence[j] = fleetEvidence{
				SourceID: encodeFleetOpaqueID(evidence.SourceID), PolicyID: encodeFleetOpaqueID(evidence.PolicyID),
				ObjectID:   encodeID(evidence.ObjectID),
				NodeID:     encodeOptionalFleetID(evidence.NodeID),
				EdgeID:     encodeOptionalFleetID(evidence.EdgeID),
				AnchorID:   encodeOptionalFleetID(evidence.AnchorID),
				RevisionID: encodeOptionalFleetID(evidence.RevisionID),
				Start:      evidence.Start, End: evidence.End,
			}
			if evidence.Reference != nil {
				reference := fleetEvidenceReferencesFrom(
					[]interaction.EvidenceReference{*evidence.Reference})[0]
				item.Evidence[j].Reference = &reference
			}
		}
		result.Events[i] = item
	}
	return result
}

func fleetEvidenceReferencesDomain(
	values []fleetEvidenceReference,
) ([]interaction.EvidenceReference, error) {
	result := make([]interaction.EvidenceReference, len(values))
	for i, value := range values {
		anchorID, err := decodeID(value.AnchorID)
		if err != nil {
			return nil, err
		}
		result[i] = interaction.EvidenceReference{
			AnchorID: anchorID, Kind: value.Kind,
			Citation: document.Citation{
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: value.Citation.Start, Page: value.Citation.StartPage},
					End:   document.SourcePosition{Offset: value.Citation.End, Page: value.Citation.EndPage},
				},
			},
		}
		for _, field := range []struct {
			input  string
			target *shoal.ID
		}{
			{value.Citation.DocumentID, &result[i].Citation.DocumentID},
			{value.Citation.RevisionID, &result[i].Citation.RevisionID},
			{value.Citation.SectionID, &result[i].Citation.SectionID},
			{value.Citation.SpanID, &result[i].Citation.SpanID},
		} {
			if field.input == "" {
				continue
			}
			*field.target, err = decodeID(field.input)
			if err != nil {
				return nil, err
			}
		}
		result[i].NodeIDs, err = decodeEventEvidenceIDs(value.NodeIDs)
		if err != nil {
			return nil, err
		}
		result[i].EdgeIDs, err = decodeEventEvidenceIDs(value.EdgeIDs)
		if err != nil {
			return nil, err
		}
		result[i].Assertions = make([]interaction.AssertionReference, len(value.Assertions))
		for j, assertion := range value.Assertions {
			assertionID, err := decodeID(assertion.AssertionID)
			if err != nil {
				return nil, err
			}
			edgeID, err := decodeID(assertion.EdgeID)
			if err != nil {
				return nil, err
			}
			result[i].Assertions[j] = interaction.AssertionReference{
				AssertionID: assertionID, EdgeID: edgeID, Origin: assertion.Origin,
			}
		}
	}
	return result, nil
}

func fleetEvidenceReferencesFrom(
	values []interaction.EvidenceReference,
) []fleetEvidenceReference {
	result := make([]fleetEvidenceReference, len(values))
	for i, value := range values {
		result[i] = fleetEvidenceReference{
			AnchorID: encodeID(value.AnchorID), Kind: value.Kind,
			Citation: fleetCitation{
				DocumentID: encodeOptionalFleetID(value.Citation.DocumentID),
				RevisionID: encodeOptionalFleetID(value.Citation.RevisionID),
				SectionID:  encodeOptionalFleetID(value.Citation.SectionID),
				SpanID:     encodeOptionalFleetID(value.Citation.SpanID),
				Start:      value.Citation.Range.Start.Offset,
				End:        value.Citation.Range.End.Offset,
				StartPage:  value.Citation.Range.Start.Page,
				EndPage:    value.Citation.Range.End.Page,
			},
			NodeIDs:    encodeEventEvidenceIDs(value.NodeIDs),
			EdgeIDs:    encodeEventEvidenceIDs(value.EdgeIDs),
			Assertions: make([]fleetAssertionReference, len(value.Assertions)),
		}
		for j, assertion := range value.Assertions {
			result[i].Assertions[j] = fleetAssertionReference{
				AssertionID: encodeID(assertion.AssertionID),
				EdgeID:      encodeID(assertion.EdgeID), Origin: assertion.Origin,
			}
		}
	}
	return result
}

func encodeEventEvidenceIDs(values []shoal.ID) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = encodeID(value)
	}
	return result
}

func decodeEventEvidenceIDs(values []string) ([]shoal.ID, error) {
	result := make([]shoal.ID, len(values))
	for i, value := range values {
		decoded, err := decodeID(value)
		if err != nil {
			return nil, err
		}
		result[i] = decoded
	}
	return result, nil
}

func encodeOptionalFleetID(value shoal.ID) string {
	if value == "" {
		return ""
	}
	return encodeID(value)
}

func encodeFleetOpaqueID(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeFleetOpaqueID(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > fleetevents.MaxIDBytes {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet ID is invalid")
	}
	return decoded, nil
}

func decodeFleetOpaqueIDs(values []string) ([][]byte, error) {
	result := make([][]byte, len(values))
	for i := range values {
		decoded, err := decodeFleetOpaqueID(values[i])
		if err != nil {
			return nil, err
		}
		result[i] = decoded
	}
	return result, nil
}
