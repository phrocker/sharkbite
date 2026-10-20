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
	"errors"
	"reflect"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func (s *Server) recordToolAdmission(
	ctx context.Context,
	decision auth.Decision,
	snapshot explorer.Snapshot,
	authorizationOperation auth.Operation,
	toolName string,
	arguments json.RawMessage,
) error {
	return s.recordToolInteraction(
		ctx, decision, snapshot, authorizationOperation, toolName, arguments,
		ToolObservation{}, false, "admitted", "mcp_tool_admission",
	)
}

func (s *Server) recordToolOutcome(
	ctx context.Context,
	decision auth.Decision,
	snapshot explorer.Snapshot,
	authorizationOperation auth.Operation,
	toolName string,
	arguments json.RawMessage,
	observation ToolObservation,
	failed bool,
	stopReason string,
	mutating bool,
) error {
	reasonCode := "mcp_tool_call"
	if mutating {
		reasonCode = "mcp_tool_outcome"
	}
	return s.recordToolInteraction(
		ctx, decision, snapshot, authorizationOperation, toolName, arguments,
		observation, failed, stopReason, reasonCode,
	)
}

func (s *Server) recordToolInteraction(
	ctx context.Context,
	decision auth.Decision,
	snapshot explorer.Snapshot,
	authorizationOperation auth.Operation,
	toolName string,
	arguments json.RawMessage,
	observation ToolObservation,
	failed bool,
	stopReason string,
	reasonCode string,
) error {
	return s.recordObservedInteraction(
		ctx, decision, snapshot, interaction.OperationToolCall,
		authorizationOperation, toolName, arguments, observation,
		failed, stopReason, reasonCode,
	)
}

func (s *Server) recordRetrievalOutcome(
	ctx context.Context,
	decision auth.Decision,
	snapshot explorer.Snapshot,
	arguments json.RawMessage,
	observation ToolObservation,
) error {
	return s.recordObservedInteraction(
		ctx, decision, snapshot, interaction.OperationRetrieval,
		auth.OperationRetrieve, "", arguments, observation,
		false, "succeeded", "mcp_retrieval",
	)
}

func (s *Server) recordObservedInteraction(
	ctx context.Context,
	decision auth.Decision,
	snapshot explorer.Snapshot,
	provenanceOperation interaction.Operation,
	authorizationOperation auth.Operation,
	toolName string,
	arguments json.RawMessage,
	observation ToolObservation,
	failed bool,
	stopReason string,
	reasonCode string,
) error {
	if s == nil || (s.recorder == nil && isAbsent(s.interactionSink)) ||
		isAbsent(s.snapshots) ||
		s.interactionNow == nil {
		return shoal.NewError(
			shoal.ErrorUnavailable, "MCP interaction recording is unavailable")
	}
	recordedAt := s.interactionNow().UTC()
	if recordedAt.IsZero() {
		return shoal.NewError(
			shoal.ErrorUnavailable, "MCP interaction clock is unavailable")
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	correlation := decision.CorrelationID()
	if correlation == "" {
		correlation = decision.RequestID()
	}
	correlation = shoal.ID(interaction.Digest(
		string(correlation) + "\x00" + reasonCode))
	sessionID, err := interaction.OperationSessionID(
		provenanceOperation, correlation, recordedAt)
	if err != nil {
		return err
	}
	resultID := shoal.ID(interaction.Digest(
		string(decision.RequestID()) + "\x00" + stopReason))
	recorder := s.recorder
	if recorder == nil {
		recorder, err = interaction.NewRecorder(ctx, s.interactionSink)
		if err != nil {
			return err
		}
	}
	session := interaction.Session{
		ID:                       sessionID,
		RecordedAt:               recordedAt,
		Operation:                provenanceOperation,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		AuthorizationOperation:   string(authorizationOperation),
		EmbeddingSpaceID:         observation.EmbeddingSpaceID,
		QueryDigest:              interaction.Digest(string(arguments)),
		RequestID:                decision.RequestID(),
		ResultID:                 resultID,
		StopReason:               stopReason,
		SeedNodeIDs: append(
			[]shoal.ID(nil), observation.RetrievedNodeIDs...),
		SeedEvidence: cloneEvidenceReferences(
			observation.RetrievedEvidence),
		CitedNodeIDs: append([]shoal.ID(nil), observation.CitedNodeIDs...),
		CitedEvidence: cloneEvidenceReferences(
			observation.CitedEvidence),
		RequiredVisibility: append(
			[]string(nil), observation.RequiredVisibility...),
	}
	if provenanceOperation == interaction.OperationToolCall {
		session.SeedNodeIDs = nil
		session.SeedEvidence = nil
		session.Turns = []interaction.Turn{{
			Index:  0,
			Failed: failed,
			ToolCall: &interaction.ToolCall{
				Kind: toolName,
				RetrievedNodeIDs: append(
					[]shoal.ID(nil), observation.RetrievedNodeIDs...),
				RetrievedEvidence: cloneEvidenceReferences(
					observation.RetrievedEvidence),
			},
		}}
	}
	persisted, err := recorder.Record(ctx, session)
	if err != nil {
		return err
	}
	if persisted.ID != sessionID ||
		persisted.Operation != provenanceOperation ||
		persisted.AuthorizationFingerprint != shoal.ID(fingerprint.String()) ||
		!persisted.AuthorizationExpiresAt.Equal(
			decision.AuthenticationExpires()) ||
		persisted.AuthorizationOperation != string(authorizationOperation) ||
		persisted.SnapshotID != shoal.ID(snapshot.ID) ||
		!persisted.SnapshotAsOf.Equal(snapshot.AsOf) {
		return shoal.NewError(
			shoal.ErrorInternal,
			"persisted MCP interaction does not match its execution pins",
		)
	}
	return nil
}

func toolStopReason(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case explorer.IsIndeterminateCommit(err):
		return "indeterminate"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "failed"
	}
}

