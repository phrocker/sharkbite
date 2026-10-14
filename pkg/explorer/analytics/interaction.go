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

package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const analyticsToolKind = "analytics"
const graphAssertionEdgeIDMetadata = "shoal.graph.edge_id"

// InteractionRecorder adapts authorized analytics records to the shared
// durable interaction recorder.
type InteractionRecorder struct {
	recorder *interaction.Recorder
	now      func() time.Time
}

// NewInteractionRecorder creates the shared-recorder adapter used by shipped
// analytics transports.
func NewInteractionRecorder(
	recorder *interaction.Recorder,
	now func() time.Time,
) (*InteractionRecorder, error) {
	if recorder == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction recorder is required")
	}
	if now == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics recorder clock is required")
	}
	if err := recorder.SetClock(now); err != nil {
		return nil, err
	}
	return &InteractionRecorder{recorder: recorder, now: now}, nil
}

// RecordAnalytics records every materialized node and exact graph edge as the
// evidence used by the analytics tool call.
func (r *InteractionRecorder) RecordAnalytics(
	ctx context.Context,
	record Record,
) (RecordingReceipt, error) {
	if r == nil || r.recorder == nil || r.now == nil {
		return RecordingReceipt{}, shoal.NewError(
			shoal.ErrorUnavailable, "analytics interaction recorder is unavailable")
	}
	if err := contextError(ctx); err != nil {
		return RecordingReceipt{}, err
	}
	if !record.Materialization.Complete || !record.Result.Scope.Complete {
		return RecordingReceipt{}, shoal.NewError(
			shoal.ErrorUnavailable, "analytics materialization is incomplete")
	}
	if record.Result.Scope.SnapshotID == "" ||
		record.Result.Scope.AuthorizationFingerprint !=
			record.Materialization.AuthorizationFingerprint.String() ||
		record.Result.Scope.PolicyGeneration !=
			record.Materialization.PolicyGeneration ||
		record.Materialization.RequestID == "" ||
		record.Materialization.AuthorizationExpiresAt.IsZero() ||
		record.Materialization.Snapshot.ID == "" ||
		record.Materialization.Snapshot.AsOf.IsZero() {
		return RecordingReceipt{}, shoal.NewError(
			shoal.ErrorInternal, "analytics recording pins are inconsistent")
	}
	if !ontologyMatchesMaterialization(
		record.Result.Scope.Ontology, record.Materialization) {
		return RecordingReceipt{}, shoal.NewError(
			shoal.ErrorInternal, "analytics recording ontology is inconsistent")
	}
	limits := record.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.Validate(); err != nil {
		return RecordingReceipt{}, err
	}
	if err := validateAnalyticsEvidenceBytes(
		record.Materialization.Neighborhood, limits.MaxEvidenceBytes); err != nil {
		return RecordingReceipt{}, err
	}
	neighborhood := cloneNeighborhood(record.Materialization.Neighborhood)
	nodeIDs := make([]shoal.ID, len(neighborhood.Nodes))
	for index, node := range neighborhood.Nodes {
		nodeIDs[index] = node.ID
	}

	sort.Slice(nodeIDs, func(i, j int) bool {
		return shoal.CompareID(nodeIDs[i], nodeIDs[j]) < 0
	})
	edges := append([]graph.Edge(nil), neighborhood.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
	if uint32(len(nodeIDs)) != record.Result.Scope.NodeCount ||
		uint32(len(edges)) != record.Result.Scope.EdgeCount ||
		!resultCoversNodes(record.Result, nodeIDs) {
		return RecordingReceipt{}, shoal.NewError(
			shoal.ErrorInternal, "analytics recording evidence is inconsistent")
	}
	recordedAt := r.now().UTC()
	if recordedAt.IsZero() ||
		!recordedAt.Before(record.Materialization.AuthorizationExpiresAt) {
		return RecordingReceipt{}, shoal.NewError(
			shoal.ErrorUnauthorized, "analytics authorization expired before recording")
	}
	sessionID, err := interaction.OperationSessionID(
		interaction.OperationToolCall,
		record.Materialization.RequestID,
		recordedAt,
	)
	if err != nil {
		return RecordingReceipt{}, err
	}
	resultID := analyticsResultID(record.Result)
	evidence, err := analyticsEvidenceReferences(
		neighborhood.Nodes, edges, neighborhood.Assertions)
	if err != nil {
		return RecordingReceipt{}, err
	}
	session := interaction.Session{
		ID:           sessionID,
		RecordedAt:   recordedAt,
		Operation:    interaction.OperationToolCall,
		SnapshotID:   shoal.ID(record.Materialization.Snapshot.ID),
		SnapshotAsOf: record.Materialization.Snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(
			record.Materialization.AuthorizationFingerprint.String()),
		AuthorizationExpiresAt: record.Materialization.AuthorizationExpiresAt,
		AuthorizationOperation: string(auth.OperationAnalyticsRead),
		OntologySchemaID:       record.Materialization.SelectedOntology.SchemaID(),
		OntologyVersionID:      record.Materialization.SelectedOntology.VersionID(),
		RequestID:              record.Materialization.RequestID,
		QueryDigest:            analyticsRequestDigest(record.Request),
		ResultID:               resultID,
		StopReason:             "completed",
		SeedNodeIDs:            append([]shoal.ID(nil), record.Request.Scope.NodeIDs...),
		Turns: []interaction.Turn{{
			Index: 0, Decision: "completed",
			ToolCall: &interaction.ToolCall{
				Kind:              analyticsToolKind,
				RetrievedNodeIDs:  nodeIDs,
				RetrievedEvidence: evidence,
			},
		}},
		Provenance: interaction.Provenance{
			Harness:  "shoal-explorer-analytics",
			Provider: "shoal", Model: "pagerank",
			ModelVersion: "1", ToolPolicy: "analytics_read",
		},
	}
	persisted, err := r.recorder.Record(ctx, session)
	if err != nil {
		return RecordingReceipt{}, err
	}
	if persisted.ID != sessionID ||
		persisted.SnapshotID != session.SnapshotID ||
		!persisted.SnapshotAsOf.UTC().Equal(session.SnapshotAsOf.UTC()) ||
		persisted.AuthorizationFingerprint != session.AuthorizationFingerprint ||
		!persisted.AuthorizationExpiresAt.Equal(session.AuthorizationExpiresAt) ||
		persisted.AuthorizationOperation != session.AuthorizationOperation ||
		persisted.OntologySchemaID != session.OntologySchemaID ||
		persisted.OntologyVersionID != session.OntologyVersionID ||
		persisted.RequestID != session.RequestID ||
		persisted.Actor.SubjectID == "" || persisted.Actor.ActorID == "" ||
		!equalIDs(persisted.TouchedNodeIDs(), nodeIDs) ||
		!equalIDs(persisted.TouchedEdgeIDs(), edgeIDs(edges)) ||
		!equalAssertionReferences(
			persisted.TouchedAssertions(), evidenceAssertions(evidence)) ||
		!equalEvidenceReferences(
			recordedEvidenceReferences(persisted),
			evidence) {
		return RecordingReceipt{}, explorer.MarkIndeterminateCommit(
			shoal.NewError(
				shoal.ErrorInternal,
				"analytics recorder returned inconsistent evidence",
			),
		)
	}
	return RecordingReceipt{InteractionID: persisted.ID}, nil
}

func validateAnalyticsEvidenceBytes(
	neighborhood explorer.Neighborhood,
	limit uint64,
) error {
	total := uint64(2)
	add := func(value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "encode analytics interaction evidence", err)
		}
		size := uint64(len(encoded)) + 1
		if size > limit || total > limit-size {
			return shoal.NewError(
				shoal.ErrorUnavailable,
				"analytics interaction evidence exceeds the configured byte limit",
			)
		}
		total += size
		return nil
	}
	for _, node := range neighborhood.Nodes {
		if err := add(node); err != nil {
			return err
		}
	}
	for _, edge := range neighborhood.Edges {
		if err := add(edge); err != nil {
			return err
		}
	}
	for _, assertion := range neighborhood.Assertions {
		if err := add(assertion); err != nil {
			return err
		}
	}
	return nil
}

