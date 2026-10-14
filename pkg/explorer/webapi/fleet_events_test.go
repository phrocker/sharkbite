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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestNewFleetEventsHandlerRejectsTypedNilService(t *testing.T) {
	var service *fleetTransportService
	if _, err := NewFleetEventsHandler(service); err == nil {
		t.Fatal("typed-nil fleet event service succeeded")
	}
}

func TestCombinedFleetAndEventMountsRemainIsolated(t *testing.T) {
	now := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentResolve, auth.OperationDispatch,
			auth.OperationSubscriptionCreate,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(
		&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	registry := &stubFleetProvider{resolver: authority.Resolver()}
	dispatch := &stubDispatchProvider{resolver: authority.Resolver()}
	combined, err := NewFleetHandler(registry, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(FleetRoutePrefix, combined); err != nil {
		t.Fatal(err)
	}
	events := &fleetTransportService{id: []byte("subscription")}
	if err := handler.MountFleetEvents(events); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"http://example.test/api/v1/fleet/agents/YWdlbnQ/resolve",
		strings.NewReader(`{"request_id":"cmVxdWVzdA","reason_code":"resolve","deadline":"2026-09-06T01:01:00Z"}`),
	)
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !registry.resolved {
		t.Fatalf("registry status/provider = %d/%v body=%s", response.Code, registry.resolved, response.Body.String())
	}

	actionBody, _ := json.Marshal(fleetEnqueueWire{
		Context: fleetRequestContextWire{
			RequestID: "cmVxdWVzdA", ReasonCode: "dispatch",
			CorrelationID: "Y29ycmVsYXRpb24", Deadline: now.Add(time.Minute),
		},
		ID: "YWN0aW9u", IdempotencyKey: "a2V5", AgentID: "YWdlbnQ",
		AgentGeneration: 1, Capability: "search", Action: "query",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: encodeFleetID("object"), Input: json.RawMessage(`{"value":1}`),
	})
	request = httptest.NewRequest(
		http.MethodPost, "http://example.test/api/v1/fleet/actions",
		bytes.NewReader(actionBody),
	)
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !dispatch.enqueued {
		t.Fatalf("dispatch status/provider = %d/%v body=%s", response.Code, dispatch.enqueued, response.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"http://example.test/api/v1/fleet/events/subscriptions",
		strings.NewReader(`{"token":"dG9rZW4","agent_id":"YWdlbnQ","agent_generation":1,"ttl_seconds":60,"retry_until":"2026-09-06T21:00:00Z"}`),
	)
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || events.created.AgentID != "agent" {
		t.Fatalf("events status/agent = %d/%q body=%s", response.Code, events.created.AgentID, response.Body.String())
	}
}

func TestMountAuthenticatedRejectsUnsafeConfigurationAtomically(t *testing.T) {
	unauthenticated, err := NewHandler(&stubWorkspaceService{}, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := unauthenticated.MountAuthenticated(
		"/mounted/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("unauthenticated mount error = %v", err)
	}

	now := time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{auth.OperationRetrieve},
		PolicyGeneration:  1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(
		&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		"/api/v1/auth-config/",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("normalized public mount error = %v", err)
	}
	for _, pattern := range []string{
		"/api/v1/foo/../bar/", "/api/v1/meta", "/api/v1/ontology/custom/",
		"/api/v1/workspaces/", "/api/v1/workspaces/custom/",
	} {
		if err := handler.MountAuthenticated(
			pattern, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("unsafe mount %q error = %v", pattern, err)
		}
	}
	first := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if err := handler.MountAuthenticated("/mounted/", first); err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		"/mounted/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("duplicate mount error = %v", err)
	}
	if err := handler.MountAuthenticated(
		"/mounted", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("normalized duplicate mount error = %v", err)
	}
	if reflect.ValueOf(handler.mountedHandler("/mounted/path")).Pointer() !=
		reflect.ValueOf(first).Pointer() {
		t.Fatal("failed mount replaced the previously registered handler")
	}
}

func TestFleetRoutesRoundTripReturnedOpaqueID(t *testing.T) {
	now := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationSubscriptionCreate,
			auth.OperationSubscriptionDelete,
			auth.OperationEventPublish,
		},
		PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(
		fleetBaseService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	service := &fleetTransportService{id: []byte{0, 0xff, '/', 0x80}}
	if err := handler.MountFleetEvents(service); err != nil {
		t.Fatal(err)
	}
	body := `{"token":"dG9rZW4","agent_id":"YWdlbnQ","agent_generation":9,"ttl_seconds":60,"retry_until":"2026-09-06T21:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/fleet/events/subscriptions", bytes.NewBufferString(body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status/body = %d/%s", response.Code, response.Body.String())
	}
	var created fleetSubscriptionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustDecodeFleetID(t, created.ID), service.id) {
		t.Fatalf("returned ID = %q", created.ID)
	}
	if service.created.AgentID != "agent" || service.created.AgentGeneration != 9 {
		t.Fatalf("created agent binding = %q/%d", service.created.AgentID, service.created.AgentGeneration)
	}
	if !service.created.RetryUntil.Equal(
		time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("created retry deadline = %v", service.created.RetryUntil)
	}
	deleteBody := `{"expected_generation":1,"retry_until":"2026-09-06T21:00:00Z"}`
	request = httptest.NewRequest(http.MethodDelete,
		"http://example.test/api/v1/fleet/events/subscriptions/"+created.ID,
		bytes.NewBufferString(deleteBody))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent ||
		!bytes.Equal(service.deleted.SubscriptionID, service.id) ||
		!service.deleted.RetryUntil.Equal(
			time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("delete status/request = %d/%#v", response.Code, service.deleted)
	}
}

func TestFleetEventTransportRoundTripsPublicIDs(t *testing.T) {
	subscriber := shoal.ID(string([]byte{0, 0xff, 's'}))
	agent := shoal.ID(string([]byte{0xff, 0, 'a'}))
	object := shoal.ID(string([]byte{0x80, 0, 'o'}))
	create, err := (fleetSubscriptionCreateRequest{
		Token: "dG9rZW4", AgentID: "YWdlbnQ", AgentGeneration: 9,
		SubscriberID: encodeID(subscriber),
		RetryUntil:   time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC),
	}).domain()
	if err != nil {
		t.Fatal(err)
	}

	if create.SubscriberID != subscriber {
		t.Fatalf("subscriber ID = %x", []byte(create.SubscriberID))
	}
	if create.AgentID != "agent" || create.AgentGeneration != 9 {
		t.Fatalf("agent binding = %q/%d", create.AgentID, create.AgentGeneration)
	}
	subscriptionWire := fleetSubscriptionResponseFrom(fleetevents.Subscription{
		ID: []byte("subscription"), AgentID: agent,
	})
	decodedAgent, err := decodeID(subscriptionWire.AgentID)
	if err != nil || decodedAgent != agent {
		t.Fatalf("wire agent ID = %q, %v", subscriptionWire.AgentID, err)
	}
	publish, err := (fleetEventPublishRequest{
		Token: "dG9rZW4", Kind: "agent.completed", ProducerID: "cHJvZHVjZXI",
		ProducerGeneration: 9, ActionID: "YWN0aW9u",
		TransitionID: "dHJhbnNpdGlvbg", OccurredAt: time.Now().UTC(),
		RetryUntil: time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC),
		Evidence: []fleetEvidence{{
			SourceID: "c291cmNl", PolicyID: "cG9saWN5", ObjectID: encodeID(object),
			Reference: &fleetEvidenceReference{
				AnchorID: encodeID("graph-anchor"), Kind: interaction.EvidenceGraph,
				NodeIDs: []string{encodeID("node-a"), encodeID("node-b")},
				EdgeIDs: []string{encodeID("edge")},
				Assertions: []fleetAssertionReference{{
					AssertionID: encodeID("assertion"), EdgeID: encodeID("edge"),
					Origin: ontology.AssertionExplicit,
				}},
			},
		}},
		ConsumedEvidence: []fleetEvidenceReference{{
			AnchorID: encodeID("graph-anchor"), Kind: interaction.EvidenceGraph,
			NodeIDs: []string{encodeID("node-a"), encodeID("node-b")},
			EdgeIDs: []string{encodeID("edge")},
			Assertions: []fleetAssertionReference{{
				AssertionID: encodeID("assertion"), EdgeID: encodeID("edge"),
				Origin: ontology.AssertionExplicit,
			}},
		}},
		CitedEvidence: []fleetEvidenceReference{{
			AnchorID: encodeID("document-anchor"), Kind: interaction.EvidenceDocument,
			Citation: fleetCitation{
				DocumentID: encodeID("document"), RevisionID: encodeID("revision"),
				SectionID: encodeID("section"), SpanID: encodeID("span"),
				Start: 3, End: 9, StartPage: 1, EndPage: 2,
			},
			NodeIDs: []string{encodeID("document"), encodeID("section"), encodeID("span")},
		}},
	}).domain()
	if err != nil {
		t.Fatal(err)
	}
	if publish.Event.Evidence[0].ObjectID != object ||
		publish.Event.Evidence[0].Reference == nil ||
		!reflect.DeepEqual(publish.Event.Evidence[0].Reference.NodeIDs,
			[]shoal.ID{"node-a", "node-b"}) ||
		len(publish.Event.ConsumedEvidence) != 1 ||
		publish.Event.ConsumedEvidence[0].AnchorID != "graph-anchor" ||
		!reflect.DeepEqual(publish.Event.ConsumedEvidence[0].NodeIDs,
			[]shoal.ID{"node-a", "node-b"}) ||
		len(publish.Event.ConsumedEvidence[0].Assertions) != 1 ||
		len(publish.Event.CitedEvidence) != 1 ||
		publish.Event.CitedEvidence[0].Citation.RevisionID != "revision" ||
		publish.Event.CitedEvidence[0].Citation.Range.Start.Page != 1 ||
		publish.Event.ProducerGeneration != 9 ||
		!publish.RetryUntil.Equal(time.Date(2026, 9, 6, 21, 0, 0, 0, time.UTC)) ||
		!bytes.Equal(publish.Event.TransitionID, []byte("transition")) {
		t.Fatalf("publish event = %#v", publish.Event)
	}
	wire := fleetEventPageFrom(fleetevents.Page{Events: []fleetevents.Event{{
		EventID: []byte("event"), Kind: "agent.completed", ProducerID: []byte("producer"),
		ProducerGeneration: 9, ActionID: []byte("action"),
		TransitionID: []byte("transition"),
		Evidence: []fleetevents.Evidence{{
			SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: object,
			Reference: &interaction.EvidenceReference{
				AnchorID: "graph-anchor", Kind: interaction.EvidenceGraph,
				NodeIDs: []shoal.ID{"node-a", "node-b"}, EdgeIDs: []shoal.ID{"edge"},
				Assertions: []interaction.AssertionReference{{
					AssertionID: "assertion", EdgeID: "edge",
					Origin: ontology.AssertionExplicit,
				}},
			},
		}},
		ConsumedEvidence: []interaction.EvidenceReference{{
			AnchorID: "graph-anchor", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"node-a", "node-b"}, EdgeIDs: []shoal.ID{"edge"},
			Assertions: []interaction.AssertionReference{{
				AssertionID: "assertion", EdgeID: "edge", Origin: ontology.AssertionExplicit,
			}},
		}},
		CitedEvidence: []interaction.EvidenceReference{{
			AnchorID: "document-anchor", Kind: interaction.EvidenceDocument,
			Citation: document.Citation{
				DocumentID: "document", RevisionID: "revision",
				SectionID: "section", SpanID: "span",
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: 3, Page: 1},
					End:   document.SourcePosition{Offset: 9, Page: 2},
				},
			},
			NodeIDs: []shoal.ID{"document", "section", "span"},
		}},
	}}})
	decoded, err := decodeID(wire.Events[0].Evidence[0].ObjectID)
	if err != nil || decoded != object {
		t.Fatalf("wire object ID = %q, %v", wire.Events[0].Evidence[0].ObjectID, err)
	}
	if wire.Events[0].ProducerGeneration != 9 ||
		wire.Events[0].TransitionID != "dHJhbnNpdGlvbg" ||
		wire.Events[0].Evidence[0].Reference == nil ||
		!reflect.DeepEqual(wire.Events[0].Evidence[0].Reference.NodeIDs,
			[]string{encodeID("node-a"), encodeID("node-b")}) ||
		len(wire.Events[0].ConsumedEvidence) != 1 ||
		wire.Events[0].ConsumedEvidence[0].Assertions[0].Origin != ontology.AssertionExplicit ||
		len(wire.Events[0].CitedEvidence) != 1 ||
		wire.Events[0].CitedEvidence[0].Citation.SpanID != encodeID("span") {
		t.Fatalf("wire transition identity = %#v", wire.Events[0])
	}
}

func TestFleetEventTransportRejectsDurationOverflow(t *testing.T) {
	if _, err := (fleetSubscriptionCreateRequest{
		Token: "dG9rZW4", AgentID: "YWdlbnQ", AgentGeneration: 1,
		TTLSeconds: int64(^uint64(0) >> 1),
	}).domain(); err == nil {
		t.Fatal("overflowing subscription TTL succeeded")
	}
	handler, err := NewFleetEventsHandler(&fleetTransportService{
		id: []byte("subscription"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://example.test"+FleetEventsRoutePrefix+"subscriptions/c3Vic2NyaXB0aW9u/pull",
		strings.NewReader(`{"wait_milliseconds":9223372036854775807}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("overflowing wait status = %d, body=%s",
			response.Code, response.Body.String())
	}
}

func TestFleetEventHandlerRejectsOversizedRequest(t *testing.T) {
	handler, err := NewFleetEventsHandler(&fleetTransportService{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://example.test"+FleetEventsRoutePrefix+"publish",
		strings.NewReader(`{"token":"`+strings.Repeat("a", int(maxFleetEventRequestBytes))+`"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized request status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func mustDecodeFleetID(t *testing.T, value string) []byte {
	t.Helper()
	result, err := decodeFleetOpaqueID(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type fleetTransportService struct {
	id      []byte
	created fleetevents.CreateRequest
	deleted fleetevents.DeleteRequest
}

func (s *fleetTransportService) Create(
	_ context.Context, request fleetevents.CreateRequest,
) (fleetevents.Subscription, error) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	s.created = request
	return fleetevents.Subscription{
		ID: append([]byte(nil), s.id...), AgentID: request.AgentID,
		AgentGeneration: request.AgentGeneration, Generation: 1,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, nil
}

func (s *fleetTransportService) Delete(_ context.Context, request fleetevents.DeleteRequest) error {
	s.deleted = request
	s.deleted.SubscriptionID = append([]byte(nil), request.SubscriptionID...)
	return nil
}

func (*fleetTransportService) Publish(context.Context, fleetevents.PublishRequest) (fleetevents.PublishResult, error) {
	return fleetevents.PublishResult{}, nil
}

func (*fleetTransportService) Pull(context.Context, fleetevents.PullRequest) (fleetevents.Page, error) {
	return fleetevents.Page{}, nil
}

type fleetBaseService struct{}

func (fleetBaseService) Documents(context.Context, DocumentsRequest) (DocumentsResponse, error) {
	return DocumentsResponse{}, nil
}
func (fleetBaseService) Document(context.Context, DocumentRequest) (DocumentResponse, error) {
	return DocumentResponse{}, nil
}
func (fleetBaseService) Retrieve(context.Context, RetrievalRequest) (RetrievalResponse, error) {
	return RetrievalResponse{}, nil
}
func (fleetBaseService) Neighborhood(context.Context, NeighborhoodRequest) (NeighborhoodResponse, error) {
	return NeighborhoodResponse{}, nil
}
func (fleetBaseService) Path(context.Context, PathRequest) (PathResponse, error) {
	return PathResponse{}, nil
}
