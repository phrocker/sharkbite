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

package authorized

import (
	"context"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func (c *Client) verifyInteractionEvidence(
	ctx context.Context,
	session interaction.Session,
	decision auth.Decision,
	now time.Time,
) error {
	evidence := append(
		[]interaction.EvidenceReference(nil), session.SeedEvidence...)
	for _, turn := range session.Turns {
		if turn.ToolCall != nil {
			evidence = append(evidence, turn.ToolCall.RetrievedEvidence...)
		}
	}
	evidence = append(evidence, session.CitedEvidence...)
	for _, reference := range evidence {
		var err error
		switch reference.Kind {
		case interaction.EvidenceDocument:
			err = c.verifyInteractionDocumentEvidence(
				ctx, reference, decision, now)
		case interaction.EvidenceGraph:
			err = c.verifyInteractionGraphEvidence(
				ctx, reference, decision, now)
		default:
			err = authorizationDenied()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) verifyInteractionDocumentEvidence(
	ctx context.Context,
	reference interaction.EvidenceReference,
	decision auth.Decision,
	now time.Time,
) error {
	citation := reference.Citation
	registration, ok, err := c.policyStore.Revision(
		ctx, citation.DocumentID, citation.RevisionID)
	if err != nil {
		return policyCatalogReadError(ctx, err)
	}
	if !ok {
		return auth.ObjectNotFound()
	}
	allowed, err := c.revisionAllows(
		ctx, registration, decision, auth.OperationRetrieve, now)
	if err != nil {
		return err
	}
	if !allowed {
		return auth.ObjectNotFound()
	}
	view, err := c.base.Document(
		ctx, citation.DocumentID, citation.RevisionID)
	if err != nil {
		return directBaseError(err)
	}
	if err := verifyDocumentViewRegistration(view, registration); err != nil {
		return inconsistentBase()
	}
	index, err := buildCanonicalRetrievalDocument(view, registration)
	if err != nil {
		return inconsistentBase()
	}
	span, quote, ok := resolveInteractionCitation(index, citation)
	if !ok {
		return auth.ObjectNotFound()
	}
	expectedNodes := []shoal.ID{
		citation.DocumentID,
		span.SectionID,
		span.ID,
	}
	if !sameIDSets(reference.NodeIDs, expectedNodes) {
		return auth.ObjectNotFound()
	}
	anchor, err := inference.NewDocumentAnchor(citation, quote)
	if err != nil || anchor.ID() != reference.AnchorID {
		return auth.ObjectNotFound()
	}
	return nil
}

func resolveInteractionCitation(
	index *canonicalRetrievalDocument,
	citation document.Citation,
) (document.Span, string, bool) {
	var span document.Span
	if citation.SpanID != "" {
		var ok bool
		span, ok = index.spans[citation.SpanID]
		if !ok || (citation.SectionID != "" &&
			citation.SectionID != span.SectionID) {
			return document.Span{}, "", false
		}
	} else {
		for _, candidate := range index.spans {
			if candidate.SectionID != citation.SectionID ||
				!sourceRangeContains(candidate.Range, citation.Range) {
				continue
			}
			start := citation.Range.Start.Offset - candidate.Range.Start.Offset
			end := citation.Range.End.Offset - candidate.Range.Start.Offset
			if start < 0 || end <= start || end > int64(len(candidate.Text)) {
				continue
			}
			if span.ID != "" {
				return document.Span{}, "", false
			}
			span = candidate
		}
		if span.ID == "" {
			return document.Span{}, "", false
		}
	}
	if !sourceRangeContains(span.Range, citation.Range) {
		return document.Span{}, "", false
	}
	start := citation.Range.Start.Offset - span.Range.Start.Offset
	end := citation.Range.End.Offset - span.Range.Start.Offset
	if start < 0 || end <= start || end > int64(len(span.Text)) {
		return document.Span{}, "", false
	}
	return span, span.Text[start:end], true
}

func sourceRangeContains(outer, inner document.SourceRange) bool {
	return outer.Start.Offset <= inner.Start.Offset &&
		inner.End.Offset <= outer.End.Offset
}

func (c *Client) verifyInteractionGraphEvidence(
	ctx context.Context,
	reference interaction.EvidenceReference,
	decision auth.Decision,
	now time.Time,
) error {
	reader := readerFor(decision, auth.OperationRetrieve, now)
	nodes, err := c.resolveNodes(ctx, reference.NodeIDs, reader)
	if err != nil {
		return err
	}
	if len(nodes) != len(reference.NodeIDs) {
		return auth.ObjectNotFound()
	}
	canonicalNodes, err := c.canonicalRegisteredNodes(ctx, nodes)
	if err != nil {
		return err
	}
	edges, err := c.resolveEdges(ctx, reference.EdgeIDs, reader)
	if err != nil {
		return err
	}
	if len(edges) != len(reference.EdgeIDs) {
		return auth.ObjectNotFound()
	}
	if len(reference.NodeIDs) > inference.MaxPathNodes ||
		len(reference.EdgeIDs) > inference.MaxPathEdges {
		return auth.ObjectNotFound()
	}
	expectedNodes := make([]shoal.ID, 0, len(edges)*2)
	for _, edgeID := range reference.EdgeIDs {
		registration := edges[edgeID]
		allowed, allowErr := c.edgeAllows(
			ctx, registration, decision, auth.OperationRetrieve, now)
		if allowErr != nil {
			return allowErr
		}
		if !allowed {
			return auth.ObjectNotFound()
		}
		expectedNodes = append(
			expectedNodes, registration.Edge.From, registration.Edge.To)
	}
	if len(edges) == 0 {
		if len(reference.NodeIDs) != 1 {
			return auth.ObjectNotFound()
		}
	} else if !sameIDSets(reference.NodeIDs, expectedNodes) {
		return auth.ObjectNotFound()
	}

	raw, err := c.base.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: reference.NodeIDs,
		Depth:   1,
	})
	if err != nil {
		return directBaseError(err)
	}
	rawEdges := make(map[shoal.ID]graph.Edge, len(raw.Edges))
	for _, edge := range raw.Edges {
		if err := edge.Validate(); err != nil {
			return inconsistentBase()
		}
		if _, duplicate := rawEdges[edge.ID]; duplicate {
			return inconsistentBase()
		}
		rawEdges[edge.ID] = edge
	}
	for edgeID, registration := range edges {
		rawEdge, ok := rawEdges[edgeID]
		if !ok || !graphEdgesEqual(rawEdge, registration.Edge) {
			return auth.ObjectNotFound()
		}
	}
	rawAssertions, err := derivedAssertionsByEdge(raw.Assertions)
	if err != nil {
		return err
	}
	// The recorder was shown, for each extracted relation, an assertion it
	// may read (readerSafeExtractionAssertions); a shared relation's may
	// not be the base's own (#700). Expect exactly that one.
	extracted := make(map[shoal.ID]ontology.Assertion)
	pathEdges := make(map[shoal.ID]graph.Edge, len(edges))
	for edgeID, registration := range edges {
		pathEdges[edgeID] = registration.Edge
		if assertion, ok := rawAssertions[edgeID]; ok &&
			assertion.Origin() != ontology.AssertionDerived {
			extracted[edgeID] = assertion
		}
	}
	readable, err := c.readerSafeExtractionAssertions(
		ctx, pathEdges, extracted, canonicalNodes,
		decision, auth.OperationRetrieve, now)
	if err != nil {
		return err
	}
	for edgeID := range extracted {
		if assertion, ok := readable[edgeID]; ok {
			rawAssertions[edgeID] = assertion
		} else {
			delete(rawAssertions, edgeID)
		}
	}
	expectedAssertions, err := interactionAssertionsForEdges(edges, rawAssertions)
	if err != nil ||
		!sameAssertionReferences(reference.Assertions, expectedAssertions) {
		return auth.ObjectNotFound()
	}

	orderedEdges := make([]graph.Edge, 0, len(reference.EdgeIDs))
	for _, edgeID := range reference.EdgeIDs {
		orderedEdges = append(orderedEdges, edges[edgeID].Edge)
	}
	if !interactionAnchorMatchesGraph(
		reference.AnchorID, canonicalNodes, orderedEdges) {
		return auth.ObjectNotFound()
	}
	return nil
}

func interactionAssertionsForEdges(
	edges registeredEdges,
	assertions map[shoal.ID]ontology.Assertion,
) ([]interaction.AssertionReference, error) {
	result := make([]interaction.AssertionReference, 0, len(edges))
	for edgeID, registration := range edges {
		assertion, ok := assertions[edgeID]
		marked := registration.Edge.Properties["ontology_relationship_id"] != "" ||
			registration.Edge.Properties["ontology.assertion.origin"] != ""
		if marked && !ok {
			return nil, authorizationDenied()
		}
		if !ok {
			continue
		}
		target, targetOK := assertion.Object().ReferenceValue()
		if !targetOK ||
			assertion.Subject() != registration.Edge.From ||
			target != registration.Edge.To ||
			// An extracted relation's weight is canonical (#700).
			(registration.Edge.Properties["ontology_relationship_id"] == "" &&
				assertion.Confidence() != registration.Edge.Weight) {
			return nil, authorizationDenied()
		}
		if relationship := registration.Edge.Properties["ontology_relationship_id"]; relationship != "" &&
			relationship != string(assertion.Predicate()) {
			return nil, authorizationDenied()
		}
		if origin := registration.Edge.Properties["ontology.assertion.origin"]; origin != "" &&
			origin != string(assertion.Origin()) {
			return nil, authorizationDenied()
		}
		result = append(result, interaction.AssertionReference{
			AssertionID: assertion.ID(),
			EdgeID:      edgeID,
			Origin:      assertion.Origin(),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if compared := shoal.CompareID(
			result[i].EdgeID, result[j].EdgeID); compared != 0 {
			return compared < 0
		}
		return shoal.CompareID(
			result[i].AssertionID, result[j].AssertionID) < 0
	})
	return result, nil
}

func interactionAnchorMatchesGraph(
	anchorID shoal.ID,
	nodes map[shoal.ID]graph.Node,
	edges []graph.Edge,
) bool {
	if len(edges) == 0 {
		if len(nodes) != 1 {
			return false
		}
		for _, node := range nodes {
			anchor, err := inference.NewGraphAnchor(
				graph.Path{Nodes: []graph.Node{node}})
			return err == nil && anchor.ID() == anchorID
		}
	}
	byFrom := make(map[shoal.ID][]int, len(edges))
	for index, edge := range edges {
		byFrom[edge.From] = append(byFrom[edge.From], index)
	}
	for nodeID := range byFrom {
		sort.Slice(byFrom[nodeID], func(i, j int) bool {
			return shoal.CompareID(
				edges[byFrom[nodeID][i]].ID,
				edges[byFrom[nodeID][j]].ID,
			) < 0
		})
	}
	degree := make(map[shoal.ID]int, len(nodes))
	for _, edge := range edges {
		degree[edge.From]++
		degree[edge.To]--
	}
	starts := make([]shoal.ID, 0, len(nodes))
	endCount := 0
	for nodeID, difference := range degree {
		switch difference {
		case 1:
			starts = append(starts, nodeID)
		case -1:
			endCount++
		case 0:
		default:
			return false
		}
	}
	switch {
	case len(starts) == 1 && endCount == 1:
	case len(starts) == 0 && endCount == 0:
		for nodeID := range byFrom {
			starts = append(starts, nodeID)
		}
	default:
		return false
	}
	sort.Slice(starts, func(i, j int) bool {
		return shoal.CompareID(starts[i], starts[j]) < 0
	})
	used := make([]bool, len(edges))
	const maxSearchStates = 65536
	candidates := 0
	var search func(shoal.ID, []graph.Node, []graph.Edge) bool
	search = func(
		current shoal.ID,
		pathNodes []graph.Node,
		pathEdges []graph.Edge,
	) bool {
		candidates++
		if candidates > maxSearchStates {
			return false
		}
		if len(pathEdges) == len(edges) {
			if len(pathNodes) != len(edges)+1 {
				return false
			}
			if !sameNodeSet(pathNodes, nodes) {
				return false
			}
			anchor, err := inference.NewGraphAnchor(graph.Path{
				Nodes: pathNodes,
				Edges: pathEdges,
			})
			return err == nil && anchor.ID() == anchorID
		}
		for _, index := range byFrom[current] {
			if used[index] {
				continue
			}
			next, ok := nodes[edges[index].To]
			if !ok {
				continue
			}
			used[index] = true
			if search(
				edges[index].To,
				append(pathNodes, next),
				append(pathEdges, edges[index]),
			) {
				return true
			}
			used[index] = false
			if candidates > maxSearchStates {
				return false
			}
		}
		return false
	}
	for _, start := range starts {
		for index := range used {
			used[index] = false
		}
		if search(start, []graph.Node{nodes[start]}, nil) {
			return true
		}
		if candidates > maxSearchStates {
			return false
		}
	}
	return false
}

func sameNodeSet(values []graph.Node, expected map[shoal.ID]graph.Node) bool {
	seen := make(map[shoal.ID]struct{}, len(values))
	for _, value := range values {
		canonical, ok := expected[value.ID]
		if !ok || !graphNodesEqual(value, canonical) {
			return false
		}
		seen[value.ID] = struct{}{}
	}
	return len(seen) == len(expected)
}

func sameIDSets(left, right []shoal.ID) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	leftSet := make(map[shoal.ID]struct{}, len(left))
	rightSet := make(map[shoal.ID]struct{}, len(right))
	for _, id := range left {
		leftSet[id] = struct{}{}
	}
	for _, id := range right {
		rightSet[id] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for id := range leftSet {
		if _, ok := rightSet[id]; !ok {
			return false
		}
	}
	return true
}

func sameAssertionReferences(
	left, right []interaction.AssertionReference,
) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]interaction.AssertionReference(nil), left...)
	right = append([]interaction.AssertionReference(nil), right...)
	sort.Slice(left, func(i, j int) bool {
		if compared := shoal.CompareID(
			left[i].EdgeID, left[j].EdgeID); compared != 0 {
			return compared < 0
		}
		return shoal.CompareID(
			left[i].AssertionID, left[j].AssertionID) < 0
	})
	sort.Slice(right, func(i, j int) bool {
		if compared := shoal.CompareID(
			right[i].EdgeID, right[j].EdgeID); compared != 0 {
			return compared < 0
		}
		return shoal.CompareID(
			right[i].AssertionID, right[j].AssertionID) < 0
	})
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