func analyticsEvidenceReferences(
	nodes []graph.Node,
	edges []graph.Edge,
	assertions []ontology.Assertion,
) ([]interaction.EvidenceReference, error) {
	nodesByID := make(map[shoal.ID]graph.Node, len(nodes))
	for _, node := range nodes {
		nodesByID[node.ID] = node
	}
	assertionsByEdge := make(
		map[shoal.ID][]interaction.AssertionReference, len(assertions))
	for _, assertion := range assertions {
		reference, err := InteractionAssertionEvidence(assertion)
		if err != nil {
			return nil, err
		}
		assertionsByEdge[reference.EdgeID] = append(
			assertionsByEdge[reference.EdgeID], reference)
	}
	references := make([]interaction.EvidenceReference, 0, len(nodes)+len(edges))
	for _, node := range nodes {
		anchor, err := inference.NewGraphAnchorWithAssertions(
			graph.Path{Nodes: []graph.Node{node}}, nil)
		if err != nil {
			return nil, err
		}
		references = append(references, interaction.EvidenceReference{
			AnchorID: anchor.ID(), Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{node.ID},
		})
	}
	for _, edge := range edges {
		from, fromOK := nodesByID[edge.From]
		to, toOK := nodesByID[edge.To]
		if !fromOK || !toOK {
			return nil, shoal.NewError(
				shoal.ErrorInternal,
				"analytics edge endpoint is absent from exact node evidence")
		}
		edgeAssertions := assertionsByEdge[edge.ID]
		anchor, err := inference.NewGraphAnchorWithAssertions(
			graph.Path{
				Nodes: []graph.Node{from, to},
				Edges: []graph.Edge{edge},
			},
			edgeAssertions,
		)
		if err != nil {
			return nil, err
		}
		references = append(references, interaction.EvidenceReference{
			AnchorID: anchor.ID(), Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{edge.From, edge.To},
			EdgeIDs: []shoal.ID{edge.ID}, Assertions: edgeAssertions,
		})
		delete(assertionsByEdge, edge.ID)
	}
	if len(assertionsByEdge) != 0 {
		return nil, shoal.NewError(
			shoal.ErrorInternal,
			"analytics assertion is not bound to returned edge evidence")
	}
	sort.Slice(references, func(i, j int) bool {
		return shoal.CompareID(references[i].AnchorID, references[j].AnchorID) < 0
	})
	return references, nil
}

