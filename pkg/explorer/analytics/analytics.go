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

// Package analytics computes deterministic graph analytics only after a
// production authorization layer has completely materialized an explicitly
// bounded subgraph.
package analytics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"sort"
	"time"

	rankkernel "github.com/phrocker/shoal-oss/internal/graphrank"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	HardMaxSeeds               uint32  = 32
	HardMaxDepth               uint32  = 4
	HardMaxFanout              uint32  = 50
	HardMaxNodes               uint32  = 250
	HardMaxEdges               uint32  = 12_500
	HardMaxEvidenceBytes       uint64  = 16 << 20
	HardMaxScannedEdgesPerNode uint32  = 1_024
	HardMaxEdgeTypes           uint32  = 64
	HardMaxPageRankIterations  uint32  = 1_000
	MinPageRankTolerance       float64 = 1e-15

	DefaultDampingFactor        float64 = 0.85
	DefaultConvergenceTolerance float64 = 1e-10
	DefaultMaxIterations        uint32  = 200
)

// Limits are the server-owned maxima advertised with an analytics provider.
type Limits struct {
	MaxSeeds               uint32  `json:"max_seeds"`
	MaxDepth               uint32  `json:"max_depth"`
	MaxFanout              uint32  `json:"max_fanout"`
	MaxNodes               uint32  `json:"max_nodes"`
	MaxEdges               uint32  `json:"max_edges"`
	MaxEvidenceBytes       uint64  `json:"max_evidence_bytes"`
	MaxScannedEdgesPerNode uint32  `json:"max_scanned_edges_per_node"`
	MaxEdgeTypes           uint32  `json:"max_edge_types"`
	MaxPageRankIterations  uint32  `json:"max_page_rank_iterations"`
	MinPageRankTolerance   float64 `json:"min_page_rank_tolerance"`
}

// DefaultLimits returns the bounded production defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxSeeds: HardMaxSeeds, MaxDepth: HardMaxDepth,
		MaxFanout: HardMaxFanout, MaxNodes: HardMaxNodes,
		MaxEdges:               HardMaxEdges,
		MaxEvidenceBytes:       HardMaxEvidenceBytes,
		MaxScannedEdgesPerNode: HardMaxScannedEdgesPerNode,
		MaxEdgeTypes:           HardMaxEdgeTypes,
		MaxPageRankIterations:  HardMaxPageRankIterations,
		MinPageRankTolerance:   MinPageRankTolerance,
	}
}

// Validate checks provider limits against hard process bounds.
func (l Limits) Validate() error {
	if l.MaxSeeds == 0 || l.MaxSeeds > HardMaxSeeds ||
		l.MaxDepth == 0 || l.MaxDepth > HardMaxDepth ||
		l.MaxFanout == 0 || l.MaxFanout > HardMaxFanout ||
		l.MaxNodes == 0 || l.MaxNodes > HardMaxNodes ||
		l.MaxEdges == 0 || l.MaxEdges > HardMaxEdges ||
		l.MaxEvidenceBytes == 0 ||
		l.MaxEvidenceBytes > HardMaxEvidenceBytes ||
		l.MaxScannedEdgesPerNode == 0 ||
		l.MaxScannedEdgesPerNode > HardMaxScannedEdgesPerNode ||
		l.MaxEdgeTypes == 0 || l.MaxEdgeTypes > HardMaxEdgeTypes ||
		l.MaxPageRankIterations == 0 ||
		l.MaxPageRankIterations > HardMaxPageRankIterations ||
		math.IsNaN(l.MinPageRankTolerance) ||
		math.IsInf(l.MinPageRankTolerance, 0) ||
		l.MinPageRankTolerance < MinPageRankTolerance ||
		l.MinPageRankTolerance >= 1 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics provider limits are invalid")
	}
	if l.MaxSeeds > l.MaxNodes || l.MaxScannedEdgesPerNode < l.MaxFanout {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics provider limits are inconsistent")
	}
	return nil
}

// Scope is the complete graph boundary within which analytics are meaningful.
// Every numeric graph bound is required; zero never means unbounded.
type Scope struct {
	NodeIDs                []shoal.ID
	Depth                  uint32
	Direction              explorer.GraphDirection
	Fanout                 uint32
	MaxNodes               uint32
	MaxEdges               uint32
	MaxScannedEdgesPerNode uint32
	EdgeTypes              []string
}

// PageRankOptions controls converged PageRank over the complete authorized
// materialization. Zero values select deterministic defaults.
type PageRankOptions struct {
	DampingFactor        float64
	ConvergenceTolerance float64
	MaxIterations        uint32
}

// Request asks for all supported exact-within-bounds analytics.
type Request struct {
	SnapshotID string
	Scope      Scope
	PageRank   PageRankOptions
}

// OntologyScope identifies the selected read-time lens.
type OntologyScope struct {
	SchemaID  shoal.ID
	VersionID shoal.ID
}

// UnresolvedSemantic preserves an ontology-lens failure instead of silently
// treating the original assertion as resolved under the selected version.
type UnresolvedSemantic struct {
	AssertionID shoal.ID
	Reading     ontology.OntologyReading
	Reason      string
}

// ScopeMetadata states exactly which authorized graph and semantics were used.
type ScopeMetadata struct {
	SnapshotID               string
	AuthorizationFingerprint string
	PolicyGeneration         int64
	Ontology                 *OntologyScope
	SeedNodeIDs              []shoal.ID
	Depth                    uint32
	Direction                explorer.GraphDirection
	Fanout                   uint32
	MaxNodes                 uint32
	MaxEdges                 uint32
	MaxScannedEdgesPerNode   uint32
	EdgeTypes                []string
	NodeCount                uint32
	EdgeCount                uint32
	ResolvedAssertionCount   uint32
	UnresolvedAssertions     []UnresolvedSemantic
	Complete                 bool
}