func toolMutates(tool Tool) bool {
	return tool.Annotations == nil ||
		tool.Annotations.ReadOnlyHint == nil ||
		!*tool.Annotations.ReadOnlyHint
}

func observeToolResult(
	tool registeredTool,
	name string,
	value any,
) (ToolObservation, error) {
	if tool.observe != nil {
		return tool.observe(value)
	}
	switch name {
	case ToolDocuments:
		response, _ := value.(webapi.DocumentsResponse)
		ids := make([]shoal.ID, 0, len(response.Documents))
		for _, item := range response.Documents {
			ids = appendPresentID(ids, item.Document.ID)
		}
		return ToolObservation{
			RetrievedNodeIDs: canonicalObservedIDs(ids),
		}, nil
	case ToolDocument:
		response, _ := value.(webapi.DocumentResponse)
		ids := appendPresentID(nil, response.Document.Document.ID)
		ids = appendSectionIDs(ids, response.Document.Root)
		return ToolObservation{
			RetrievedNodeIDs: canonicalObservedIDs(ids),
		}, nil
	case ToolRetrieve:
		response, _ := value.(webapi.RetrievalResponse)
		return observeRetrievalResponse(response)
	case ToolNeighborhood:
		response, _ := value.(webapi.NeighborhoodResponse)
		ids := make([]shoal.ID, 0, len(response.Neighborhood.Nodes))
		for _, node := range response.Neighborhood.Nodes {
			ids = appendPresentID(ids, node.ID)
		}
		evidence, err := neighborhoodEvidenceReferences(
			response.Neighborhood.Nodes, response.Neighborhood.Edges,
			response.Neighborhood.Assertions)
		if err != nil {
			return ToolObservation{}, err
		}
		return ToolObservation{
			SnapshotID:        shoal.ID(response.Snapshot.ID),
			SnapshotAsOf:      response.Snapshot.AsOf,
			RetrievedNodeIDs:  canonicalObservedIDs(ids),
			RetrievedEvidence: evidence,
		}, nil
	case ToolPath:
		response, _ := value.(webapi.PathResponse)
		ids := make([]shoal.ID, 0, len(response.Path.Nodes))
		for _, node := range response.Path.Nodes {
			ids = appendPresentID(ids, node.ID)
		}
		evidence, err := pathEvidenceReference(
			response.Path, response.Assertions)
		if err != nil {
			return ToolObservation{}, err
		}
		return ToolObservation{
			SnapshotID:        shoal.ID(response.Snapshot.ID),
			SnapshotAsOf:      response.Snapshot.AsOf,
			RetrievedNodeIDs:  canonicalObservedIDs(ids),
			RetrievedEvidence: evidence,
		}, nil
	case ToolIngest:
		response, _ := value.(webapi.IngestResponse)
		ids := make([]shoal.ID, 0, len(response.Files))
		for _, file := range response.Files {
			ids = appendPresentID(ids, file.Document.ID)
		}
		return ToolObservation{
			RetrievedNodeIDs: canonicalObservedIDs(ids),
		}, nil
	case ToolExtract:
		response, _ := value.(webapi.ExtractResponse)
		ids := appendPresentID(nil, response.DocumentID)
		ids = append(ids, response.EntityNodeIDs...)
		return ToolObservation{
			RetrievedNodeIDs: canonicalObservedIDs(ids),
		}, nil
	case ToolRecompute:
		response, _ := value.(webapi.RecomputeDerivationResponse)
		ids := appendPresentID(nil, response.Detail.AssertionID)
		ids = appendPresentID(ids, response.Detail.DerivationID)
		return ToolObservation{
			RetrievedNodeIDs: canonicalObservedIDs(ids),
		}, nil
	case ToolChanges:
		response, _ := value.(webapi.ChangesResponse)
		ids := make([]shoal.ID, 0, len(response.Changes))
		for _, change := range response.Changes {
			ids = appendPresentID(ids, change.Document.Document.ID)
		}
		return ToolObservation{
			RetrievedNodeIDs: canonicalObservedIDs(ids),
		}, nil
	default:
		return ToolObservation{}, nil
	}
}

