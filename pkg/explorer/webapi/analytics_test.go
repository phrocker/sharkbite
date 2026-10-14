// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package webapi_test

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

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAnalyticsRouteUsesHostAndAuthenticationGuards(t *testing.T) {
	now := time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "reader", Actor: "reader-actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationAnalyticsRead},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &analyticsRouteService{resolver: authority.Resolver()}
	handler, err := webapi.NewAuthenticatedHandler(
		service,
		webapi.AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(),
		"workspace.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	request := webapi.AnalyticsRequest{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{"node"},
			Depth:   1, Direction: explorer.GraphDirectionBoth,
			Fanout: 4, MaxNodes: 8, MaxEdges: 16,
			MaxScannedEdgesPerNode: 32,
		},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(
		http.MethodPost, "http://workspace.test/api/v1/analytics",
		bytes.NewReader(body),
	)
	httpRequest.Host = "workspace.test"
	httpRequest.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	if response.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("analytics response = %d %s, calls=%d",
			response.Code, response.Body.String(), service.calls)
	}
	if !strings.Contains(response.Body.String(), `"recorded":true`) ||
		!strings.Contains(response.Body.String(), `"interaction_id"`) ||
		strings.Contains(response.Body.String(), `"Recorded"`) {
		t.Fatalf("analytics response schema = %s", response.Body.String())
	}
	service.invalid = true
	invalidRequest := httptest.NewRequest(
		http.MethodPost, "http://workspace.test/api/v1/analytics",
		bytes.NewReader(body),
	)
	invalidRequest.Host = "workspace.test"
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusServiceUnavailable ||
		invalidResponse.Header().Get(webapi.CommitOutcomeHeader) !=
			webapi.CommitOutcomeIndeterminate {
		t.Fatalf("invalid provider response = %d %s",
			invalidResponse.Code, invalidResponse.Body.String())
	}
	service.invalid = false

	badHost := httptest.NewRequest(
		http.MethodPost, "http://evil.test/api/v1/analytics",
		bytes.NewReader(body),
	)
	badHost.Host = "evil.test"
	badHost.Header.Set("Content-Type", "application/json")
	badHostResponse := httptest.NewRecorder()
	handler.ServeHTTP(badHostResponse, badHost)
	if badHostResponse.Code != http.StatusMisdirectedRequest || service.calls != 2 {
		t.Fatalf("host guard response = %d, calls=%d",
			badHostResponse.Code, service.calls)
	}

	metaRequest := httptest.NewRequest(
		http.MethodGet, "http://workspace.test/api/v1/meta", nil)
	metaRequest.Host = "workspace.test"
	metaResponse := httptest.NewRecorder()
	handler.ServeHTTP(metaResponse, metaRequest)
	if metaResponse.Code != http.StatusOK ||
		!strings.Contains(metaResponse.Body.String(), `"analytics":true`) ||
		!strings.Contains(metaResponse.Body.String(), `"analytics_limits"`) {
		t.Fatalf("metadata = %d %s", metaResponse.Code, metaResponse.Body.String())
	}
}

func TestAnalyticsWireRoundTripsOpaqueIDs(t *testing.T) {
	request := webapi.AnalyticsRequest{
		Snapshot: webapi.AnalyticsSnapshot{ID: "analytics-sha256:pin"},
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{shoal.ID([]byte{0xff, 0x00})},
			Depth:   1, Direction: explorer.GraphDirectionIncoming,
			Fanout: 2, MaxNodes: 3, MaxEdges: 4,
			MaxScannedEdgesPerNode: 5,
			EdgeTypes:              []string{"related"},
		},
		PageRank: analytics.PageRankOptions{
			DampingFactor: 0.8, ConvergenceTolerance: 1e-9,
			MaxIterations: 50,
		},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte{0xff}) {
		t.Fatalf("opaque ID was emitted without encoding: %q", encoded)
	}
	var decoded webapi.AnalyticsRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", decoded, request)
	}
}