// NodeSummary contains directed degree, weak component membership, and
// converged PageRank for one authorized node.
type NodeSummary struct {
	NodeID          shoal.ID
	InDegree        uint32
	OutDegree       uint32
	Degree          uint32
	PageRank        float64
	WeakComponentID string
}

// ComponentSummary is one weakly connected component. It is not a community
// detection claim.
type ComponentSummary struct {
	ID        string
	NodeIDs   []shoal.ID
	NodeCount uint32
	EdgeCount uint32
}

// PageRankSummary records the convergence contract used for the returned
// scores.
type PageRankSummary struct {
	DampingFactor        float64 `json:"damping_factor"`
	ConvergenceTolerance float64 `json:"convergence_tolerance"`
	MaxIterations        uint32  `json:"max_iterations"`
	Iterations           uint32  `json:"iterations"`
	Converged            bool    `json:"converged"`
}

// RecordingStatus is the integration seam for the shared interaction recorder.
type RecordingStatus struct {
	Recorded      bool     `json:"recorded"`
	Required      bool     `json:"required"`
	InteractionID shoal.ID `json:"-"`
}

// Result contains deterministic analytics over one complete authorized scope.
type Result struct {
	Scope                     ScopeMetadata
	Nodes                     []NodeSummary
	WeaklyConnectedComponents []ComponentSummary
	PageRank                  PageRankSummary
	Recording                 RecordingStatus
}

// Materialization is the trusted handoff from an authorization-enforcing
// bounded graph provider. Snapshot is retained only for end-of-request
// revalidation and is never copied to the public analytics result.
type Materialization struct {
	Snapshot                 explorer.Snapshot         `json:"-"`
	Neighborhood             explorer.Neighborhood     `json:"-"`
	AuthorizationFingerprint auth.Fingerprint          `json:"-"`
	PolicyGeneration         int64                     `json:"-"`
	SelectedOntology         ontology.OntologyIdentity `json:"-"`
	RequestID                shoal.ID                  `json:"-"`
	AuthorizationExpiresAt   time.Time                 `json:"-"`
	Complete                 bool                      `json:"-"`
}

// Materializer is implemented by the production authorization wrapper. It
// returns only completely authorization-filtered graph material.
type Materializer interface {
	MaterializeAnalytics(
		context.Context,
		explorer.BoundedNeighborhoodRequest,
		uint32,
		uint64,
	) (Materialization, error)
	RevalidateAnalytics(context.Context, Materialization) error
}

// Recorder durably captures the complete authorized analytics evidence before
// a required-recording service may return success.
type Recorder interface {
	RecordAnalytics(context.Context, Record) (RecordingReceipt, error)
}

// Record carries the complete authorized materialization to the recorder.
// Transports never serialize this value.
type Record struct {
	Request         Request
	Result          Result
	Materialization Materialization
	Limits          Limits
}

// RecordingReceipt identifies the exact durable interaction accepted by the
// shared recorder.
type RecordingReceipt struct {
	InteractionID shoal.ID
}

// Config constructs a bounded analytics service.
type Config struct {
	Source           Materializer
	Limits           Limits
	Recorder         Recorder
	RequireRecording bool
}

// Service materializes, computes, revalidates, and optionally records one
// authorized analytics request without caching graphs across requests.
type Service struct {
	source           Materializer
	limits           Limits
	recorder         Recorder
	requireRecording bool
}

// NewService validates all production dependencies and bounds.
func NewService(config Config) (*Service, error) {
	if isNil(config.Source) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics materializer is required")
	}
	limits := config.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if config.RequireRecording && isNil(config.Recorder) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "analytics recording is required but unavailable")
	}
	return &Service{
		source: config.Source, limits: limits, recorder: config.Recorder,
		requireRecording: config.RequireRecording,
	}, nil
}

// Limits returns the immutable server-owned analytics bounds.
func (s *Service) Limits() Limits {
	if s == nil {
		return Limits{}
	}
	return s.limits
}

// ValidateRequest checks and normalizes one request against advertised limits.
func ValidateRequest(request Request, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	_, err := normalizeRequest(request, limits)
	return err
}

