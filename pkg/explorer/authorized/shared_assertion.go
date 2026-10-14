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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ExtractionAssertionReader is the explicitly trusted source of every
// extraction's assertion of a relation edge (#700). Several documents can
// assert the same relation, but the base graph attaches only one assertion to
// the edge, and that assertion's evidence quotes its own document. So a
// reader who sees a shared relation through one asserting document could
// otherwise be shown another document's assertion, and its quote. NewClient
// promotes it from SnapshotValidator, never from Base.
type ExtractionAssertionReader interface {
	ExtractionAssertions(
		context.Context, []shoal.ID,
	) (map[shoal.ID][]ontology.Assertion, error)
}

// readerSafeExtractionAssertions returns, for each admitted edge that carries
// an extraction assertion, an assertion of it whose every piece of evidence
// the reader may read: the base's own when it qualifies, and otherwise the
// first one, in extraction order, from the trusted reader. An edge absent
// from the result has no such assertion, and the caller withholds it, as it
// withholds anything it cannot show without another document.
func (c *Client) readerSafeExtractionAssertions(
	ctx context.Context,
	edges map[shoal.ID]graph.Edge,
	assertions map[shoal.ID]ontology.Assertion,
	visibleNodes map[shoal.ID]graph.Node,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (map[shoal.ID]ontology.Assertion, error) {
	if len(assertions) == 0 {
		return assertions, nil
	}
	evidence := c.newEvidenceGate(decision, operation, now, visibleNodes, edges)
	safe := make(map[shoal.ID]ontology.Assertion, len(assertions))
	var unreadable []shoal.ID
	if err := evidence.load(ctx, mapValues(assertions)); err != nil {
		return nil, err
	}
	for edgeID, assertion := range assertions {
		readable, err := evidence.readable(assertion)
		if err != nil {
			return nil, err
		}
		if readable {
			safe[edgeID] = assertion
			continue
		}
		unreadable = append(unreadable, edgeID)
	}
	if len(unreadable) == 0 || isNilDependency(c.relationAssertions) {
		return safe, nil
	}
	sortIDs(unreadable)
	for start := 0; start < len(unreadable); {
		end := chunkEnd(start, len(unreadable), maxInteractionAuthorizationIDs)
		chunk := unreadable[start:end]
		start = end
		alternatives, err := c.relationAssertions.ExtractionAssertions(ctx, chunk)
		if err != nil {
			return nil, directBaseError(err)
		}
		var candidates []ontology.Assertion
		for _, edgeID := range chunk {
			candidates = append(candidates, alternatives[edgeID]...)
		}
		if err := evidence.load(ctx, candidates); err != nil {
			return nil, err
		}
		for _, edgeID := range chunk {
			for _, alternative := range alternatives[edgeID] {
				if err := alternative.Validate(); err != nil {
					return nil, inconsistentBase()
				}
				if shoal.ID(alternative.Metadata()[graphAssertionEdgeIDMetadata]) != edgeID ||
					alternative.Origin() == ontology.AssertionDerived {
					return nil, inconsistentBase()
				}
				readable, err := evidence.readable(alternative)
				if err != nil {
					return nil, err
				}
				if readable {
					safe[edgeID] = alternative
					break
				}
			}
		}
	}
	return safe, nil
}

func mapValues(assertions map[shoal.ID]ontology.Assertion) []ontology.Assertion {
	values := make([]ontology.Assertion, 0, len(assertions))
	for _, assertion := range assertions {
		values = append(values, assertion)
	}
	return values
}

// evidenceGate decides whether a reader may read every piece of an
// assertion's evidence: each cited document revision under revisionAllows,
// and each node and edge of a graph path as a graph read would admit it.
type evidenceGate struct {
	client    *Client
	revisions *revisionGate
	reader    *registrationReader
	nodes     map[shoal.ID]bool
	edges     map[shoal.ID]graph.Edge
}

func (c *Client) newEvidenceGate(
	decision auth.Decision, operation auth.Operation, now time.Time,
	visibleNodes map[shoal.ID]graph.Node, admittedEdges map[shoal.ID]graph.Edge,
) *evidenceGate {
	nodes := make(map[shoal.ID]bool, len(visibleNodes))
	for id := range visibleNodes {
		nodes[id] = true
	}
	return &evidenceGate{
		client:    c,
		revisions: c.newRevisionGate(decision, operation, now),
		reader:    readerFor(decision, operation, now),
		nodes:     nodes,
		edges:     admittedEdges,
	}
}

// load resolves, in bounded batches, what deciding assertions needs.
func (g *evidenceGate) load(ctx context.Context, assertions []ontology.Assertion) error {
	var references []RevisionReference
	var nodeIDs []shoal.ID
	for _, assertion := range assertions {
		for _, item := range assertion.Evidence() {
			citation := item.Citation()
			if citation.DocumentID != "" && citation.RevisionID != "" {
				references = append(references, RevisionReference{
					DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
				})
			}
			if path, ok := item.Path(); ok {
				for _, node := range path.Nodes {
					if _, known := g.nodes[node.ID]; !known && node.ID != "" {
						nodeIDs = append(nodeIDs, node.ID)
					}
				}
			}
		}
	}
	if len(references) > 0 {
		canonical, err := canonicalRevisionReferences(references)
		if err != nil {
			return inconsistentBase()
		}
		if err := g.revisions.cache.loadReferences(ctx, canonical); err != nil {
			return err
		}
	}
	for start := 0; start < len(nodeIDs); {
		end := chunkEnd(start, len(nodeIDs), maxInteractionAuthorizationIDs)
		chunk := nodeIDs[start:end]
		start = end
		resolved, err := g.client.resolveNodes(ctx, chunk, g.reader)
		if err != nil {
			return err
		}
		for _, nodeID := range chunk {
			registration, ok := resolved[nodeID]
			allowed := false
			if ok {
				allowed, err = ruleAllows(registration.Rule,
					g.reader.decision, g.reader.operation, g.reader.now)
				if err != nil {
					return err
				}
			}
			g.nodes[nodeID] = allowed
		}
	}
	return nil
}

func (g *evidenceGate) readable(assertion ontology.Assertion) (bool, error) {
	for _, item := range assertion.Evidence() {
		citation := item.Citation()
		if citation.DocumentID != "" || citation.RevisionID != "" {
			if citation.DocumentID == "" || citation.RevisionID == "" {
				return false, nil
			}
			allowed, err := g.revisions.referenceAllows(RevisionReference{
				DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
			})
			if err != nil || !allowed {
				return false, err
			}
		}
		path, ok := item.Path()
		if !ok {
			continue
		}
		for _, node := range path.Nodes {
			if !g.nodes[node.ID] {
				return false, nil
			}
		}
		for _, edge := range path.Edges {
			if _, ok := g.edges[edge.ID]; !ok {
				return false, nil
			}
		}
	}
	return true, nil
}
