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

// AssertionReferencesByEdge maps each edge that one of the given authoritative
// assertions materialized to the references a graph anchor over that edge
// must carry. The Explorer's recorder refuses a graph anchor that omits the
// assertion behind an asserted edge, so a recorder that was shown the
// assertions alongside the edges cites them through this map.
//
// An assertion is bound to an edge by its AssertionEdgeIDMetadata, or by
// sharing the edge's identity; either way its subject, predicate, object
// reference, and confidence must reproduce the edge exactly. A derived
// assertion is not source evidence and is refused, as is an assertion that
// matches none of the edges.
func AssertionReferencesByEdge(
	assertions []ontology.Assertion,
	edges []graph.Edge,
) (map[shoal.ID][]interaction.AssertionReference, error) {
	byID := make(map[shoal.ID]graph.Edge, len(edges))
	for _, edge := range edges {
		byID[edge.ID] = edge
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
			if edge, ok := byID[edgeID]; ok {
				if !assertionMatchesEdge(assertion, edge) {
					return nil, invalid(
						"graph assertion does not match its authoritative edge")
				}
				matched[edgeID] = struct{}{}
			}
		}
		if edge, ok := byID[assertion.ID()]; ok {
			if !assertionMatchesEdge(assertion, edge) {
				return nil, invalid(
					"graph assertion does not match its authoritative edge")
			}
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

func assertionMatchesEdge(assertion ontology.Assertion, edge graph.Edge) bool {
	target, ok := assertion.Object().ReferenceValue()
	return ok &&
		edge.From == assertion.Subject() &&
		edge.To == target &&
		edge.Type == string(assertion.Predicate()) &&
		math.Float64bits(float64(edge.Weight)) ==
			math.Float64bits(float64(assertion.Confidence()))
}