// ValidateResult checks an analytics response against its request and
// advertised provider limits.
func ValidateResult(request Request, result Result, limits Limits) error {
	normalized, err := normalizeRequest(request, limits)
	if err != nil {
		return err
	}
	scope := result.Scope
	if !scope.Complete || scope.SnapshotID == "" ||
		len(scope.SnapshotID) > 256 ||
		scope.AuthorizationFingerprint == "" ||
		scope.PolicyGeneration <= 0 ||
		scope.Depth != normalized.Scope.Depth ||
		scope.Direction != normalized.Scope.Direction ||
		scope.Fanout != normalized.Scope.Fanout ||
		scope.MaxNodes != normalized.Scope.MaxNodes ||
		scope.MaxEdges != normalized.Scope.MaxEdges ||
		scope.MaxScannedEdgesPerNode != normalized.Scope.MaxScannedEdgesPerNode ||
		!equalIDs(scope.SeedNodeIDs, normalized.Scope.NodeIDs) ||
		!equalStrings(scope.EdgeTypes, normalized.Scope.EdgeTypes) ||
		scope.NodeCount != uint32(len(result.Nodes)) ||
		scope.NodeCount > normalized.Scope.MaxNodes ||
		scope.EdgeCount > normalized.Scope.MaxEdges ||
		(normalized.SnapshotID != "" &&
			normalized.SnapshotID != scope.SnapshotID) {
		return shoal.NewError(
			shoal.ErrorInternal, "analytics response scope is inconsistent")
	}
	if scope.Ontology != nil {
		identity, err := ontology.NewOntologyIdentityFromIDs(
			scope.Ontology.SchemaID, scope.Ontology.VersionID)
		if err != nil || !identity.Known() {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response ontology is invalid")
		}
	} else if scope.ResolvedAssertionCount != 0 ||
		len(scope.UnresolvedAssertions) != 0 {
		return shoal.NewError(
			shoal.ErrorInternal,
			"analytics response assertion metadata requires an ontology",
		)
	}
	seenUnresolved := make(map[shoal.ID]struct{}, len(scope.UnresolvedAssertions))
	for index, unresolved := range scope.UnresolvedAssertions {
		if err := ontology.ValidateID(unresolved.AssertionID); err != nil ||
			ontology.IDNamespace(unresolved.AssertionID) != "assertion" {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response assertion ID is invalid")
		}
		if unresolved.Reading == "" {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response assertion status is invalid")
		}
		switch ontology.OntologyReading(unresolved.Reading) {
		case ontology.OntologyUnresolved, ontology.OntologyMalformed,
			ontology.OntologySameVersion, ontology.OntologyOtherVersion,
			ontology.OntologyOtherSchema:
		default:
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response assertion reading is invalid")
		}
		if _, duplicate := seenUnresolved[unresolved.AssertionID]; duplicate ||
			(index > 0 && shoal.CompareID(
				scope.UnresolvedAssertions[index-1].AssertionID,
				unresolved.AssertionID,
			) >= 0) {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response assertions are not canonical")
		}
		seenUnresolved[unresolved.AssertionID] = struct{}{}
	}
	if uint64(scope.ResolvedAssertionCount)+
		uint64(len(scope.UnresolvedAssertions)) > uint64(scope.EdgeCount) {
		return shoal.NewError(
			shoal.ErrorInternal,
			"analytics response assertion totals exceed the materialized edges",
		)
	}
	if result.PageRank.DampingFactor != normalized.PageRank.DampingFactor ||
		result.PageRank.ConvergenceTolerance !=
			normalized.PageRank.ConvergenceTolerance ||
		result.PageRank.MaxIterations != normalized.PageRank.MaxIterations ||
		(scope.NodeCount > 0 && (!result.PageRank.Converged ||
			result.PageRank.Iterations == 0)) ||
		result.PageRank.Iterations > result.PageRank.MaxIterations {
		return shoal.NewError(
			shoal.ErrorInternal, "analytics response PageRank metadata is inconsistent")
	}
	seenNodes := make(map[shoal.ID]NodeSummary, len(result.Nodes))
	componentIDs := make(map[string]struct{}, len(result.WeaklyConnectedComponents))
	var rankSum float64
	var inSum, outSum uint64
	for index, node := range result.Nodes {
		if err := shoal.ValidateRequiredID("analytics node ID", node.NodeID); err != nil ||
			math.IsNaN(node.PageRank) || math.IsInf(node.PageRank, 0) ||
			node.PageRank < 0 || node.Degree != node.InDegree+node.OutDegree ||
			node.WeakComponentID == "" {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response node is invalid")
		}
		if _, duplicate := seenNodes[node.NodeID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response has duplicate nodes")
		}
		if index > 0 {
			previous := result.Nodes[index-1]
			if previous.PageRank < node.PageRank ||
				(previous.PageRank == node.PageRank &&
					shoal.CompareID(previous.NodeID, node.NodeID) >= 0) {
				return shoal.NewError(
					shoal.ErrorInternal, "analytics response nodes are not canonical")
			}
		}
		seenNodes[node.NodeID] = node
		rankSum += node.PageRank
		inSum += uint64(node.InDegree)
		outSum += uint64(node.OutDegree)
	}
	for _, seedID := range normalized.Scope.NodeIDs {
		if _, ok := seenNodes[seedID]; !ok {
			return shoal.NewError(
				shoal.ErrorInternal,
				"analytics response omits a requested seed node",
			)
		}
	}
	if inSum != uint64(scope.EdgeCount) || outSum != uint64(scope.EdgeCount) ||
		(scope.NodeCount > 0 && math.Abs(rankSum-1) > 1e-8) {
		return shoal.NewError(
			shoal.ErrorInternal, "analytics response graph totals are inconsistent")
	}
	covered := make(map[shoal.ID]struct{}, len(result.Nodes))
	var componentEdges uint64
	for index, component := range result.WeaklyConnectedComponents {
		if component.ID == "" ||
			component.NodeCount != uint32(len(component.NodeIDs)) ||
			len(component.NodeIDs) == 0 ||
			component.ID != componentID(component.NodeIDs) {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response component is invalid")
		}
		if _, duplicate := componentIDs[component.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response has duplicate components")
		}
		componentIDs[component.ID] = struct{}{}
		var memberEdges uint64
		if index > 0 && shoal.CompareID(
			result.WeaklyConnectedComponents[index-1].NodeIDs[0],
			component.NodeIDs[0],
		) >= 0 {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics response components are not canonical")
		}
		for memberIndex, nodeID := range component.NodeIDs {
			if memberIndex > 0 &&
				shoal.CompareID(component.NodeIDs[memberIndex-1], nodeID) >= 0 {
				return shoal.NewError(
					shoal.ErrorInternal, "analytics component members are not canonical")
			}
			node, ok := seenNodes[nodeID]
			if !ok || node.WeakComponentID != component.ID {
				return shoal.NewError(
					shoal.ErrorInternal, "analytics component membership is inconsistent")
			}
			if _, duplicate := covered[nodeID]; duplicate {
				return shoal.NewError(
					shoal.ErrorInternal, "analytics node occurs in multiple components")
			}
			covered[nodeID] = struct{}{}
			memberEdges += uint64(node.OutDegree)
		}
		if memberEdges != uint64(component.EdgeCount) {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics component edge count is inconsistent")
		}
		componentEdges += uint64(component.EdgeCount)
	}
	if result.Recording.Recorded {
		if err := shoal.ValidateRequiredID(
			"analytics recording interaction", result.Recording.InteractionID,
		); err != nil {
			return shoal.NewError(
				shoal.ErrorInternal, "analytics recording interaction ID is invalid")
		}
	}
	if len(covered) != len(result.Nodes) ||
		componentEdges != uint64(scope.EdgeCount) ||
		(result.Recording.Required && !result.Recording.Recorded) ||
		result.Recording.Recorded != (result.Recording.InteractionID != "") {
		return shoal.NewError(
			shoal.ErrorInternal, "analytics response is incomplete")
	}
	return nil
}

