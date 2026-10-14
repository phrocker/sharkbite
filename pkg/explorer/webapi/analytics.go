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

package webapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	exploreranalytics "github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AnalyticsSnapshot pins one exact authorized and ontology-lensed subgraph.
// It intentionally does not expose the whole-corpus snapshot frontier.
type AnalyticsSnapshot struct {
	ID string `json:"id"`
}

// AnalyticsRequest computes deterministic analytics within an explicit graph
// scope. Every graph work bound is required and nonzero.
type AnalyticsRequest struct {
	Snapshot AnalyticsSnapshot
	Scope    exploreranalytics.Scope
	PageRank exploreranalytics.PageRankOptions
}

// AnalyticsResponse returns only analytics derived from the authorized scope.
type AnalyticsResponse struct {
	Snapshot  AnalyticsSnapshot
	Analytics exploreranalytics.Result
}

// AnalyticsProvider is an optional service extension for authorized bounded
// graph analytics.
type AnalyticsProvider interface {
	Analytics(context.Context, AnalyticsRequest) (AnalyticsResponse, error)
}

// AnalyticsLimitsProvider advertises validated runtime limits. A service is
// not advertised as analytics-capable without both provider and limits.
type AnalyticsLimitsProvider interface {
	AnalyticsLimits() (exploreranalytics.Limits, bool)
}

// AnalyticsRecordingProvider confirms that successful calls require durable
// interaction capture. Providers without this guarantee are not advertised.
type AnalyticsRecordingProvider interface {
	AnalyticsRecordingRequired() bool
}

// AnalyticsAvailable reports whether the embedded service has an authorized
// materializer and valid server-owned limits.
func (s *EmbeddedService) AnalyticsAvailable() bool {
	if s == nil || s.analytics == nil {
		return false
	}
	return s.analytics.Limits().Validate() == nil
}

// AnalyticsLimits returns the exact server-owned bounds when available.
func (s *EmbeddedService) AnalyticsLimits() (exploreranalytics.Limits, bool) {
	if !s.AnalyticsAvailable() {
		return exploreranalytics.Limits{}, false
	}
	return s.analytics.Limits(), true
}

// AnalyticsRecordingRequired reports the shipped fail-closed recording
// contract.
func (s *EmbeddedService) AnalyticsRecordingRequired() bool {
	return s.AnalyticsAvailable()
}

// Analytics runs the authorized materializer and deterministic pure kernel.
func (s *EmbeddedService) Analytics(
	ctx context.Context,
	request AnalyticsRequest,
) (AnalyticsResponse, error) {
	if !s.AnalyticsAvailable() {
		return AnalyticsResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"analytics\" is unavailable")
	}
	result, err := s.analytics.Run(ctx, exploreranalytics.Request{
		SnapshotID: request.Snapshot.ID,
		Scope:      request.Scope,
		PageRank:   request.PageRank,
	})
	if err != nil {
		return AnalyticsResponse{}, err
	}
	return AnalyticsResponse{
		Snapshot:  AnalyticsSnapshot{ID: result.Scope.SnapshotID},
		Analytics: result,
	}, nil
}

func analyticsEndpoint(service Service) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		capabilities, err := capabilitiesFor(request.Context(), service)
		if err != nil {
			writeError(writer, err)
			return
		}
		if !capabilities.Analytics {
			writeError(writer, shoal.NewError(
				shoal.ErrorUnavailable, "workspace capability \"analytics\" is unavailable"))
			return
		}
		provider, limits, ok := analyticsProvider(service)
		if !ok {
			writeError(writer, shoal.NewError(
				shoal.ErrorUnavailable, "workspace capability \"analytics\" is unavailable"))
			return
		}
		var input AnalyticsRequest
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		applyWorkspaceRequestLimits(request.Context(), &input)
		if err := exploreranalytics.ValidateRequest(
			exploreranalytics.Request{
				SnapshotID: input.Snapshot.ID,
				Scope:      input.Scope, PageRank: input.PageRank,
			},
			limits,
		); err != nil {
			writeError(writer, err)
			return
		}
		response, err := provider.Analytics(request.Context(), input)
		if err != nil {
			writeError(writer, err)
			return
		}
		if err := ValidateAnalyticsResponse(input, response, limits); err != nil {
			writeError(writer, explorer.MarkIndeterminateCommit(err))
			return
		}
		writeResponse(writer, http.StatusOK, response)
	}
}