func observeRetrievalResponse(
	response webapi.RetrievalResponse,
) (ToolObservation, error) {
	observation := ToolObservation{
		SnapshotID:       shoal.ID(response.Snapshot.ID),
		SnapshotAsOf:     response.Snapshot.AsOf,
		EmbeddingSpaceID: response.Retrieval.EmbeddingSpaceID,
		EmbeddingSpaceIDs: append(
			[]shoal.ID(nil), response.Retrieval.EmbeddingSpaceIDs...),
	}
	for _, result := range response.Retrieval.Results {
		if len(result.Evidence) == 0 {
			observation.RetrievedNodeIDs = appendPresentID(
				observation.RetrievedNodeIDs, result.ID)
		}
		for _, evidence := range result.Evidence {
			// The anchor must be the one the Explorer's recorder recomputes
			// from the cited bytes (#137). It refuses any other identity as
			// not authoritative, and the tool result is then withheld.
			anchor, err := inference.NewDocumentAnchor(
				evidence.Citation, evidence.Quote)
			if err != nil {
				return ToolObservation{}, err
			}
			documentReference, err := anchor.EvidenceReference()
			if err != nil {
				return ToolObservation{}, err
			}
			observation.RetrievedEvidence = append(
				observation.RetrievedEvidence, documentReference)
			observation.RetrievedNodeIDs = append(
				observation.RetrievedNodeIDs, documentReference.NodeIDs...)

			if len(evidence.Path.Nodes) == 0 &&
				len(evidence.Path.Edges) == 0 {
				continue
			}
			graphReference, err := graphPathReference(evidence.Path, nil)
			if err != nil {
				return ToolObservation{}, err
			}
			observation.RetrievedEvidence = append(
				observation.RetrievedEvidence, graphReference)
			observation.RetrievedNodeIDs = append(
				observation.RetrievedNodeIDs, graphReference.NodeIDs...)
		}
	}
	observation.RetrievedNodeIDs = canonicalObservedIDs(
		observation.RetrievedNodeIDs)
	return observation, nil
}

// graphPathReference records one directed path exactly as the Explorer's
// recorder recomputes it: node order kept, and every edge an authoritative
// assertion materialized cited through that assertion.
func graphPathReference(
	path graph.Path,
	assertionsByEdge map[shoal.ID][]interaction.AssertionReference,
) (interaction.EvidenceReference, error) {
	var assertions []interaction.AssertionReference
	for _, edge := range path.Edges {
		assertions = append(assertions, assertionsByEdge[edge.ID]...)
	}
	anchor, err := inference.NewGraphAnchorWithAssertions(path, assertions)
	if err != nil {
		return interaction.EvidenceReference{}, err
	}
	return anchor.EvidenceReference()
}

// pathEvidenceReference records the path tool's result as one anchor.
func pathEvidenceReference(
	path graph.Path,
	assertions []ontology.Assertion,
) ([]interaction.EvidenceReference, error) {
	if len(path.Nodes) == 0 && len(path.Edges) == 0 {
		return nil, nil
	}
	byEdge, err := inference.AssertionReferencesByEdge(assertions, path.Edges)
	if err != nil {
		return nil, err
	}
	reference, err := graphPathReference(path, byEdge)
	if err != nil {
		return nil, err
	}
	return []interaction.EvidenceReference{reference}, nil
}