// Run computes analytics only after complete authorized materialization and
// revalidates identity, ontology lens, policy generation, and corpus state
// before returning.
func (s *Service) Run(ctx context.Context, request Request) (Result, error) {
	if s == nil || isNil(s.source) {
		return Result{}, shoal.NewError(
			shoal.ErrorUnavailable, "analytics provider is unavailable")
	}
	normalized, err := normalizeRequest(request, s.limits)
	if err != nil {
		return Result{}, err
	}
	materialization, err := s.source.MaterializeAnalytics(
		ctx,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: append([]shoal.ID(nil), normalized.Scope.NodeIDs...),
			Depth:   normalized.Scope.Depth, Fanout: normalized.Scope.Fanout,
			MaxNodes:        normalized.Scope.MaxNodes,
			MaxScannedEdges: normalized.Scope.MaxScannedEdgesPerNode,
			EdgeTypes:       append([]string(nil), normalized.Scope.EdgeTypes...),
			Direction:       normalized.Scope.Direction,
		},
		normalized.Scope.MaxEdges, s.limits.MaxEvidenceBytes,
	)
	if err != nil {
		return Result{}, err
	}
	if !materialization.Complete {
		return Result{}, shoal.NewError(
			shoal.ErrorUnavailable, "authorized analytics materialization is incomplete")
	}
	neighborhood := cloneNeighborhood(materialization.Neighborhood)
	analysis, err := Analyze(ctx, neighborhood, normalized.PageRank)
	if err != nil {
		return Result{}, err
	}
	scope, err := scopeMetadata(normalized.Scope, materialization, neighborhood)
	if err != nil {
		return Result{}, err
	}
	if normalized.SnapshotID != "" &&
		normalized.SnapshotID != scope.SnapshotID {
		return Result{}, shoal.NewError(
			shoal.ErrorConflict, "requested analytics snapshot is no longer current")
	}
	result := Result{
		Scope: scope, Nodes: analysis.Nodes,
		WeaklyConnectedComponents: analysis.WeaklyConnectedComponents,
		PageRank:                  analysis.PageRank,
		Recording:                 RecordingStatus{Required: s.requireRecording},
	}
	if err := s.source.RevalidateAnalytics(ctx, materialization); err != nil {
		return Result{}, err
	}
	if !isNil(s.recorder) {
		receipt, err := s.recorder.RecordAnalytics(ctx, Record{
			Request: normalized, Result: result,
			Materialization: materialization,
			Limits:          s.limits,
		})
		if err != nil {
			wrapped := shoal.WrapError(
				shoal.ErrorUnavailable, "record analytics result", err)
			if explorer.IsCommittedInteraction(err) ||
				explorer.IsIndeterminateCommit(err) {
				return Result{}, explorer.MarkIndeterminateCommit(wrapped)
			}
			return Result{}, wrapped
		}
		if err := shoal.ValidateRequiredID(
			"analytics interaction ID", receipt.InteractionID,
		); err != nil {
			return Result{}, explorer.MarkIndeterminateCommit(
				shoal.NewError(
					shoal.ErrorUnavailable,
					"analytics recorder returned no durable interaction identity",
				),
			)
		}
		result.Recording.Recorded = true
		result.Recording.InteractionID = receipt.InteractionID
		if err := s.source.RevalidateAnalytics(ctx, materialization); err != nil {
			return Result{}, explorer.MarkIndeterminateCommit(
				shoal.WrapError(
					shoal.ErrorUnavailable,
					"analytics result was recorded but authorization revalidation failed",
					err,
				),
			)
		}
	}
	return result, nil
}

// Analysis is the deterministic pure-kernel output before authorization scope
// metadata is attached.
type Analysis struct {
	Nodes                     []NodeSummary
	WeaklyConnectedComponents []ComponentSummary
	PageRank                  PageRankSummary
}