// ValidateAnalyticsResponse verifies an extension provider result against the
// exact request, advertised limits, snapshot, and durable-recording contract.
func ValidateAnalyticsResponse(
	request AnalyticsRequest,
	response AnalyticsResponse,
	limits exploreranalytics.Limits,
) error {
	if response.Snapshot.ID == "" ||
		response.Snapshot.ID != response.Analytics.Scope.SnapshotID {
		return shoal.NewError(
			shoal.ErrorInternal, "analytics provider returned an inconsistent snapshot")
	}
	if err := exploreranalytics.ValidateResult(
		exploreranalytics.Request{
			SnapshotID: request.Snapshot.ID,
			Scope:      request.Scope,
			PageRank:   request.PageRank,
		},
		response.Analytics,
		limits,
	); err != nil {
		return err
	}
	if !response.Analytics.Recording.Required ||
		!response.Analytics.Recording.Recorded {
		return shoal.NewError(
			shoal.ErrorInternal,
			"analytics provider did not durably record the result",
		)
	}
	return nil
}

func analyticsProvider(
	service Service,
) (AnalyticsProvider, exploreranalytics.Limits, bool) {
	provider, ok := service.(AnalyticsProvider)
	if !ok || isAbsentInterface(provider) {
		return nil, exploreranalytics.Limits{}, false
	}
	limitsProvider, ok := service.(AnalyticsLimitsProvider)
	if !ok || isAbsentInterface(limitsProvider) {
		return nil, exploreranalytics.Limits{}, false
	}
	limits, available := limitsProvider.AnalyticsLimits()
	if !available || limits.Validate() != nil {
		return nil, exploreranalytics.Limits{}, false
	}
	recording, ok := service.(AnalyticsRecordingProvider)
	if !ok || isAbsentInterface(recording) ||
		!recording.AnalyticsRecordingRequired() {
		return nil, exploreranalytics.Limits{}, false
	}
	return provider, limits, true
}

type wireAnalyticsScope struct {
	NodeIDs                []string `json:"node_ids"`
	Depth                  uint32   `json:"depth"`
	Direction              string   `json:"direction,omitempty"`
	Fanout                 uint32   `json:"fanout"`
	MaxNodes               uint32   `json:"max_nodes"`
	MaxEdges               uint32   `json:"max_edges"`
	MaxScannedEdgesPerNode uint32   `json:"max_scanned_edges_per_node"`
	EdgeTypes              []string `json:"edge_types,omitempty"`
}

type wirePageRankOptions struct {
	DampingFactor        float64 `json:"damping_factor,omitempty"`
	ConvergenceTolerance float64 `json:"convergence_tolerance,omitempty"`
	MaxIterations        uint32  `json:"max_iterations,omitempty"`
}

func (r AnalyticsRequest) MarshalJSON() ([]byte, error) {
	nodeIDs := make([]string, len(r.Scope.NodeIDs))
	for index, nodeID := range r.Scope.NodeIDs {
		nodeIDs[index] = encodeID(nodeID)
	}
	return json.Marshal(struct {
		Snapshot AnalyticsSnapshot   `json:"snapshot,omitempty"`
		Scope    wireAnalyticsScope  `json:"scope"`
		PageRank wirePageRankOptions `json:"page_rank,omitempty"`
	}{
		Snapshot: r.Snapshot,
		Scope: wireAnalyticsScope{
			NodeIDs: nodeIDs, Depth: r.Scope.Depth,
			Direction: string(r.Scope.Direction), Fanout: r.Scope.Fanout,
			MaxNodes: r.Scope.MaxNodes, MaxEdges: r.Scope.MaxEdges,
			MaxScannedEdgesPerNode: r.Scope.MaxScannedEdgesPerNode,
			EdgeTypes:              append([]string(nil), r.Scope.EdgeTypes...),
		},
		PageRank: wirePageRankOptions{
			DampingFactor:        r.PageRank.DampingFactor,
			ConvergenceTolerance: r.PageRank.ConvergenceTolerance,
			MaxIterations:        r.PageRank.MaxIterations,
		},
	})
}