func evidenceAssertions(
	references []interaction.EvidenceReference,
) []interaction.AssertionReference {
	var assertions []interaction.AssertionReference
	for _, reference := range references {
		assertions = append(assertions, reference.Assertions...)
	}
	sort.Slice(assertions, func(i, j int) bool {
		if compared := shoal.CompareID(
			assertions[i].EdgeID, assertions[j].EdgeID); compared != 0 {
			return compared < 0
		}
		return shoal.CompareID(
			assertions[i].AssertionID, assertions[j].AssertionID) < 0
	})
	return assertions
}

// InteractionAssertionEvidence projects an ontology assertion into the exact
// durable evidence representation used by analytics interaction records.
func InteractionAssertionEvidence(
	assertion ontology.Assertion,
) (interaction.AssertionReference, error) {
	edgeID := shoal.ID(assertion.Metadata()[graphAssertionEdgeIDMetadata])
	if edgeID == "" && assertion.Origin() == ontology.AssertionDerived {
		edgeID = assertion.ID()
	}
	if edgeID == "" {
		return interaction.AssertionReference{}, shoal.NewError(
			shoal.ErrorInternal,
			"analytics assertion is not bound to an exact graph edge",
		)
	}
	return interaction.AssertionReference{
		AssertionID: assertion.ID(),
		EdgeID:      edgeID,
		Origin:      assertion.Origin(),
	}, nil
}

func recordedEvidenceReferences(
	session interaction.Session,
) []interaction.EvidenceReference {
	var references []interaction.EvidenceReference
	for _, turn := range session.Turns {
		if turn.ToolCall != nil {
			references = append(
				references, turn.ToolCall.RetrievedEvidence...)
		}
	}
	return references
}

func equalEvidenceReferences(
	left, right []interaction.EvidenceReference,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		leftCanonical, leftErr := left[index].Canonical()
		rightCanonical, rightErr := right[index].Canonical()
		if leftErr != nil || rightErr != nil ||
			!equalIDs(leftCanonical.NodeIDs, rightCanonical.NodeIDs) ||
			!equalIDs(leftCanonical.EdgeIDs, rightCanonical.EdgeIDs) ||
			leftCanonical.AnchorID != rightCanonical.AnchorID ||
			leftCanonical.Kind != rightCanonical.Kind ||
			leftCanonical.Citation != rightCanonical.Citation ||
			!equalAssertionReferences(
				leftCanonical.Assertions, rightCanonical.Assertions) {
			return false
		}
	}
	return true
}