func TestAnalyticsResponseWireRoundTripsOpaqueIDs(t *testing.T) {
	opaque := shoal.ID([]byte{0xff, 0x00})
	response := webapi.AnalyticsResponse{
		Snapshot: webapi.AnalyticsSnapshot{ID: "analytics-sha256:pin"},
		Analytics: analytics.Result{
			Scope: analytics.ScopeMetadata{
				SnapshotID:               "analytics-sha256:pin",
				AuthorizationFingerprint: "auth-sha256:scope",
				PolicyGeneration:         1, SeedNodeIDs: []shoal.ID{opaque},
				Depth: 1, Direction: explorer.GraphDirectionBoth,
				Fanout: 2, MaxNodes: 3, MaxEdges: 4,
				MaxScannedEdgesPerNode: 5,
				UnresolvedAssertions: []analytics.UnresolvedSemantic{{
					AssertionID: opaque,
					Reading:     ontology.OntologyUnresolved,
					Reason:      "ontology identity was not recorded",
				}},
				NodeCount: 1, Complete: true,
			},
			Nodes: []analytics.NodeSummary{{
				NodeID: opaque, PageRank: 1,
				WeakComponentID: "component-sha256:test",
			}},
			WeaklyConnectedComponents: []analytics.ComponentSummary{{
				ID: "component-sha256:test", NodeIDs: []shoal.ID{opaque},
				NodeCount: 1,
			}},
			PageRank: analytics.PageRankSummary{
				DampingFactor:        analytics.DefaultDampingFactor,
				ConvergenceTolerance: analytics.DefaultConvergenceTolerance,
				MaxIterations:        analytics.DefaultMaxIterations,
				Iterations:           1, Converged: true,
			},
			Recording: analytics.RecordingStatus{
				Recorded: true, Required: true, InteractionID: opaque,
			},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte{0xff}) {
		t.Fatalf("opaque response ID was emitted without encoding: %q", encoded)
	}
	var decoded webapi.AnalyticsResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, response) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", decoded, response)
	}
}

func TestEmbeddedAnalyticsIsUnavailableWithoutRequiredRecorderSink(t *testing.T) {
	base, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	service, err := webapi.NewEmbeddedService(&recordlessAnalyticsClient{
		BoundedClient: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.AnalyticsAvailable() {
		t.Fatal("analytics advertised without a durable interaction sink")
	}
	metadata, err := service.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Analytics {
		t.Fatal("analytics capability was true without a durable recorder")
	}
}

func TestRemoteAnalyticsDecodesLimitsAndInvokesUpstream(t *testing.T) {
	limits := analytics.DefaultLimits()
	var fingerprint auth.Fingerprint
	fingerprint[0] = 1
	source := &remoteAnalyticsMaterializer{materialization: analytics.Materialization{
		Snapshot: explorer.Snapshot{
			ID: "internal", AsOf: time.Now().UTC(), Frontier: 1,
		},
		Neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{{ID: "node"}},
		},
		AuthorizationFingerprint: fingerprint,
		PolicyGeneration:         1, RequestID: "request",
		AuthorizationExpiresAt: time.Now().UTC().Add(time.Hour),
		Complete:               true,
	}}
	core, err := analytics.NewService(analytics.Config{
		Source: source, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := webapi.AnalyticsRequest{Scope: analytics.Scope{
		NodeIDs: []shoal.ID{"node"}, Depth: 1,
		Direction: explorer.GraphDirectionBoth,
		Fanout:    4, MaxNodes: 8, MaxEdges: 16,
		MaxScannedEdgesPerNode: 32,
	}}
	result, err := core.Run(context.Background(), analytics.Request{
		Scope: request.Scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	result.Recording = analytics.RecordingStatus{
		Recorded: true, Required: true,
		InteractionID: "interaction-remote",
	}
	expected := webapi.AnalyticsResponse{
		Snapshot:  webapi.AnalyticsSnapshot{ID: result.Scope.SnapshotID},
		Analytics: result,
	}
	analyticsCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		httpRequest *http.Request,
	) {
		switch {
		case httpRequest.Method == http.MethodGet &&
			httpRequest.URL.Path == "/api/v1/meta":
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
				MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
				MaxNodes:                   webapi.MaxNodes,
				AnalyticsLimits:            &limits,
				AnalyticsRecordingRequired: true,
				Capabilities:               webapi.Capabilities{Analytics: true},
			})
		case httpRequest.Method == http.MethodPost &&
			httpRequest.URL.Path == "/api/v1/analytics":
			analyticsCalled = true
			var received webapi.AnalyticsRequest
			if err := json.NewDecoder(httpRequest.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(received, request) {
				t.Fatalf("remote request = %#v, want %#v", received, request)
			}
			writeJSON(t, writer, expected)
		default:
			http.NotFound(writer, httpRequest)
		}
	}))
	defer upstream.Close()
	remote, err := webapi.NewRemoteService(upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Analytics(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !analyticsCalled || !reflect.DeepEqual(got, expected) {
		t.Fatalf("remote analytics = %#v, called=%t", got, analyticsCalled)
	}
	expected.Analytics.Recording.Recorded = false
	if _, err := remote.Analytics(
		context.Background(), request,
	); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("invalid remote analytics commit state = %v", err)
	}
}

func TestRemoteMetadataRequiresValidRecordedAnalyticsLimits(t *testing.T) {
	valid := analytics.DefaultLimits()
	invalid := valid
	invalid.MaxEdges = 0
	tests := []webapi.MetadataResponse{
		{
			MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
			MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
			MaxNodes:                   webapi.MaxNodes,
			AnalyticsRecordingRequired: true,
			Capabilities:               webapi.Capabilities{Analytics: true},
		},
		{
			MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
			MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
			MaxNodes: webapi.MaxNodes, AnalyticsLimits: &invalid,
			AnalyticsRecordingRequired: true,
			Capabilities:               webapi.Capabilities{Analytics: true},
		},
		{
			MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
			MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
			MaxNodes: webapi.MaxNodes, AnalyticsLimits: &valid,
			Capabilities: webapi.Capabilities{Analytics: true},
		},
		{
			MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
			MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
			MaxNodes: webapi.MaxNodes, AnalyticsLimits: &valid,
		},
		{
			MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
			MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
			MaxNodes:                   webapi.MaxNodes,
			AnalyticsRecordingRequired: true,
		},
	}
	for index, metadata := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				request *http.Request,
			) {
				writeJSON(t, writer, metadata)
			}))
			defer upstream.Close()
			remote, err := webapi.NewRemoteService(
				upstream.URL, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := remote.Metadata(
				context.Background(),
			); !shoal.IsErrorCode(err, shoal.ErrorInternal) {
				t.Fatalf("metadata error = %v", err)
			}
		})
	}
}