func (r *AnalyticsRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot AnalyticsSnapshot   `json:"snapshot,omitempty"`
		Scope    wireAnalyticsScope  `json:"scope"`
		PageRank wirePageRankOptions `json:"page_rank,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	nodeIDs := make([]shoal.ID, len(wire.Scope.NodeIDs))
	for index, encoded := range wire.Scope.NodeIDs {
		nodeID, err := decodeID(encoded)
		if err != nil {
			return err
		}
		nodeIDs[index] = nodeID
	}
	*r = AnalyticsRequest{
		Snapshot: wire.Snapshot,
		Scope: exploreranalytics.Scope{
			NodeIDs: nodeIDs, Depth: wire.Scope.Depth,
			Direction: explorerDirection(wire.Scope.Direction),
			Fanout:    wire.Scope.Fanout, MaxNodes: wire.Scope.MaxNodes,
			MaxEdges:               wire.Scope.MaxEdges,
			MaxScannedEdgesPerNode: wire.Scope.MaxScannedEdgesPerNode,
			EdgeTypes:              append([]string(nil), wire.Scope.EdgeTypes...),
		},
		PageRank: exploreranalytics.PageRankOptions{
			DampingFactor:        wire.PageRank.DampingFactor,
			ConvergenceTolerance: wire.PageRank.ConvergenceTolerance,
			MaxIterations:        wire.PageRank.MaxIterations,
		},
	}
	return nil
}

type wireAnalyticsOntology struct {
	SchemaID  string `json:"schema_id"`
	VersionID string `json:"version_id"`
}

type wireAnalyticsScopeMetadata struct {
	SnapshotID               string                   `json:"snapshot_id"`
	AuthorizationFingerprint string                   `json:"authorization_fingerprint"`
	PolicyGeneration         int64                    `json:"policy_generation"`
	Ontology                 *wireAnalyticsOntology   `json:"ontology,omitempty"`
	SeedNodeIDs              []string                 `json:"seed_node_ids"`
	Depth                    uint32                   `json:"depth"`
	Direction                string                   `json:"direction"`
	Fanout                   uint32                   `json:"fanout"`
	MaxNodes                 uint32                   `json:"max_nodes"`
	MaxEdges                 uint32                   `json:"max_edges"`
	MaxScannedEdgesPerNode   uint32                   `json:"max_scanned_edges_per_node"`
	EdgeTypes                []string                 `json:"edge_types,omitempty"`
	NodeCount                uint32                   `json:"node_count"`
	EdgeCount                uint32                   `json:"edge_count"`
	ResolvedAssertionCount   uint32                   `json:"resolved_assertion_count"`
	UnresolvedAssertions     []wireUnresolvedSemantic `json:"unresolved_assertions,omitempty"`
	Complete                 bool                     `json:"complete"`
}

type wireUnresolvedSemantic struct {
	AssertionID string `json:"assertion_id"`
	Reading     string `json:"reading"`
	Reason      string `json:"reason"`
}

type wireAnalyticsNode struct {
	NodeID          string  `json:"node_id"`
	InDegree        uint32  `json:"in_degree"`
	OutDegree       uint32  `json:"out_degree"`
	Degree          uint32  `json:"degree"`
	PageRank        float64 `json:"page_rank"`
	WeakComponentID string  `json:"weak_component_id"`
}

type wireAnalyticsComponent struct {
	ID        string   `json:"id"`
	NodeIDs   []string `json:"node_ids"`
	NodeCount uint32   `json:"node_count"`
	EdgeCount uint32   `json:"edge_count"`
}

type wireAnalyticsResult struct {
	Scope                     wireAnalyticsScopeMetadata        `json:"scope"`
	Nodes                     []wireAnalyticsNode               `json:"nodes"`
	WeaklyConnectedComponents []wireAnalyticsComponent          `json:"weakly_connected_components"`
	PageRank                  exploreranalytics.PageRankSummary `json:"page_rank"`
	Recording                 wireAnalyticsRecording            `json:"recording"`
}

type wireAnalyticsRecording struct {
	Recorded      bool   `json:"recorded"`
	Required      bool   `json:"required"`
	InteractionID string `json:"interaction_id,omitempty"`
}

func (r AnalyticsResponse) MarshalJSON() ([]byte, error) {
	scope := r.Analytics.Scope
	seedIDs := make([]string, len(scope.SeedNodeIDs))
	for index, id := range scope.SeedNodeIDs {
		seedIDs[index] = encodeID(id)
	}
	unresolved := make([]wireUnresolvedSemantic, len(scope.UnresolvedAssertions))
	for index, item := range scope.UnresolvedAssertions {
		unresolved[index] = wireUnresolvedSemantic{
			AssertionID: encodeID(item.AssertionID),
			Reading:     string(item.Reading), Reason: item.Reason,
		}
	}
	var ontology *wireAnalyticsOntology
	if scope.Ontology != nil {
		ontology = &wireAnalyticsOntology{
			SchemaID:  encodeID(scope.Ontology.SchemaID),
			VersionID: encodeID(scope.Ontology.VersionID),
		}
	}
	nodes := make([]wireAnalyticsNode, len(r.Analytics.Nodes))
	for index, node := range r.Analytics.Nodes {
		nodes[index] = wireAnalyticsNode{
			NodeID: encodeID(node.NodeID), InDegree: node.InDegree,
			OutDegree: node.OutDegree, Degree: node.Degree,
			PageRank: node.PageRank, WeakComponentID: node.WeakComponentID,
		}
	}
	components := make([]wireAnalyticsComponent, len(r.Analytics.WeaklyConnectedComponents))
	for index, component := range r.Analytics.WeaklyConnectedComponents {
		nodeIDs := make([]string, len(component.NodeIDs))
		for member, nodeID := range component.NodeIDs {
			nodeIDs[member] = encodeID(nodeID)
		}
		components[index] = wireAnalyticsComponent{
			ID: component.ID, NodeIDs: nodeIDs,
			NodeCount: component.NodeCount, EdgeCount: component.EdgeCount,
		}
	}
	return json.Marshal(struct {
		Snapshot  AnalyticsSnapshot   `json:"snapshot"`
		Analytics wireAnalyticsResult `json:"analytics"`
	}{
		Snapshot: r.Snapshot,
		Analytics: wireAnalyticsResult{
			Scope: wireAnalyticsScopeMetadata{
				SnapshotID:               scope.SnapshotID,
				AuthorizationFingerprint: scope.AuthorizationFingerprint,
				PolicyGeneration:         scope.PolicyGeneration, Ontology: ontology,
				SeedNodeIDs: seedIDs, Depth: scope.Depth,
				Direction: string(scope.Direction), Fanout: scope.Fanout,
				MaxNodes: scope.MaxNodes, MaxEdges: scope.MaxEdges,
				MaxScannedEdgesPerNode: scope.MaxScannedEdgesPerNode,
				EdgeTypes:              append([]string(nil), scope.EdgeTypes...),
				NodeCount:              scope.NodeCount, EdgeCount: scope.EdgeCount,
				ResolvedAssertionCount: scope.ResolvedAssertionCount,
				UnresolvedAssertions:   unresolved, Complete: scope.Complete,
			},
			Nodes: nodes, WeaklyConnectedComponents: components,
			PageRank: r.Analytics.PageRank,
			Recording: wireAnalyticsRecording{
				Recorded: r.Analytics.Recording.Recorded,
				Required: r.Analytics.Recording.Required,
				InteractionID: encodeOptionalID(
					r.Analytics.Recording.InteractionID),
			},
		},
	})
}

func (r *AnalyticsResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Snapshot  AnalyticsSnapshot   `json:"snapshot"`
		Analytics wireAnalyticsResult `json:"analytics"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	seedIDs, err := decodeAnalyticsIDs(wire.Analytics.Scope.SeedNodeIDs)
	if err != nil {
		return err
	}
	unresolved := make(
		[]exploreranalytics.UnresolvedSemantic,
		len(wire.Analytics.Scope.UnresolvedAssertions),
	)
	for index, item := range wire.Analytics.Scope.UnresolvedAssertions {
		assertionID, err := decodeID(item.AssertionID)
		if err != nil {
			return err
		}
		unresolved[index] = exploreranalytics.UnresolvedSemantic{
			AssertionID: assertionID,
			Reading:     ontology.OntologyReading(item.Reading),
			Reason:      item.Reason,
		}
	}
	var ontology *exploreranalytics.OntologyScope
	if wire.Analytics.Scope.Ontology != nil {
		schemaID, err := decodeID(wire.Analytics.Scope.Ontology.SchemaID)
		if err != nil {
			return err
		}
		versionID, err := decodeID(wire.Analytics.Scope.Ontology.VersionID)
		if err != nil {
			return err
		}
		ontology = &exploreranalytics.OntologyScope{
			SchemaID: schemaID, VersionID: versionID,
		}
	}
	nodes := make([]exploreranalytics.NodeSummary, len(wire.Analytics.Nodes))
	for index, node := range wire.Analytics.Nodes {
		nodeID, err := decodeID(node.NodeID)
		if err != nil {
			return err
		}
		nodes[index] = exploreranalytics.NodeSummary{
			NodeID: nodeID, InDegree: node.InDegree, OutDegree: node.OutDegree,
			Degree: node.Degree, PageRank: node.PageRank,
			WeakComponentID: node.WeakComponentID,
		}
	}
	components := make(
		[]exploreranalytics.ComponentSummary,
		len(wire.Analytics.WeaklyConnectedComponents),
	)
	for index, component := range wire.Analytics.WeaklyConnectedComponents {
		nodeIDs, err := decodeAnalyticsIDs(component.NodeIDs)
		if err != nil {
			return err
		}
		components[index] = exploreranalytics.ComponentSummary{
			ID: component.ID, NodeIDs: nodeIDs,
			NodeCount: component.NodeCount, EdgeCount: component.EdgeCount,
		}
	}
	scope := wire.Analytics.Scope
	recordingID, err := decodeOptionalID(
		wire.Analytics.Recording.InteractionID)
	if err != nil {
		return err
	}
	*r = AnalyticsResponse{
		Snapshot: wire.Snapshot,
		Analytics: exploreranalytics.Result{
			Scope: exploreranalytics.ScopeMetadata{
				SnapshotID:               scope.SnapshotID,
				AuthorizationFingerprint: scope.AuthorizationFingerprint,
				PolicyGeneration:         scope.PolicyGeneration, Ontology: ontology,
				SeedNodeIDs: seedIDs, Depth: scope.Depth,
				Direction: explorerDirection(scope.Direction),
				Fanout:    scope.Fanout, MaxNodes: scope.MaxNodes,
				MaxEdges:               scope.MaxEdges,
				MaxScannedEdgesPerNode: scope.MaxScannedEdgesPerNode,
				EdgeTypes:              append([]string(nil), scope.EdgeTypes...),
				NodeCount:              scope.NodeCount, EdgeCount: scope.EdgeCount,
				ResolvedAssertionCount: scope.ResolvedAssertionCount,
				UnresolvedAssertions:   unresolved, Complete: scope.Complete,
			},
			Nodes: nodes, WeaklyConnectedComponents: components,
			PageRank: wire.Analytics.PageRank,
			Recording: exploreranalytics.RecordingStatus{
				Recorded:      wire.Analytics.Recording.Recorded,
				Required:      wire.Analytics.Recording.Required,
				InteractionID: recordingID,
			},
		},
	}
	return nil
}

func decodeAnalyticsIDs(values []string) ([]shoal.ID, error) {
	ids := make([]shoal.ID, len(values))
	for index, value := range values {
		id, err := decodeID(value)
		if err != nil {
			return nil, err
		}
		ids[index] = id
	}
	return ids, nil
}

func explorerDirection(value string) explorer.GraphDirection {
	return explorer.GraphDirection(value)
}