// Analyze computes directed degrees, weakly connected components, and
// converged PageRank over an already complete subgraph.
func Analyze(
	ctx context.Context,
	neighborhood explorer.Neighborhood,
	options PageRankOptions,
) (Analysis, error) {
	if err := contextError(ctx); err != nil {
		return Analysis{}, err
	}
	options, err := normalizePageRank(options, DefaultLimits())
	if err != nil {
		return Analysis{}, err
	}
	nodes := make(map[shoal.ID]graph.Node, len(neighborhood.Nodes))
	nodeIDs := make([]shoal.ID, 0, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		if err := node.Validate(); err != nil {
			return Analysis{}, shoal.WrapError(
				shoal.ErrorInvalidArgument, "analytics graph contains an invalid node", err)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return Analysis{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "analytics graph contains duplicate node IDs")
		}
		nodes[node.ID] = node
		nodeIDs = append(nodeIDs, node.ID)
	}
	sort.Slice(nodeIDs, func(i, j int) bool {
		return shoal.CompareID(nodeIDs[i], nodeIDs[j]) < 0
	})
	edges := append([]graph.Edge(nil), neighborhood.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
	seenEdges := make(map[shoal.ID]struct{}, len(edges))
	inDegree := make(map[shoal.ID]uint32, len(nodes))
	outDegree := make(map[shoal.ID]uint32, len(nodes))
	adjacency := make(map[shoal.ID][]shoal.ID, len(nodes))
	rankEdges := make([]rankkernel.Edge, 0, len(edges))
	for _, edge := range edges {
		if err := edge.Validate(); err != nil {
			return Analysis{}, shoal.WrapError(
				shoal.ErrorInvalidArgument, "analytics graph contains an invalid edge", err)
		}
		if _, duplicate := seenEdges[edge.ID]; duplicate {
			return Analysis{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "analytics graph contains duplicate edge IDs")
		}
		seenEdges[edge.ID] = struct{}{}
		if _, ok := nodes[edge.From]; !ok {
			return Analysis{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "analytics edge source is outside the subgraph")
		}
		if _, ok := nodes[edge.To]; !ok {
			return Analysis{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "analytics edge target is outside the subgraph")
		}
		outDegree[edge.From]++
		inDegree[edge.To]++
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		if edge.From != edge.To {
			adjacency[edge.To] = append(adjacency[edge.To], edge.From)
		}
		rankEdges = append(rankEdges, rankkernel.Edge{
			From: string(edge.From), To: string(edge.To),
		})
	}
	rankVertices := make([]string, len(nodeIDs))
	for index, nodeID := range nodeIDs {
		rankVertices[index] = string(nodeID)
	}
	ranked, err := rankkernel.Compute(ctx, rankVertices, rankEdges, rankkernel.Options{
		DampingFactor:        options.DampingFactor,
		MaxIterations:        int(options.MaxIterations),
		ConvergenceThreshold: options.ConvergenceTolerance,
		RedistributeDangling: true,
	})
	if err != nil {
		return Analysis{}, contextErrorFrom(err)
	}
	if len(nodeIDs) > 0 && !ranked.Converged {
		return Analysis{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"analytics PageRank did not converge within max_iterations",
		)
	}
	components, componentByNode, err := weakComponents(ctx, nodeIDs, adjacency, edges)
	if err != nil {
		return Analysis{}, err
	}
	summaries := make([]NodeSummary, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		summaries = append(summaries, NodeSummary{
			NodeID: nodeID, InDegree: inDegree[nodeID],
			OutDegree:       outDegree[nodeID],
			Degree:          inDegree[nodeID] + outDegree[nodeID],
			PageRank:        ranked.Ranks[string(nodeID)],
			WeakComponentID: componentByNode[nodeID],
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].PageRank != summaries[j].PageRank {
			return summaries[i].PageRank > summaries[j].PageRank
		}
		return shoal.CompareID(summaries[i].NodeID, summaries[j].NodeID) < 0
	})
	return Analysis{
		Nodes: summaries, WeaklyConnectedComponents: components,
		PageRank: PageRankSummary{
			DampingFactor:        options.DampingFactor,
			ConvergenceTolerance: options.ConvergenceTolerance,
			MaxIterations:        options.MaxIterations,
			Iterations:           uint32(ranked.Iterations), Converged: ranked.Converged,
		},
	}, nil
}

func normalizeRequest(request Request, limits Limits) (Request, error) {
	if err := contextlessValidateSnapshotID(request.SnapshotID); err != nil {
		return Request{}, err
	}
	if request.Scope.Depth == 0 || request.Scope.Fanout == 0 ||
		request.Scope.MaxNodes == 0 || request.Scope.MaxEdges == 0 ||
		request.Scope.MaxScannedEdgesPerNode == 0 {
		return Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics graph bounds must be explicit and nonzero")
	}
	if uint32(len(request.Scope.NodeIDs)) > limits.MaxSeeds ||
		request.Scope.Depth > limits.MaxDepth ||
		request.Scope.Fanout > limits.MaxFanout ||
		request.Scope.MaxNodes > limits.MaxNodes ||
		request.Scope.MaxEdges > limits.MaxEdges ||
		request.Scope.MaxScannedEdgesPerNode > limits.MaxScannedEdgesPerNode {
		return Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics request exceeds provider limits")
	}
	if request.Scope.MaxScannedEdgesPerNode < request.Scope.Fanout {
		return Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"max_scanned_edges_per_node cannot be less than fanout",
		)
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: request.Scope.NodeIDs, Depth: request.Scope.Depth,
		EdgeTypes: request.Scope.EdgeTypes,
	}).Normalize()
	if err != nil {
		return Request{}, err
	}
	if uint32(len(normalized.NodeIDs)) > request.Scope.MaxNodes {
		return Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics seeds exceed max_nodes")
	}
	if uint32(len(normalized.EdgeTypes)) > limits.MaxEdgeTypes {
		return Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics edge types exceed provider limits")
	}
	direction := request.Scope.Direction
	if direction == "" {
		direction = explorer.GraphDirectionBoth
	}
	switch direction {
	case explorer.GraphDirectionBoth,
		explorer.GraphDirectionOutgoing,
		explorer.GraphDirectionIncoming:
	default:
		return Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown analytics graph direction")
	}
	pageRank, err := normalizePageRank(request.PageRank, limits)
	if err != nil {
		return Request{}, err
	}
	request.Scope.NodeIDs = normalized.NodeIDs
	request.Scope.EdgeTypes = normalized.EdgeTypes
	sort.Slice(request.Scope.NodeIDs, func(i, j int) bool {
		return shoal.CompareID(request.Scope.NodeIDs[i], request.Scope.NodeIDs[j]) < 0
	})
	sort.Strings(request.Scope.EdgeTypes)
	request.Scope.Direction = direction
	request.PageRank = pageRank
	return request, nil
}