func TestExtensionOnlyAnalyticsAdvertisesAndServesRemoteClients(t *testing.T) {
	service := &analyticsRouteService{}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(
		service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	remote, err := webapi.NewRemoteService(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := remote.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.Capabilities.Analytics ||
		metadata.AnalyticsLimits == nil ||
		!metadata.AnalyticsRecordingRequired {
		t.Fatalf("extension-only metadata = %+v", metadata)
	}
	request := webapi.AnalyticsRequest{Scope: analytics.Scope{
		NodeIDs: []shoal.ID{"node"}, Depth: 1,
		Direction: explorer.GraphDirectionBoth,
		Fanout:    4, MaxNodes: 8, MaxEdges: 16,
		MaxScannedEdgesPerNode: 32,
	}}
	if _, err := remote.Analytics(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if service.calls != 1 {
		t.Fatalf("remote analytics calls = %d, want 1", service.calls)
	}
}

func TestExplicitCapabilityProviderCanDisableAnalyticsExtension(t *testing.T) {
	service := &disabledAnalyticsService{
		analyticsRouteService: &analyticsRouteService{},
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(
		service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	remote, err := webapi.NewRemoteService(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := remote.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Capabilities.Analytics ||
		metadata.AnalyticsLimits != nil ||
		metadata.AnalyticsRecordingRequired {
		t.Fatalf("explicit-disabled metadata = %+v", metadata)
	}
	request := webapi.AnalyticsRequest{Scope: analytics.Scope{
		NodeIDs: []shoal.ID{"node"}, Depth: 1,
		Direction: explorer.GraphDirectionBoth,
		Fanout:    4, MaxNodes: 8, MaxEdges: 16,
		MaxScannedEdgesPerNode: 32,
	}}
	if _, err := remote.Analytics(
		context.Background(), request,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("explicit-disabled analytics error = %v", err)
	}
	if service.calls != 0 {
		t.Fatalf("disabled analytics reached provider %d times", service.calls)
	}
}

func TestNeighborhoodWireDistinguishesKnownZeroFromUnknownScanCount(t *testing.T) {
	zero := uint32(0)
	public, err := json.Marshal(webapi.NeighborhoodResponse{
		ScannedEdges: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte(`"scanned_edges"`)) {
		t.Fatalf("public scan count was exposed: %s", public)
	}
	known := append(
		append([]byte(nil), bytes.TrimSuffix(public, []byte("}"))...),
		[]byte(`,"scanned_edges":0}`)...,
	)
	if !bytes.Contains(known, []byte(`"scanned_edges":0`)) {
		t.Fatalf("known zero test wire was not constructed: %s", known)
	}
	var decodedKnown webapi.NeighborhoodResponse
	if err := json.Unmarshal(known, &decodedKnown); err != nil {
		t.Fatal(err)
	}
	if decodedKnown.ScannedEdges == nil || *decodedKnown.ScannedEdges != 0 {
		t.Fatalf("known zero scan count = %#v", decodedKnown.ScannedEdges)
	}

	unknown, err := json.Marshal(webapi.NeighborhoodResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(unknown, []byte(`"scanned_edges"`)) {
		t.Fatalf("unknown scan count was advertised: %s", unknown)
	}
	var decodedUnknown webapi.NeighborhoodResponse
	if err := json.Unmarshal(unknown, &decodedUnknown); err != nil {
		t.Fatal(err)
	}
	if decodedUnknown.ScannedEdges != nil {
		t.Fatalf("unknown scan count became known: %#v", decodedUnknown.ScannedEdges)
	}
}

type analyticsRouteService struct {
	gateStubService
	resolver auth.Resolver
	calls    int
	invalid  bool
}

type disabledAnalyticsService struct {
	*analyticsRouteService
}

func (*disabledAnalyticsService) Capabilities(
	context.Context,
) (webapi.Capabilities, error) {
	return webapi.Capabilities{Analytics: false}, nil
}

type remoteAnalyticsMaterializer struct {
	materialization analytics.Materialization
}

func (s *remoteAnalyticsMaterializer) MaterializeAnalytics(
	context.Context,
	explorer.BoundedNeighborhoodRequest,
	uint32,
	uint64,
) (analytics.Materialization, error) {
	return s.materialization, nil
}

func (*remoteAnalyticsMaterializer) RevalidateAnalytics(
	context.Context,
	analytics.Materialization,
) error {
	return nil
}

type recordlessAnalyticsClient struct {
	explorer.BoundedClient
}

func (*recordlessAnalyticsClient) MaterializeAnalytics(
	context.Context,
	explorer.BoundedNeighborhoodRequest,
	uint32,
	uint64,
) (analytics.Materialization, error) {
	return analytics.Materialization{}, nil
}

func (*recordlessAnalyticsClient) RevalidateAnalytics(
	context.Context,
	analytics.Materialization,
) error {
	return nil
}

func (s *analyticsRouteService) AnalyticsLimits() (analytics.Limits, bool) {
	return analytics.DefaultLimits(), true
}

func (*analyticsRouteService) AnalyticsRecordingRequired() bool {
	return true
}

func (s *analyticsRouteService) Analytics(
	ctx context.Context,
	request webapi.AnalyticsRequest,
) (webapi.AnalyticsResponse, error) {
	if s.resolver != nil {
		decision, err := s.resolver.Resolve(ctx)
		if err != nil {
			return webapi.AnalyticsResponse{}, err
		}
		if err := decision.Authorize(
			auth.OperationAnalyticsRead,
			auth.ResourceRequest{AuthorizationDomain: decision.AuthorizationDomain()},
			time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC),
		); err != nil {
			return webapi.AnalyticsResponse{}, err
		}
	}
	s.calls++
	return webapi.AnalyticsResponse{
		Snapshot: webapi.AnalyticsSnapshot{ID: "analytics-sha256:test"},
		Analytics: analytics.Result{
			Scope: analytics.ScopeMetadata{
				SnapshotID:               "analytics-sha256:test",
				AuthorizationFingerprint: "auth-sha256:test",
				PolicyGeneration:         1,
				SeedNodeIDs:              []shoal.ID{"node"},
				Depth:                    request.Scope.Depth, Direction: request.Scope.Direction,
				Fanout: request.Scope.Fanout, MaxNodes: request.Scope.MaxNodes,
				MaxEdges:               request.Scope.MaxEdges,
				MaxScannedEdgesPerNode: request.Scope.MaxScannedEdgesPerNode,
				NodeCount:              1,
				Complete:               true,
			},
			Nodes: []analytics.NodeSummary{{
				NodeID: "node", PageRank: 1,
				WeakComponentID: "component-sha256:1fd9520a06abfed7821c68b3626a8ff3dcca5d4da5d663da612324802ebc821f",
			}},
			WeaklyConnectedComponents: []analytics.ComponentSummary{{
				ID:        "component-sha256:1fd9520a06abfed7821c68b3626a8ff3dcca5d4da5d663da612324802ebc821f",
				NodeIDs:   []shoal.ID{"node"},
				NodeCount: 1,
			}},
			PageRank: analytics.PageRankSummary{
				DampingFactor:        analytics.DefaultDampingFactor,
				ConvergenceTolerance: analytics.DefaultConvergenceTolerance,
				MaxIterations:        analytics.DefaultMaxIterations,
				Iterations:           1, Converged: true,
			},
			Recording: analytics.RecordingStatus{
				Recorded: !s.invalid, Required: true,
				InteractionID: "interaction-route",
			},
		},
	}, nil
}
