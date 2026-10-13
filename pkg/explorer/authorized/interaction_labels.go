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

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Output labels on interaction and fold reads (#567, #568).
//
// An interaction record stores the output restriction its producer required
// of a reader (Session.RequiredVisibility), and the label expression stamped
// on its derived graph (Summary.Visibility, and the shoal.visibility metadata
// of each derived node and edge). The touched-source re-check every read
// already makes enforces each touched node's AccessRule, label policies
// included (#570); it does not enforce the output restriction, whose labels
// belong to no touched node (for example a workspace output policy).
//
// So every read here evaluates the record's output restriction with the
// reader label evaluator (label_visibility.go). A record the reader may not
// see is ObjectNotFound on a point read and absent from a list, so what an
// outsider receives is byte-for-byte what it would receive had the record
// never been written: no count, no placeholder, no "filtered" marker.
//
// Records are never partially withheld. One conjunction is stamped on every
// derived node and edge, and turns carry no labels of their own, so there is
// nothing finer to withhold.
//
// The label expression. #574 withheld it from every reader that did not
// record the session, because no read could yet ask whether a reader held
// the labels it names. The evaluator answers that, so the recorder rule is
// replaced by the question itself: the expression is shown exactly when the
// reader may see every term it names. Its free-form node labels translate to
// the touched nodes' label policies (which the touched-source re-check has
// already required of the reader) and its structured terms are decided as
// they stand. A reader who may see the record but not the whole expression
// (a trusted service whose ceiling lacks a touched node's label, or a record
// whose producer kept an output label out of RequiredVisibility) receives
// the record with the expression cleared exactly as an unlabelled record
// stores it.
//
// A fold is visible only when every member session is and the reader may see
// the fold's own visibility, which conjoins every member's expression. So a
// visible fold's expressions are all permitted and are returned unchanged.

// readableInteraction returns record as the reader may see it, and false when
// the reader may not see it at all. record has already passed the touched
// source re-check (or the recorder check for a tombstone or zero-hit record).
func (c *Client) readableInteraction(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	record explorer.InteractionRecord,
) (explorer.InteractionRecord, bool, error) {
	required, err := c.structuredLabels(
		ctx, readerFor(decision, auth.OperationRead, now),
		record.Session.RequiredVisibility,
		record.TouchedNodeIDs, record.TouchedEdgeIDs)
	if err != nil {
		return explorer.InteractionRecord{}, false, err
	}
	visible, err := c.labelVisibility.permits(ctx, decision, required, now)
	if err != nil || !visible {
		return explorer.InteractionRecord{}, false, err
	}
	permitted, err := c.expressionPermitted(
		ctx, decision, now, record.Summary.Visibility,
		record.TouchedNodeIDs, record.TouchedEdgeIDs)
	if err != nil {
		return explorer.InteractionRecord{}, false, err
	}
	if !permitted {
		record.Summary.Visibility = ""
	}
	return record, true, nil
}

// expressionPermitted reports whether the reader may see every term of a
// stored label expression, translated over the touched nodes that stamped it.
func (c *Client) expressionPermitted(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	expression string,
	nodeIDs, edgeIDs []shoal.ID,
) (bool, error) {
	if expression == "" {
		return true, nil
	}
	labels, err := c.structuredLabels(
		ctx, readerFor(decision, auth.OperationRead, now),
		auth.SplitVisibilityConjunction(expression), nodeIDs, edgeIDs)
	if err != nil {
		return false, err
	}
	return c.labelVisibility.permits(ctx, decision, labels, now)
}

// withholdSubgraphLabels clears the visibility metadata
// ConjoinSubgraphVisibility stamps on every derived node and edge of an
// interaction subgraph, unless the reader may see the expression. A record
// stored without labels never sets these keys, so deleting them reproduces
// its shape exactly.
func withholdSubgraphLabels(
	subgraph explorer.Neighborhood, permitted bool,
) explorer.Neighborhood {
	if permitted {
		return subgraph
	}
	nodes := make([]graph.Node, len(subgraph.Nodes))
	for index, node := range subgraph.Nodes {
		node.Properties = withholdVisibilityMetadata(node.Properties)
		nodes[index] = node
	}
	edges := make([]graph.Edge, len(subgraph.Edges))
	for index, edge := range subgraph.Edges {
		edge.Properties = withholdVisibilityMetadata(edge.Properties)
		edges[index] = edge
	}
	if subgraph.Nodes != nil {
		subgraph.Nodes = nodes
	}
	if subgraph.Edges != nil {
		subgraph.Edges = edges
	}
	return subgraph
}

func withholdVisibilityMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	cloned := cloneMetadata(metadata)
	// The digest and term count stand in for an expression too long for one
	// metadata value. A digest of a short label conjunction is guessable, so
	// it is withheld with the expression it stands for.
	delete(cloned, interaction.PropertyVisibility)
	delete(cloned, interaction.PropertyVisibilityDigest)
	delete(cloned, interaction.PropertyVisibilityCount)
	return cloned
}

// foldVisibility returns a fold's own visibility, the conjunction of its
// members' expressions, and the touched provenance it was stamped from. The
// union is taken without validating terms: a malformed stored term must
// reach the evaluator, which refuses it, rather than fail the read.
func foldVisibility(fold interaction.Fold) (labels []string, nodeIDs, edgeIDs []shoal.ID) {
	seen := make(map[string]struct{})
	for _, member := range fold.Members {
		for _, label := range member.Visibility {
			if _, duplicate := seen[label]; duplicate {
				continue
			}
			seen[label] = struct{}{}
			labels = append(labels, label)
		}
		nodeIDs = append(nodeIDs, member.RetrievedNodeIDs...)
		nodeIDs = append(nodeIDs, member.CitedNodeIDs...)
		edgeIDs = append(edgeIDs, member.TouchedEdgeIDs...)
	}
	return labels, nodeIDs, edgeIDs
}
