// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package inference

import (
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AssertionEdgeIDMetadata is the assertion metadata key that names the graph
// edge an extracted assertion materialized.
const AssertionEdgeIDMetadata = "shoal.graph.edge_id"

// EdgeAssertionIDProperty is the graph edge property that names the
// assertion an edge materialized when the assertion's own metadata does not
// name the edge.
const EdgeAssertionIDProperty = "ontology.assertion.id"

// AssertionReferencesByEdge maps each edge that one of the given authoritative
// assertions materialized to the references a graph anchor over that edge
// must carry. The Explorer's recorder refuses a graph anchor that omits the
// assertion behind an asserted edge, so a recorder that was shown the
// assertions alongside the edges cites them through this map.
//
// An assertion is bound to an edge exactly as the Explorer's recorder binds
// them when it validates a recording: by naming the edge in its
// AssertionEdgeIDMetadata, by the edge naming it in EdgeAssertionIDProperty,
// or by sharing the edge's identity while its subject, predicate, object
// reference, and confidence reproduce the edge. The metadata binding alone
// is authoritative: an extracted relation's edge carries its relationship's
// key as its type and a canonical weight, while the relationship ID and the
// extraction's confidence stay on the assertion. A derived assertion is not
// source evidence and is refused, as is an assertion bound to none of the
// edges.
func AssertionReferencesByEdge(
	assertions []ontology.Assertion,
	edges []graph.Edge,
) (map[shoal.ID][]interaction.AssertionReference, error) {
	byID := make(map[shoal.ID]graph.Edge, len(edges))
	namedBy := make(map[shoal.ID][]shoal.ID)
	for _, edge := range edges {
		byID[edge.ID] = edge
		if assertionID := shoal.ID(
			edge.Properties[EdgeAssertionIDProperty]); assertionID != "" {
			namedBy[assertionID] = append(namedBy[assertionID], edge.ID)
		}
	}
	result := make(map[shoal.ID][]interaction.AssertionReference)
	for _, assertion := range assertions {
		if err := assertion.Validate(); err != nil {
			return nil, err
		}
		if assertion.Origin() == ontology.AssertionDerived {
			return nil, invalid(
				"derived assertion cannot be used as source graph evidence")
		}
		matched := make(map[shoal.ID]struct{})
		if edgeID := shoal.ID(
			assertion.Metadata()[AssertionEdgeIDMetadata]); edgeID != "" {
			if _, ok := byID[edgeID]; ok {
				matched[edgeID] = struct{}{}
			}
		}
		for _, edgeID := range namedBy[assertion.ID()] {
			matched[edgeID] = struct{}{}
		}
		if edge, ok := byID[assertion.ID()]; ok &&
			assertionReproducesEdge(assertion, edge) {
			matched[assertion.ID()] = struct{}{}
		}
		if len(matched) == 0 {
			return nil, invalid(
				"graph assertion has no authoritative neighborhood edge")
		}
		for edgeID := range matched {
			result[edgeID] = append(
				result[edgeID],
				interaction.AssertionReference{
					AssertionID: assertion.ID(),
					EdgeID:      edgeID,
					Origin:      assertion.Origin(),
				},
			)
		}
	}
	for edgeID := range result {
		sort.Slice(result[edgeID], func(i, j int) bool {
			if compared := shoal.CompareID(
				result[edgeID][i].AssertionID,
				result[edgeID][j].AssertionID,
			); compared != 0 {
				return compared < 0
			}
			return result[edgeID][i].Origin < result[edgeID][j].Origin
		})
	}
	return result, nil
}

// assertionReproducesEdge is the identity binding: an assertion whose ID is
// the edge's is its edge only when it reproduces the edge field for field.
func assertionReproducesEdge(assertion ontology.Assertion, edge graph.Edge) bool {
	target, ok := assertion.Object().ReferenceValue()
	return ok &&
		edge.From == assertion.Subject() &&
		edge.To == target &&
		edge.Type == string(assertion.Predicate()) &&
		math.Float64bits(float64(edge.Weight)) ==
			math.Float64bits(float64(assertion.Confidence()))
}