func normalizePageRank(options PageRankOptions, limits Limits) (PageRankOptions, error) {
	if options.DampingFactor == 0 {
		options.DampingFactor = DefaultDampingFactor
	}
	if options.ConvergenceTolerance == 0 {
		options.ConvergenceTolerance = math.Max(
			DefaultConvergenceTolerance, limits.MinPageRankTolerance)
	}
	if options.MaxIterations == 0 {
		options.MaxIterations = min(
			DefaultMaxIterations, limits.MaxPageRankIterations)
	}
	if math.IsNaN(options.DampingFactor) ||
		math.IsInf(options.DampingFactor, 0) ||
		options.DampingFactor <= 0 || options.DampingFactor >= 1 ||
		math.IsNaN(options.ConvergenceTolerance) ||
		math.IsInf(options.ConvergenceTolerance, 0) ||
		options.ConvergenceTolerance < limits.MinPageRankTolerance ||
		options.ConvergenceTolerance >= 1 ||
		options.MaxIterations > limits.MaxPageRankIterations {
		return PageRankOptions{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics PageRank options are invalid")
	}
	return options, nil
}

func weakComponents(
	ctx context.Context,
	nodeIDs []shoal.ID,
	adjacency map[shoal.ID][]shoal.ID,
	edges []graph.Edge,
) ([]ComponentSummary, map[shoal.ID]string, error) {
	for nodeID := range adjacency {
		sort.Slice(adjacency[nodeID], func(i, j int) bool {
			return shoal.CompareID(adjacency[nodeID][i], adjacency[nodeID][j]) < 0
		})
	}
	visited := make(map[shoal.ID]struct{}, len(nodeIDs))
	componentByNode := make(map[shoal.ID]string, len(nodeIDs))
	components := make([]ComponentSummary, 0)
	for _, start := range nodeIDs {
		if _, ok := visited[start]; ok {
			continue
		}
		if err := contextError(ctx); err != nil {
			return nil, nil, err
		}
		queue := []shoal.ID{start}
		visited[start] = struct{}{}
		members := make([]shoal.ID, 0)
		for len(queue) > 0 {
			nodeID := queue[0]
			queue = queue[1:]
			members = append(members, nodeID)
			for _, neighbor := range adjacency[nodeID] {
				if _, ok := visited[neighbor]; ok {
					continue
				}
				visited[neighbor] = struct{}{}
				queue = append(queue, neighbor)
			}
		}
		sort.Slice(members, func(i, j int) bool {
			return shoal.CompareID(members[i], members[j]) < 0
		})
		componentID := componentID(members)
		memberSet := make(map[shoal.ID]struct{}, len(members))
		for _, member := range members {
			memberSet[member] = struct{}{}
			componentByNode[member] = componentID
		}
		edgeCount := uint32(0)
		for _, edge := range edges {
			if _, ok := memberSet[edge.From]; ok {
				edgeCount++
			}
		}
		components = append(components, ComponentSummary{
			ID: componentID, NodeIDs: members,
			NodeCount: uint32(len(members)), EdgeCount: edgeCount,
		})
	}
	return components, componentByNode, nil
}

func componentID(nodeIDs []shoal.ID) string {
	var encoded bytes.Buffer
	writeText(&encoded, "authorized-analytics-weak-component-v1")
	writeUint64(&encoded, uint64(len(nodeIDs)))
	for _, nodeID := range nodeIDs {
		writeBytes(&encoded, []byte(nodeID))
	}
	sum := sha256.Sum256(encoded.Bytes())
	return "component-sha256:" + hex.EncodeToString(sum[:])
}

func scopeMetadata(
	request Scope,
	materialization Materialization,
	neighborhood explorer.Neighborhood,
) (ScopeMetadata, error) {
	fingerprint := materialization.AuthorizationFingerprint
	selected := materialization.SelectedOntology
	hasOntology := selected.Known()
	var selectedScope *OntologyScope
	if hasOntology {
		selectedScope = &OntologyScope{
			SchemaID: selected.SchemaID(), VersionID: selected.VersionID(),
		}
		if len(neighborhood.Interpretations) != len(neighborhood.Assertions) {
			return ScopeMetadata{}, shoal.NewError(
				shoal.ErrorInternal,
				"authorized ontology interpretation set is incomplete",
			)
		}
	} else if len(neighborhood.Interpretations) != 0 {
		return ScopeMetadata{}, shoal.NewError(
			shoal.ErrorInternal, "unexpected ontology interpretations")
	}
	assertionIDs := make(map[shoal.ID]struct{}, len(neighborhood.Assertions))
	for _, assertion := range neighborhood.Assertions {
		if err := assertion.Validate(); err != nil {
			return ScopeMetadata{}, shoal.NewError(
				shoal.ErrorInternal, "authorized assertion is invalid")
		}
		if _, duplicate := assertionIDs[assertion.ID()]; duplicate {
			return ScopeMetadata{}, shoal.NewError(
				shoal.ErrorInternal, "authorized assertions are duplicated")
		}
		assertionIDs[assertion.ID()] = struct{}{}
	}
	unresolved := make([]UnresolvedSemantic, 0)
	resolved := uint32(0)
	interpreted := make(map[shoal.ID]struct{}, len(neighborhood.Interpretations))
	for _, interpretation := range neighborhood.Interpretations {
		original := interpretation.Original()
		if _, ok := assertionIDs[original.ID()]; !ok ||
			interpretation.Reader() != selected ||
			interpretation.Reading() == "" {
			return ScopeMetadata{}, shoal.NewError(
				shoal.ErrorInternal, "authorized ontology interpretation is invalid")
		}
		if _, duplicate := interpreted[original.ID()]; duplicate {
			return ScopeMetadata{}, shoal.NewError(
				shoal.ErrorInternal, "authorized ontology interpretations are duplicated")
		}
		interpreted[original.ID()] = struct{}{}
		switch interpretation.Status() {
		case ontology.InterpretationResolved,
			ontology.InterpretationUnresolved:
		default:
			return ScopeMetadata{}, shoal.NewError(
				shoal.ErrorInternal, "authorized ontology interpretation status is invalid")
		}
		if interpretation.Resolved() {
			resolved++
			continue
		}
		unresolved = append(unresolved, UnresolvedSemantic{
			AssertionID: original.ID(),
			Reading:     interpretation.Reading(), Reason: interpretation.Reason(),
		})
	}
	sort.Slice(unresolved, func(i, j int) bool {
		return shoal.CompareID(
			unresolved[i].AssertionID, unresolved[j].AssertionID) < 0
	})
	snapshotID := authorizedScopeSnapshotID(
		request, fingerprint, materialization.PolicyGeneration,
		selected, hasOntology, neighborhood)
	return ScopeMetadata{
		SnapshotID:               snapshotID,
		AuthorizationFingerprint: fingerprint.String(),
		PolicyGeneration:         materialization.PolicyGeneration,
		Ontology:                 selectedScope,
		SeedNodeIDs:              append([]shoal.ID(nil), request.NodeIDs...),
		Depth:                    request.Depth, Direction: request.Direction,
		Fanout: request.Fanout, MaxNodes: request.MaxNodes,
		MaxEdges:               request.MaxEdges,
		MaxScannedEdgesPerNode: request.MaxScannedEdgesPerNode,
		EdgeTypes:              append([]string(nil), request.EdgeTypes...),
		NodeCount:              uint32(len(neighborhood.Nodes)),
		EdgeCount:              uint32(len(neighborhood.Edges)),
		ResolvedAssertionCount: resolved,
		UnresolvedAssertions:   unresolved,
		Complete:               true,
	}, nil
}

func cloneNeighborhood(value explorer.Neighborhood) explorer.Neighborhood {
	cloned := explorer.Neighborhood{
		Nodes: append([]graph.Node(nil), value.Nodes...),
		Edges: append([]graph.Edge(nil), value.Edges...),
		Assertions: append(
			[]ontology.Assertion(nil), value.Assertions...),
		Interpretations: append(
			[]ontology.AssertionInterpretation(nil), value.Interpretations...),
	}
	for index := range cloned.Nodes {
		cloned.Nodes[index].Labels = append(
			[]string(nil), cloned.Nodes[index].Labels...)
		if cloned.Nodes[index].Properties != nil {
			properties := make(shoal.Metadata, len(cloned.Nodes[index].Properties))
			for key, item := range cloned.Nodes[index].Properties {
				properties[key] = item
			}
			cloned.Nodes[index].Properties = properties
		}
	}
	for index := range cloned.Edges {
		if cloned.Edges[index].Properties != nil {
			properties := make(shoal.Metadata, len(cloned.Edges[index].Properties))
			for key, item := range cloned.Edges[index].Properties {
				properties[key] = item
			}
			cloned.Edges[index].Properties = properties
		}
	}
	return cloned
}

func authorizedScopeSnapshotID(
	request Scope,
	fingerprint [sha256.Size]byte,
	generation int64,
	selected ontology.OntologyIdentity,
	hasOntology bool,
	neighborhood explorer.Neighborhood,
) string {
	var encoded bytes.Buffer
	writeText(&encoded, "authorized-analytics-snapshot-v2")
	writeBytes(&encoded, fingerprint[:])
	writeUint64(&encoded, uint64(generation))
	if hasOntology {
		writeUint64(&encoded, 1)
		writeBytes(&encoded, []byte(selected.SchemaID()))
		writeBytes(&encoded, []byte(selected.VersionID()))
	} else {
		writeUint64(&encoded, 0)
	}
	writeUint64(&encoded, uint64(len(request.NodeIDs)))
	for _, nodeID := range request.NodeIDs {
		writeBytes(&encoded, []byte(nodeID))
	}
	writeUint64(&encoded, uint64(request.Depth))
	writeText(&encoded, string(request.Direction))
	writeUint64(&encoded, uint64(request.Fanout))
	writeUint64(&encoded, uint64(request.MaxNodes))
	writeUint64(&encoded, uint64(request.MaxEdges))
	writeUint64(&encoded, uint64(request.MaxScannedEdgesPerNode))
	writeUint64(&encoded, uint64(len(request.EdgeTypes)))
	for _, edgeType := range request.EdgeTypes {
		writeText(&encoded, edgeType)
	}
	nodes := append([]graph.Node(nil), neighborhood.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return shoal.CompareID(nodes[i].ID, nodes[j].ID) < 0
	})
	writeUint64(&encoded, uint64(len(nodes)))
	for _, node := range nodes {
		writeText(&encoded, "node")
		writeBytes(&encoded, []byte(node.ID))
		writeText(&encoded, node.Kind)
		writeUint64(&encoded, uint64(len(node.Labels)))
		for _, label := range node.Labels {
			writeText(&encoded, label)
		}
		writeMetadata(&encoded, node.Properties)
	}
	edges := append([]graph.Edge(nil), neighborhood.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
	writeUint64(&encoded, uint64(len(edges)))
	for _, edge := range edges {
		writeText(&encoded, "edge")
		writeBytes(&encoded, []byte(edge.ID))
		writeBytes(&encoded, []byte(edge.From))
		writeBytes(&encoded, []byte(edge.To))
		writeText(&encoded, edge.Type)
		writeUint64(&encoded, math.Float64bits(float64(edge.Weight)))
		writeMetadata(&encoded, edge.Properties)
	}
	assertions := append(
		[]ontology.Assertion(nil), neighborhood.Assertions...)
	sort.Slice(assertions, func(i, j int) bool {
		return shoal.CompareID(assertions[i].ID(), assertions[j].ID()) < 0
	})
	writeUint64(&encoded, uint64(len(assertions)))
	for _, assertion := range assertions {
		writeText(&encoded, "assertion")
		writeBytes(&encoded, []byte(assertion.ID()))
		writeBytes(&encoded, []byte(assertion.Subject()))
		writeBytes(&encoded, []byte(assertion.Predicate()))
		target, _ := assertion.Object().ReferenceValue()
		writeBytes(&encoded, []byte(target))
		writeText(&encoded, string(assertion.Origin()))
		writeUint64(
			&encoded, math.Float64bits(float64(assertion.Confidence())))
		writeMetadata(&encoded, assertion.Metadata())
		writeMetadata(&encoded, assertion.Provenance().Metadata())
		evidence := assertion.Evidence()
		writeUint64(&encoded, uint64(len(evidence)))
		for _, item := range evidence {
			writeBytes(&encoded, []byte(item.ID()))
			writeMetadata(&encoded, item.Metadata())
		}
	}
	interpretations := append(
		[]ontology.AssertionInterpretation(nil), neighborhood.Interpretations...)
	sort.Slice(interpretations, func(i, j int) bool {
		return shoal.CompareID(
			interpretations[i].Original().ID(),
			interpretations[j].Original().ID(),
		) < 0
	})
	writeUint64(&encoded, uint64(len(interpretations)))
	for _, interpretation := range interpretations {
		original := interpretation.Original()
		originalSubject, _ := original.SubjectType()
		originalObject, _ := original.ObjectType()
		subject, _ := interpretation.SubjectType()
		object, _ := interpretation.ObjectType()
		writeText(&encoded, "interpretation")
		writeBytes(&encoded, []byte(original.ID()))
		writeText(&encoded, string(interpretation.Reading()))
		writeText(&encoded, string(interpretation.Status()))
		writeBytes(&encoded, []byte(originalSubject))
		writeBytes(&encoded, []byte(subject))
		writeBytes(&encoded, []byte(original.Predicate()))
		writeBytes(&encoded, []byte(interpretation.Predicate()))
		writeBytes(&encoded, []byte(originalObject))
		writeBytes(&encoded, []byte(object))
		writeText(&encoded, interpretation.Reason())
		morphisms := interpretation.AppliedMorphisms()
		writeUint64(&encoded, uint64(len(morphisms)))
		for _, morphismID := range morphisms {
			writeBytes(&encoded, []byte(morphismID))
		}
	}
	sum := sha256.Sum256(encoded.Bytes())
	return "analytics-sha256:" + hex.EncodeToString(sum[:])
}

func writeMetadata(buffer *bytes.Buffer, metadata shoal.Metadata) {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeUint64(buffer, uint64(len(keys)))
	for _, key := range keys {
		writeText(buffer, key)
		writeText(buffer, metadata[key])
	}
}

func writeText(buffer *bytes.Buffer, value string) {
	writeBytes(buffer, []byte(value))
}

func writeBytes(buffer *bytes.Buffer, value []byte) {
	writeUint64(buffer, uint64(len(value)))
	_, _ = buffer.Write(value)
}

func writeUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = buffer.Write(encoded[:])
}

func contextlessValidateSnapshotID(value string) error {
	if len(value) > 256 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics snapshot ID exceeds the public bound")
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "analytics context is required")
	}
	return contextErrorFrom(ctx.Err())
}

func contextErrorFrom(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return shoal.NewError(shoal.ErrorCanceled, "analytics canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return shoal.NewError(shoal.ErrorDeadline, "analytics deadline exceeded")
	default:
		return shoal.WrapError(shoal.ErrorUnavailable, "analytics unavailable", err)
	}
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