// neighborhoodEvidenceReferences records a neighborhood one edge at a time.
// A neighborhood is not a path and an anchor is one, so each edge with its
// two endpoints is its own anchor. A node no edge reaches, such as a second
// seed with no neighbors, is a single-node anchor of its own, so the
// evidence covers every node the tool retrieved. A neighborhood with no
// edges is observed by node ID only. An edge whose endpoint the
// neighborhood does not carry cannot be anchored exactly and is refused
// rather than recorded loosely.
func neighborhoodEvidenceReferences(
	nodes []graph.Node,
	edges []graph.Edge,
	assertions []ontology.Assertion,
) ([]interaction.EvidenceReference, error) {
	if len(edges) == 0 {
		return nil, nil
	}
	byID := make(map[shoal.ID]graph.Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	byEdge, err := inference.AssertionReferencesByEdge(assertions, edges)
	if err != nil {
		return nil, err
	}
	references := make([]interaction.EvidenceReference, 0, len(edges))
	reached := make(map[shoal.ID]struct{}, len(nodes))
	for _, edge := range edges {
		from, fromOK := byID[edge.From]
		to, toOK := byID[edge.To]
		if !fromOK || !toOK {
			return nil, shoal.NewError(
				shoal.ErrorInternal,
				"neighborhood edge has a missing endpoint")
		}
		reference, err := graphPathReference(graph.Path{
			Nodes: []graph.Node{from, to},
			Edges: []graph.Edge{edge},
		}, byEdge)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
		reached[edge.From] = struct{}{}
		reached[edge.To] = struct{}{}
	}
	for _, node := range nodes {
		if _, ok := reached[node.ID]; ok {
			continue
		}
		reference, err := graphPathReference(
			graph.Path{Nodes: []graph.Node{node}}, nil)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, nil
}

func firstObservedID(values []shoal.ID) shoal.ID {
	values = canonicalObservedIDs(values)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func cloneEvidenceReferences(
	values []interaction.EvidenceReference,
) []interaction.EvidenceReference {
	if len(values) == 0 {
		return nil
	}
	result := make([]interaction.EvidenceReference, len(values))
	for index, value := range values {
		canonical, _ := value.Canonical()
		result[index] = canonical
	}
	return result
}

func canonicalToolObservation(
	ctx context.Context,
	value ToolObservation,
	decision auth.Decision,
) (ToolObservation, error) {
	value.RetrievedNodeIDs = canonicalObservedIDs(value.RetrievedNodeIDs)
	value.CitedNodeIDs = canonicalObservedIDs(value.CitedNodeIDs)
	var err error
	value.RetrievedEvidence, err = canonicalObservedEvidence(
		value.RetrievedEvidence)
	if err != nil {
		return ToolObservation{}, err
	}
	value.CitedEvidence, err = canonicalObservedEvidence(value.CitedEvidence)
	if err != nil {
		return ToolObservation{}, err
	}
	requiredVisibility := append(
		[]string(nil), value.RequiredVisibility...)
	if effective, ok := webapi.EffectiveWorkspaceSettings(ctx); ok {
		if len(effective.OutputPolicies()) > 0 {
			policy, policyErr := effective.OutputVisibility()
			if policyErr != nil {
				return ToolObservation{}, policyErr
			}
			if len(policy) > 0 {
				labels, err := interaction.ParseVisibility(string(policy))
				if err != nil {
					return ToolObservation{}, err
				}
				requiredVisibility = append(requiredVisibility, labels...)
			}
		}
	}
	value.RequiredVisibility, err = interaction.Conjoin(requiredVisibility)
	if err != nil {
		return ToolObservation{}, err
	}
	if len(value.RetrievedEvidence) > 0 &&
		!reflect.DeepEqual(
			value.RetrievedNodeIDs,
			observedEvidenceNodeIDs(value.RetrievedEvidence),
		) {
		return ToolObservation{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"tool retrieved nodes do not match complete evidence",
		)
	}
	if len(value.CitedEvidence) > 0 &&
		!reflect.DeepEqual(
			value.CitedNodeIDs,
			observedEvidenceNodeIDs(value.CitedEvidence),
		) {
		return ToolObservation{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"tool cited nodes do not match complete evidence",
		)
	}
	hasSnapshot := value.SnapshotID != "" || !value.SnapshotAsOf.IsZero()
	if hasSnapshot {
		if err := shoal.ValidateRequiredID(
			"tool observation snapshot ID", value.SnapshotID,
		); err != nil {
			return ToolObservation{}, err
		}
		if value.SnapshotAsOf.IsZero() {
			return ToolObservation{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"tool observation snapshot time is required",
			)
		}
		value.SnapshotAsOf = value.SnapshotAsOf.UTC()
	}
	hasAuthorization := value.AuthorizationFingerprint != "" ||
		!value.AuthorizationExpiresAt.IsZero() || value.RequestID != ""
	if hasAuthorization {
		fingerprint, err := auth.AuthorizationFingerprint(decision)
		if err != nil {
			return ToolObservation{}, shoal.NewError(
				shoal.ErrorUnauthorized, "authorization denied")
		}
		if value.AuthorizationFingerprint != shoal.ID(fingerprint.String()) ||
			!value.AuthorizationExpiresAt.Equal(
				decision.AuthenticationExpires()) ||
			value.RequestID != decision.RequestID() {
			return ToolObservation{}, shoal.NewError(
				shoal.ErrorUnauthorized,
				"tool observation authorization does not match the request",
			)
		}
		value.AuthorizationExpiresAt = value.AuthorizationExpiresAt.UTC()
	}
	if err := shoal.ValidateOptionalID(
		"tool observation embedding space ID",
		value.EmbeddingSpaceID,
	); err != nil {
		return ToolObservation{}, err
	}
	value.EmbeddingSpaceIDs = canonicalObservedIDs(value.EmbeddingSpaceIDs)
	if (value.EmbeddingSpaceID == "") !=
		(len(value.EmbeddingSpaceIDs) == 0) {
		return ToolObservation{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"tool observation embedding aggregate and constituents are inconsistent",
		)
	}
	if len(value.EmbeddingSpaceIDs) > 0 {
		expected, err := retrieval.EmbeddingSpaceSetID(
			value.EmbeddingSpaceIDs...)
		if err != nil || expected != value.EmbeddingSpaceID {
			return ToolObservation{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"tool observation embedding space identity is not canonical",
			)
		}
	}
	return value, nil
}

// canonicalObservedEvidence sorts the evidence by anchor. An anchor's ID is
// derived from its content, so the same anchor observed twice, as when two
// evidence paths of one result cite the same bytes, is one reference and is
// kept once, as the interaction recorder keeps it. Two references under one
// anchor that disagree are refused.
func canonicalObservedEvidence(
	values []interaction.EvidenceReference,
) ([]interaction.EvidenceReference, error) {
	if len(values) == 0 {
		return nil, nil
	}
	byAnchor := make(map[shoal.ID]interaction.EvidenceReference, len(values))
	for _, value := range values {
		canonical, err := value.Canonical()
		if err != nil {
			return nil, err
		}
		if existing, duplicate := byAnchor[canonical.AnchorID]; duplicate {
			if !reflect.DeepEqual(existing, canonical) {
				return nil, shoal.NewError(
					shoal.ErrorInvalidArgument,
					"tool observation evidence anchor has conflicting references",
				)
			}
			continue
		}
		byAnchor[canonical.AnchorID] = canonical
	}
	result := make([]interaction.EvidenceReference, 0, len(byAnchor))
	for _, value := range byAnchor {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i].AnchorID, result[j].AnchorID) < 0
	})
	return result, nil
}

func observedEvidenceNodeIDs(
	values []interaction.EvidenceReference,
) []shoal.ID {
	var ids []shoal.ID
	for _, value := range values {
		ids = append(ids, value.NodeIDs...)
	}
	return canonicalObservedIDs(ids)
}

func appendSectionIDs(ids []shoal.ID, section explorer.SectionView) []shoal.ID {
	ids = appendPresentID(ids, section.Section.ID)
	for _, span := range section.Spans {
		ids = appendPresentID(ids, span.ID)
	}
	for _, child := range section.Children {
		ids = appendSectionIDs(ids, child)
	}
	return ids
}

func appendPresentID(ids []shoal.ID, id shoal.ID) []shoal.ID {
	if id != "" {
		return append(ids, id)
	}
	return ids
}

func canonicalObservedIDs(ids []shoal.ID) []shoal.ID {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[shoal.ID]struct{}, len(ids))
	result := make([]shoal.ID, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return result
}
