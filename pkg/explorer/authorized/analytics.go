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

package authorized

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	exploreranalytics "github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaterializeAnalytics builds a complete, explicitly bounded subgraph under
// OperationAnalyticsRead. It pages each expanded node independently through
// the production authorization filter so hidden edges never consume the
// caller-visible fanout. Any incomplete page or exhausted bound fails closed.
// Every bound that can fail is one on the visible subgraph, except the
// uniform per-node MaxBoundedNodeAdjacency (#698); request.MaxScannedEdges
// must be nonzero but, being caller-chosen, does not decide the outcome.
func (c *Client) MaterializeAnalytics(
	ctx context.Context,
	request explorer.BoundedNeighborhoodRequest,
	maxEdges uint32,
	maxEvidenceBytes uint64,
) (exploreranalytics.Materialization, error) {
	bounded, err := c.boundedBase()
	if err != nil {
		return exploreranalytics.Materialization{}, err
	}
	if request.Depth == 0 || request.Fanout == 0 ||
		request.MaxNodes == 0 || request.MaxScannedEdges == 0 ||
		maxEdges == 0 || maxEvidenceBytes == 0 {
		return exploreranalytics.Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics graph limits must be nonzero")
	}
	if request.AfterEdgeID != "" {
		return exploreranalytics.Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics materialization does not accept a cursor")
	}
	if request.Direction == "" {
		request.Direction = explorer.GraphDirectionBoth
	}
	switch request.Direction {
	case explorer.GraphDirectionBoth,
		explorer.GraphDirectionOutgoing,
		explorer.GraphDirectionIncoming:
	default:
		return exploreranalytics.Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown graph direction")
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: request.Depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return exploreranalytics.Materialization{}, err
	}
	if uint32(len(normalized.NodeIDs)) > request.MaxNodes {
		return exploreranalytics.Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics seeds exceed max_nodes")
	}
	request.NodeIDs = normalized.NodeIDs
	request.EdgeTypes = normalized.EdgeTypes

	decision, guard, now, err := c.begin(ctx, auth.OperationAnalyticsRead)
	if err != nil {
		return exploreranalytics.Materialization{}, err
	}
	before, err := bounded.Snapshot(ctx)
	if err != nil {
		return exploreranalytics.Materialization{}, directBaseError(err)
	}
	ctx, err = c.withCanonicalDocumentIndex(ctx)
	if err != nil {
		return exploreranalytics.Materialization{}, err
	}
	for _, nodeID := range normalized.NodeIDs {
		if possibleProvenanceNodeID(nodeID) {
			continue
		}
		if _, err := c.authorizedNode(
			ctx, nodeID, decision, auth.OperationAnalyticsRead, now,
		); err != nil {
			return exploreranalytics.Materialization{}, err
		}
	}
	neighborhood, err := c.materializeAnalyticsNeighborhood(
		ctx, bounded, request, maxEdges, maxEvidenceBytes, decision, guard, now)
	if err != nil {
		return exploreranalytics.Materialization{}, err
	}
	neighborhood, err = c.applyOntologyLens(ctx, neighborhood, decision)
	if err != nil {
		return exploreranalytics.Materialization{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return exploreranalytics.Materialization{}, err
	}
	after, err := bounded.Snapshot(ctx)
	if err != nil {
		return exploreranalytics.Materialization{}, directBaseError(err)
	}
	if before != after {
		return exploreranalytics.Materialization{}, shoal.NewError(
			shoal.ErrorConflict, "corpus changed while materializing analytics")
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return exploreranalytics.Materialization{}, authorizationDenied()
	}
	selected, _ := decision.SelectedOntology()
	return exploreranalytics.Materialization{
		Snapshot: before, Neighborhood: cloneAnalyticsNeighborhood(neighborhood),
		AuthorizationFingerprint: fingerprint,
		PolicyGeneration:         decision.PolicyGeneration(),
		SelectedOntology:         selected,
		RequestID:                decision.RequestID(),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		Complete:                 true,
	}, nil
}

func (c *Client) materializeAnalyticsNeighborhood(
	ctx context.Context,
	bounded explorer.BoundedClient,
	request explorer.BoundedNeighborhoodRequest,
	maxEdges uint32,
	maxEvidenceBytes uint64,
	decision auth.Decision,
	guard auth.GenerationGuard,
	now time.Time,
) (explorer.Neighborhood, error) {
	ctx, err := c.withCanonicalDocumentIndex(ctx)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	ctx = withNeighborhoodAuthorizationCache(ctx)
	nodes := make(map[shoal.ID]graph.Node, len(request.NodeIDs))
	edges := make(map[shoal.ID]graph.Edge)
	assertions := make(map[shoal.ID]ontology.Assertion)
	evidenceBytes := uint64(2)
	charge := func(value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return inconsistentBase()
		}
		size := uint64(len(encoded)) + 1
		if size > maxEvidenceBytes || evidenceBytes > maxEvidenceBytes-size {
			return shoal.NewError(
				shoal.ErrorUnavailable,
				"authorized analytics evidence exceeds max_evidence_bytes",
			)
		}
		evidenceBytes += size
		return nil
	}
	seen := make(map[shoal.ID]struct{}, len(request.NodeIDs))
	frontier := append([]shoal.ID(nil), request.NodeIDs...)
	sort.Slice(frontier, func(i, j int) bool {
		return shoal.CompareID(frontier[i], frontier[j]) < 0
	})
	for _, nodeID := range frontier {
		seen[nodeID] = struct{}{}
	}

	for level := uint32(0); level < request.Depth && len(frontier) > 0; level++ {
		nextSet := make(map[shoal.ID]struct{})
		for _, seed := range frontier {
			if err := contextFailure(ctx); err != nil {
				return explorer.Neighborhood{}, err
			}
			pageRequest := request
			pageRequest.NodeIDs = []shoal.ID{seed}
			pageRequest.Depth = 1
			pageRequest.AfterEdgeID = ""
			page, err := c.boundedAuthorizedNeighborhoodPage(
				ctx,
				bounded,
				pageRequest,
				explorer.NeighborhoodRequest{
					NodeIDs: []shoal.ID{seed}, Depth: 1,
					EdgeTypes: append([]string(nil), request.EdgeTypes...),
				},
				decision,
				guard,
				now,
				request.Direction,
				auth.OperationAnalyticsRead,
				false,
				false,
			)
			if err != nil {
				return explorer.Neighborhood{}, err
			}
			if page.Truncated || page.Continuation || page.NextAfterEdgeID != "" {
				return explorer.Neighborhood{}, shoal.NewError(
					shoal.ErrorUnavailable,
					"authorized analytics subgraph exceeds its explicit bounds",
				)
			}
			for _, node := range page.Neighborhood.Nodes {
				if _, exists := nodes[node.ID]; exists {
					continue
				}
				if uint32(len(nodes)) >= request.MaxNodes {
					return explorer.Neighborhood{}, shoal.NewError(
						shoal.ErrorUnavailable,
						"authorized analytics subgraph exceeds max_nodes",
					)
				}
				if err := charge(node); err != nil {
					return explorer.Neighborhood{}, err
				}
				nodes[node.ID] = cloneGraphNode(node)
			}
			if _, ok := nodes[seed]; !ok {
				return explorer.Neighborhood{}, inconsistentBase()
			}
			for _, edge := range page.Neighborhood.Edges {
				if _, exists := edges[edge.ID]; !exists {
					if uint32(len(edges)) >= maxEdges {
						return explorer.Neighborhood{}, shoal.NewError(
							shoal.ErrorUnavailable,
							"authorized analytics subgraph exceeds max_edges",
						)
					}
					if err := charge(edge); err != nil {
						return explorer.Neighborhood{}, err
					}
					edges[edge.ID] = cloneGraphEdge(edge)
				}
				other, ok := analyticsNeighbor(seed, edge, request.Direction)
				if !ok {
					return explorer.Neighborhood{}, inconsistentBase()
				}
				if _, visited := seen[other]; !visited {
					nextSet[other] = struct{}{}
				}
			}
			for _, assertion := range page.Neighborhood.Assertions {
				if _, exists := assertions[assertion.ID()]; !exists {
					evidence, err :=
						exploreranalytics.InteractionAssertionEvidence(assertion)
					if err != nil {
						return explorer.Neighborhood{}, err
					}
					if err := charge(evidence); err != nil {
						return explorer.Neighborhood{}, err
					}
					assertions[assertion.ID()] = assertion
				}
			}
		}
		frontier = frontier[:0]
		for nodeID := range nextSet {
			seen[nodeID] = struct{}{}
			frontier = append(frontier, nodeID)
		}
		sort.Slice(frontier, func(i, j int) bool {
			return shoal.CompareID(frontier[i], frontier[j]) < 0
		})
	}

	result := explorer.Neighborhood{
		Nodes:      make([]graph.Node, 0, len(nodes)),
		Edges:      make([]graph.Edge, 0, len(edges)),
		Assertions: make([]ontology.Assertion, 0, len(assertions)),
	}
	for _, node := range nodes {
		result.Nodes = append(result.Nodes, cloneGraphNode(node))
	}
	for _, edge := range edges {
		if _, ok := nodes[edge.From]; !ok {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		if _, ok := nodes[edge.To]; !ok {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		result.Edges = append(result.Edges, cloneGraphEdge(edge))
	}
	for _, assertion := range assertions {
		result.Assertions = append(result.Assertions, assertion)
	}
	sort.Slice(result.Nodes, func(i, j int) bool {
		return shoal.CompareID(result.Nodes[i].ID, result.Nodes[j].ID) < 0
	})
	sort.Slice(result.Edges, func(i, j int) bool {
		return shoal.CompareID(result.Edges[i].ID, result.Edges[j].ID) < 0
	})
	sort.Slice(result.Assertions, func(i, j int) bool {
		return shoal.CompareID(result.Assertions[i].ID(), result.Assertions[j].ID()) < 0
	})
	return result, nil
}

func analyticsNeighbor(
	seed shoal.ID,
	edge graph.Edge,
	direction explorer.GraphDirection,
) (shoal.ID, bool) {
	switch direction {
	case explorer.GraphDirectionOutgoing:
		if edge.From != seed {
			return "", false
		}
		return edge.To, true
	case explorer.GraphDirectionIncoming:
		if edge.To != seed {
			return "", false
		}
		return edge.From, true
	default:
		switch {
		case edge.From == seed:
			return edge.To, true
		case edge.To == seed:
			return edge.From, true
		default:
			return "", false
		}
	}
}

// RevalidateAnalytics confirms that neither the bound identity/lens, the
// authorization generation, nor the corpus changed while analytics ran.
func (c *Client) RevalidateAnalytics(
	ctx context.Context,
	materialization exploreranalytics.Materialization,
) error {
	bounded, err := c.boundedBase()
	if err != nil {
		return err
	}
	decision, guard, _, err := c.begin(ctx, auth.OperationAnalyticsRead)
	if err != nil {
		return err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil || fingerprint != materialization.AuthorizationFingerprint {
		return authorizationDenied()
	}
	if err := guard.Check(ctx); err != nil {
		return err
	}
	snapshot, err := bounded.Snapshot(ctx)
	if err != nil {
		return directBaseError(err)
	}
	if snapshot != materialization.Snapshot {
		return shoal.NewError(
			shoal.ErrorConflict, "corpus changed while computing analytics")
	}
	return guard.Check(ctx)
}

func cloneAnalyticsNeighborhood(value explorer.Neighborhood) explorer.Neighborhood {
	cloned := explorer.Neighborhood{
		Nodes:           make([]graph.Node, 0, len(value.Nodes)),
		Edges:           make([]graph.Edge, 0, len(value.Edges)),
		Assertions:      append([]ontology.Assertion(nil), value.Assertions...),
		Interpretations: append([]ontology.AssertionInterpretation(nil), value.Interpretations...),
	}
	for _, node := range value.Nodes {
		cloned.Nodes = append(cloned.Nodes, cloneGraphNode(node))
	}
	for _, edge := range value.Edges {
		cloned.Edges = append(cloned.Edges, cloneGraphEdge(edge))
	}
	return cloned
}
