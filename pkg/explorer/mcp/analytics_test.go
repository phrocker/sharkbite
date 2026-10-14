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

package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAnalyticsOptionalToolAdvertisesAndInvokesOnlyWithLimits(t *testing.T) {
	limits := analytics.DefaultLimits()
	service := &analyticsMCPService{
		stubService: &stubService{}, limits: limits,
		available: true, recording: true,
	}
	provider, err := NewAnalyticsTool(service, limits)
	if err != nil {
		t.Fatal(err)
	}
	tool := provider.Tool()
	schema, err := parseOptionalToolInputSchema(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"damping_factor", "convergence_tolerance"} {
		bound := schema.Properties["page_rank"].Properties[name]
		if bound.Maximum != nil || bound.ExclusiveMaximum == nil {
			t.Fatalf("%s upper bound = %#v", name, bound)
		}
		if err := validateOptionalToolValue(json.Number("1"), bound); err == nil {
			t.Fatalf("%s accepted exclusive upper bound", name)
		}
		if err := validateOptionalToolValue(json.Number("0.5"), bound); err != nil {
			t.Fatalf("%s rejected valid value: %v", name, err)
		}
	}
	for _, name := range []string{"convergence_tolerance", "max_iterations"} {
		bound := schema.Properties["page_rank"].Properties[name]
		if err := validateOptionalToolValue(json.Number("0"), bound); err != nil {
			t.Fatalf("%s rejected explicit default value: %v", name, err)
		}
	}
	now := time.Now()
	authority := auth.NewAuthority()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationAnalyticsRead},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "template-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		OptionalTools:    []OptionalToolProvider{provider},
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.resolver = authority.Resolver()
	makeReady(t, server)
	if names := listedToolNames(t, server); !slices.Contains(names, ToolAnalytics) {
		t.Fatalf("tools = %v", names)
	}
	request := webapi.AnalyticsRequest{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{"node"}, Depth: 1,
			Direction: explorer.GraphDirectionBoth,
			Fanout:    4, MaxNodes: 8, MaxEdges: 16,
			MaxScannedEdgesPerNode: 32,
		},
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeToolResult(
		t, callToolRequest(t, server, ToolAnalytics, string(arguments)))
	if result.IsError || service.calls != 1 {
		t.Fatalf("analytics result = %#v, calls=%d", result, service.calls)
	}
	if !strings.Contains(
		string(result.StructuredContent), "analytics-sha256:test") {
		t.Fatalf("structured result = %s", result.StructuredContent)
	}
	service.invalid = true
	invalid := decodeToolResult(
		t, callToolRequest(t, server, ToolAnalytics, string(arguments)))
	if !invalid.IsError || service.calls != 2 {
		t.Fatalf("invalid analytics result = %#v, calls=%d", invalid, service.calls)
	}

	without, _ := newTestServer(t, &stubService{}, nil)
	makeReady(t, without)
	if names := listedToolNames(t, without); slices.Contains(names, ToolAnalytics) {
		t.Fatalf("analytics advertised without provider: %v", names)
	}
}

func TestAnalyticsOptionalToolRejectsUnavailableOrMismatchedLimits(t *testing.T) {
	limits := analytics.DefaultLimits()
	unavailable := &analyticsMCPService{
		stubService: &stubService{}, limits: limits, available: false,
	}
	if _, err := NewAnalyticsTool(unavailable, limits); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("unavailable limits error = %v", err)
	}
	available := &analyticsMCPService{
		stubService: &stubService{}, limits: limits,
		available: true, recording: true,
	}
	mismatch := limits
	mismatch.MaxEdges--
	if _, err := NewAnalyticsTool(available, mismatch); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("mismatched limits error = %v", err)
	}
	unrecorded := &analyticsMCPService{
		stubService: &stubService{}, limits: limits, available: true,
	}
	if _, err := NewAnalyticsTool(unrecorded, limits); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("unrecorded provider error = %v", err)
	}
}

type analyticsMCPService struct {
	*stubService
	resolver  auth.Resolver
	limits    analytics.Limits
	available bool
	recording bool
	calls     int
	invalid   bool
}

func (s *analyticsMCPService) AnalyticsLimits() (analytics.Limits, bool) {
	return s.limits, s.available
}

func (s *analyticsMCPService) AnalyticsRecordingRequired() bool {
	return s.recording
}

func (s *analyticsMCPService) Analytics(
	ctx context.Context,
	request webapi.AnalyticsRequest,
) (webapi.AnalyticsResponse, error) {
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return webapi.AnalyticsResponse{}, err
	}
	if err := decision.Authorize(
		auth.OperationAnalyticsRead,
		auth.ResourceRequest{AuthorizationDomain: decision.AuthorizationDomain()},
		time.Now(),
	); err != nil {
		return webapi.AnalyticsResponse{}, err
	}
	s.calls++
	return webapi.AnalyticsResponse{
		Snapshot: webapi.AnalyticsSnapshot{ID: "analytics-sha256:test"},
		Analytics: analytics.Result{
			Scope: analytics.ScopeMetadata{
				SnapshotID:               "analytics-sha256:test",
				AuthorizationFingerprint: "auth-sha256:test",
				PolicyGeneration:         1, SeedNodeIDs: request.Scope.NodeIDs,
				Depth: request.Scope.Depth, Direction: request.Scope.Direction,
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
				InteractionID: "interaction-mcp",
			},
		},
	}, nil
}