func equalAssertionReferences(
	left, right []interaction.AssertionReference,
) bool {
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

func ontologyMatchesMaterialization(
	scope *OntologyScope,
	materialization Materialization,
) bool {
	selected := materialization.SelectedOntology
	if !selected.Known() {
		return scope == nil
	}
	return scope != nil &&
		scope.SchemaID == selected.SchemaID() &&
		scope.VersionID == selected.VersionID()
}

func resultCoversNodes(result Result, expected []shoal.ID) bool {
	actual := make([]shoal.ID, len(result.Nodes))
	for index, node := range result.Nodes {
		actual[index] = node.NodeID
	}
	sort.Slice(actual, func(i, j int) bool {
		return shoal.CompareID(actual[i], actual[j]) < 0
	})
	return equalIDs(actual, expected)
}

func edgeIDs(edges []graph.Edge) []shoal.ID {
	ids := make([]shoal.ID, len(edges))
	for index, edge := range edges {
		ids[index] = edge.ID
	}
	return ids
}

func analyticsRequestDigest(request Request) string {
	var encoded bytes.Buffer
	writeText(&encoded, "authorized-analytics-request-v1")
	writeText(&encoded, request.SnapshotID)
	writeUint64(&encoded, uint64(len(request.Scope.NodeIDs)))
	for _, nodeID := range request.Scope.NodeIDs {
		writeBytes(&encoded, []byte(nodeID))
	}
	writeUint64(&encoded, uint64(request.Scope.Depth))
	writeText(&encoded, string(request.Scope.Direction))
	writeUint64(&encoded, uint64(request.Scope.Fanout))
	writeUint64(&encoded, uint64(request.Scope.MaxNodes))
	writeUint64(&encoded, uint64(request.Scope.MaxEdges))
	writeUint64(&encoded, uint64(request.Scope.MaxScannedEdgesPerNode))
	writeUint64(&encoded, uint64(len(request.Scope.EdgeTypes)))
	for _, edgeType := range request.Scope.EdgeTypes {
		writeText(&encoded, edgeType)
	}
	writeUint64(&encoded, math.Float64bits(request.PageRank.DampingFactor))
	writeUint64(&encoded, math.Float64bits(request.PageRank.ConvergenceTolerance))
	writeUint64(&encoded, uint64(request.PageRank.MaxIterations))
	return interaction.Digest(encoded.String())
}

func analyticsResultID(result Result) shoal.ID {
	var encoded bytes.Buffer
	writeText(&encoded, "authorized-analytics-result-v2")
	writeText(&encoded, result.Scope.SnapshotID)
	writeText(&encoded, result.Scope.AuthorizationFingerprint)
	writeUint64(&encoded, uint64(result.Scope.PolicyGeneration))
	if result.Scope.Ontology != nil {
		writeText(&encoded, string(result.Scope.Ontology.SchemaID))
		writeText(&encoded, string(result.Scope.Ontology.VersionID))
	} else {
		writeText(&encoded, "")
		writeText(&encoded, "")
	}
	writeUint64(&encoded, uint64(len(result.Scope.SeedNodeIDs)))
	for _, nodeID := range result.Scope.SeedNodeIDs {
		writeText(&encoded, string(nodeID))
	}
	writeUint64(&encoded, uint64(result.Scope.Depth))
	writeText(&encoded, string(result.Scope.Direction))
	writeUint64(&encoded, uint64(result.Scope.Fanout))
	writeUint64(&encoded, uint64(result.Scope.MaxNodes))
	writeUint64(&encoded, uint64(result.Scope.MaxEdges))
	writeUint64(&encoded, uint64(result.Scope.MaxScannedEdgesPerNode))
	writeUint64(&encoded, uint64(len(result.Scope.EdgeTypes)))
	for _, edgeType := range result.Scope.EdgeTypes {
		writeText(&encoded, edgeType)
	}
	writeUint64(&encoded, uint64(result.Scope.NodeCount))
	writeUint64(&encoded, uint64(result.Scope.EdgeCount))
	writeUint64(&encoded, uint64(result.Scope.ResolvedAssertionCount))
	writeUint64(&encoded, uint64(len(result.Scope.UnresolvedAssertions)))
	for _, unresolved := range result.Scope.UnresolvedAssertions {
		writeText(&encoded, string(unresolved.AssertionID))
		writeText(&encoded, string(unresolved.Reading))
		writeText(&encoded, unresolved.Reason)
	}
	if result.Scope.Complete {
		writeUint64(&encoded, 1)
	} else {
		writeUint64(&encoded, 0)
	}
	writeUint64(&encoded, uint64(len(result.Nodes)))
	for _, node := range result.Nodes {
		writeBytes(&encoded, []byte(node.NodeID))
		writeUint64(&encoded, uint64(node.InDegree))
		writeUint64(&encoded, uint64(node.OutDegree))
		writeUint64(&encoded, uint64(node.Degree))
		writeUint64(&encoded, math.Float64bits(node.PageRank))
		writeText(&encoded, node.WeakComponentID)
	}
	writeUint64(&encoded, uint64(len(result.WeaklyConnectedComponents)))
	for _, component := range result.WeaklyConnectedComponents {
		writeText(&encoded, component.ID)
		writeUint64(&encoded, uint64(len(component.NodeIDs)))
		for _, nodeID := range component.NodeIDs {
			writeText(&encoded, string(nodeID))
		}
		writeUint64(&encoded, uint64(component.NodeCount))
		writeUint64(&encoded, uint64(component.EdgeCount))
	}
	writeUint64(&encoded, math.Float64bits(result.PageRank.DampingFactor))
	writeUint64(&encoded, math.Float64bits(result.PageRank.ConvergenceTolerance))
	writeUint64(&encoded, uint64(result.PageRank.MaxIterations))
	writeUint64(&encoded, uint64(result.PageRank.Iterations))
	if result.PageRank.Converged {
		writeUint64(&encoded, 1)
	} else {
		writeUint64(&encoded, 0)
	}
	return interaction.DerivedID(
		"analytics_result", interaction.Digest(encoded.String()))
}
