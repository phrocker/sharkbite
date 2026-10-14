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

package interaction

// Fold-style summarization, following the public `fold` semantics of
// github.com/phrocker/sag: a set of recorded interactions collapses into one
// compact vertex, and that vertex can later be unfolded back into exactly what
// it replaced.
//
// Three properties make the Shoal form of this safe rather than merely
// compact:
//
//   - A fold is derived content. Its node kind is in the reserved
//     interaction.* namespace, so every default-exclusion rule that applies to
//     a session applies to it unchanged. A model cannot retrieve a fold and
//     cannot cite one as though it were a source document.
//   - A fold's visibility is the conjunction of everything it folds: every
//     label of every folded session plus every label of every source node
//     those sessions touched. Summarizing can therefore only ever narrow
//     visibility, never widen it. Publishing a genuinely public redacted
//     summary is a separate, explicit, reviewed action, and it is deliberately
//     not implemented here.
//   - Unfolding is lossless with respect to provenance. The retrieved set and
//     the cited set are kept apart at every level, because the visibility
//     conjunction is only sound if it covers everything the model was shown,
//     not only what it went on to cite.
//
// A fold carries no summary text. It carries the digest of a summary held
// out-of-band, because the summary is derived from evidence and an interaction
// record may hold identities, digests, counts, and node IDs only.

import (
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FoldMember is the provenance of exactly one folded session. Retrieved and
// cited are separate fields rather than one touched set: collapsing them is
// precisely the loss that would make the visibility conjunction unsound.
type FoldMember struct {
	SessionID        shoal.ID
	RetrievedNodeIDs []shoal.ID
	CitedNodeIDs     []shoal.ID
	TouchedEdgeIDs   []shoal.ID
	// Visibility is the folded session's own recorded visibility. It is
	// conjoined into the fold so a fold can never be readable by someone who
	// could not read a session it summarizes.
	Visibility []string
}

// Fold is a derived summary vertex over one or more recorded sessions. Its
// identity is content-addressed: the same members and the same summary digest
// always fold to the same vertex.
type Fold struct {
	Members []FoldMember
	// SummaryDigest is the SHA-256 digest, lowercase hex, of the summary text
	// this fold stands for. The text is never carried. An empty digest means
	// the fold is purely structural.
	SummaryDigest string
	// FoldedAt is recorded on the node but is deliberately not part of the
	// fold's identity, so folding the same input twice is idempotent.
	FoldedAt time.Time
}

// FoldSubgraph is a materialized fold: the single fold node, its edges to the
// sessions it folds and to every source node those sessions touched, and the
// conjoined visibility the whole fold requires.
type FoldSubgraph struct {
	ID               shoal.ID
	Nodes            []graph.Node
	Edges            []graph.Edge
	Visibility       []string
	RetrievedNodeIDs []shoal.ID
	CitedNodeIDs     []shoal.ID
	TouchedNodeIDs   []shoal.ID
	TouchedEdgeIDs   []shoal.ID
}

// TouchedSet is the retrieved/cited split recovered from a materialized
// interaction subgraph.
type TouchedSet struct {
	RetrievedNodeIDs []shoal.ID
	CitedNodeIDs     []shoal.ID
}

// TouchedNodes recovers the source nodes an interaction subgraph retrieved and
// the ones it cited, keeping the two apart. Endpoints that are themselves
// nodes of the subgraph are excluded, so only source nodes are returned.
//
// This is the read side of the provenance contract: it lets a fold be derived
// from what a session actually recorded rather than from what a caller claims
// the session touched, which is what stops a fold from understating exposure
// and so widening visibility.
func TouchedNodes(nodes []graph.Node, edges []graph.Edge) TouchedSet {
	internal := make(map[shoal.ID]struct{}, len(nodes))
	for _, node := range nodes {
		internal[node.ID] = struct{}{}
	}
	var retrieved, cited []shoal.ID
	for _, edge := range edges {
		if _, ok := internal[edge.To]; ok {
			continue
		}
		switch edge.Type {
		case EdgeRetrieved:
			retrieved = append(retrieved, edge.To)
		case EdgeCited:
			cited = append(cited, edge.To)
		}
	}
	return TouchedSet{
		RetrievedNodeIDs: dedupeIDs(retrieved),
		CitedNodeIDs:     dedupeIDs(cited),
	}
}

// Canonical returns the fold in its canonical form: members sorted by session
// ID, node IDs within each member sorted and deduplicated, visibility labels
// normalized. Identity is computed over this form, so member ordering and
// duplicate node IDs cannot produce two vertices for one input.
func (f Fold) Canonical() (Fold, error) {
	members := make([]FoldMember, 0, len(f.Members))
	seen := make(map[shoal.ID]struct{}, len(f.Members))
	for _, member := range f.Members {
		if _, duplicate := seen[member.SessionID]; duplicate {
			return Fold{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"fold lists the same session more than once",
			)
		}
		seen[member.SessionID] = struct{}{}
		visibility, err := Conjoin(member.Visibility)
		if err != nil {
			return Fold{}, err
		}
		members = append(members, FoldMember{
			SessionID:        member.SessionID,
			RetrievedNodeIDs: dedupeIDs(member.RetrievedNodeIDs),
			CitedNodeIDs:     dedupeIDs(member.CitedNodeIDs),
			TouchedEdgeIDs:   dedupeIDs(member.TouchedEdgeIDs),
			Visibility:       visibility,
		})
	}
	sort.Slice(members, func(i, j int) bool {
		return shoal.CompareID(members[i].SessionID, members[j].SessionID) < 0
	})
	return Fold{
		Members:       members,
		SummaryDigest: f.SummaryDigest,
		FoldedAt:      f.FoldedAt,
	}, nil
}

// Validate checks the static shape of a fold before it is materialized.
func (f Fold) Validate() error {
	if len(f.Members) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "fold requires at least one session")
	}
	if len(f.Members) > MaxFoldMembers {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "fold exceeds the public member bound")
	}
	if err := validateSummaryDigest(f.SummaryDigest); err != nil {
		return err
	}
	for _, member := range f.Members {
		if err := shoal.ValidateRequiredID(
			"fold session ID", member.SessionID,
		); err != nil {
			return err
		}
		for _, id := range member.RetrievedNodeIDs {
			if err := shoal.ValidateRequiredID(
				"fold retrieved node ID", id,
			); err != nil {
				return err
			}
			if err := requireSourceNodeID(id); err != nil {
				return err
			}
		}
		for _, id := range member.CitedNodeIDs {
			if err := shoal.ValidateRequiredID("fold cited node ID", id); err != nil {
				return err
			}
			if err := requireSourceNodeID(id); err != nil {
				return err
			}
		}
		for _, id := range member.TouchedEdgeIDs {
			if err := shoal.ValidateRequiredID("fold touched edge ID", id); err != nil {
				return err
			}
			if IsInteractionID(id) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"fold cannot treat an interaction edge as source evidence",
				)
			}
		}
	}
	return nil
}

// ID is the content-addressed identity of the fold: a digest over its
// canonical members and its summary digest. Neither the fold time nor the
// derived visibility participates, so the same input always folds to the same
// vertex while a corpus whose labels changed still fails closed at write time
// rather than minting a second vertex.
func (f Fold) ID() (shoal.ID, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	canonical, err := f.Canonical()
	if err != nil {
		return "", err
	}
	hasEdgeProvenance := false
	for _, member := range canonical.Members {
		if len(member.TouchedEdgeIDs) > 0 {
			hasEdgeProvenance = true
			break
		}
	}
	if hasEdgeProvenance {
		parts := []string{
			"edge-provenance-v1",
			"members", strconv.Itoa(len(canonical.Members)),
		}
		for _, member := range canonical.Members {
			parts = append(parts, "session", string(member.SessionID))
			parts = append(
				parts, "retrieved", strconv.Itoa(len(member.RetrievedNodeIDs)))
			for _, id := range member.RetrievedNodeIDs {
				parts = append(parts, string(id))
			}
			parts = append(parts, "cited", strconv.Itoa(len(member.CitedNodeIDs)))
			for _, id := range member.CitedNodeIDs {
				parts = append(parts, string(id))
			}
			parts = append(
				parts, "edges", strconv.Itoa(len(member.TouchedEdgeIDs)))
			for _, id := range member.TouchedEdgeIDs {
				parts = append(parts, string(id))
			}
		}
		parts = append(parts, "summary", canonical.SummaryDigest)
		return DerivedID("fold", parts...), nil
	}

	// Preserve the original identity encoding for folds without typed edge
	// provenance so durable legacy records remain addressable.
	parts := make([]string, 0, 4*len(canonical.Members)+2)
	for _, member := range canonical.Members {
		parts = append(parts, "session", string(member.SessionID))
		parts = append(parts, "retrieved")
		for _, id := range member.RetrievedNodeIDs {
			parts = append(parts, string(id))
		}
		parts = append(parts, "cited")
		for _, id := range member.CitedNodeIDs {
			parts = append(parts, string(id))
		}
		if len(member.TouchedEdgeIDs) > 0 {
			parts = append(parts, "edges")
			for _, id := range member.TouchedEdgeIDs {
				parts = append(parts, string(id))
			}
		}
	}
	parts = append(parts, "summary", canonical.SummaryDigest)
	return DerivedID("fold", parts...), nil
}

// Subgraph materializes a compatibility fold that has no retained source-edge
// provenance. Edge-bearing folds require SubgraphWithEvidence so edge-local
// visibility cannot be omitted. resolve supplies the visibility labels of
// every source node the fold covers; if it fails for any node the whole fold
// fails rather than being written with understated visibility.
//
// The resulting visibility is the conjunction of every folded session's own
// visibility and every touched source node's labels. Folding therefore never
// widens visibility: the fold requires at least everything each of its parts
// required.
func (f Fold) Subgraph(resolve VisibilityResolver) (FoldSubgraph, error) {
	return f.SubgraphWithEvidence(resolve, nil)
}

// SubgraphWithEvidence additionally resolves every exact source edge retained
// by the folded members, so edge-local labels can never be omitted.
func (f Fold) SubgraphWithEvidence(
	resolveNode VisibilityResolver,
	resolveEdge VisibilityResolver,
) (FoldSubgraph, error) {
	if err := f.Validate(); err != nil {
		return FoldSubgraph{}, err
	}
	if resolveNode == nil {
		return FoldSubgraph{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fold visibility resolver is required")
	}
	if f.FoldedAt.IsZero() {
		return FoldSubgraph{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fold time is required")
	}
	canonical, err := f.Canonical()
	if err != nil {
		return FoldSubgraph{}, err
	}
	foldID, err := canonical.ID()
	if err != nil {
		return FoldSubgraph{}, err
	}

	var retrieved, cited, touchedEdges []shoal.ID
	sets := make([][]string, 0, len(canonical.Members)+1)
	for _, member := range canonical.Members {
		retrieved = append(retrieved, member.RetrievedNodeIDs...)
		cited = append(cited, member.CitedNodeIDs...)
		touchedEdges = append(touchedEdges, member.TouchedEdgeIDs...)
		sets = append(sets, member.Visibility)
	}
	retrieved = dedupeIDs(retrieved)
	cited = dedupeIDs(cited)
	touched := dedupeIDs(append(append([]shoal.ID(nil), retrieved...), cited...))
	touchedEdges = dedupeIDs(touchedEdges)
	for _, id := range touched {
		labels, err := resolveNode(id)
		if err != nil {
			return FoldSubgraph{}, err
		}
		normalized, err := Conjoin(labels)
		if err != nil {
			return FoldSubgraph{}, err
		}
		sets = append(sets, normalized)
	}
	if len(touchedEdges) > 0 && resolveEdge == nil {
		return FoldSubgraph{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fold edge visibility resolver is required")
	}
	for _, id := range touchedEdges {
		labels, err := resolveEdge(id)
		if err != nil {
			return FoldSubgraph{}, err
		}
		normalized, err := Conjoin(labels)
		if err != nil {
			return FoldSubgraph{}, err
		}
		sets = append(sets, normalized)
	}
	visibility, err := Conjoin(sets...)
	if err != nil {
		return FoldSubgraph{}, err
	}

	node := graph.Node{
		ID:     foldID,
		Kind:   KindFold,
		Labels: []string{LabelInteraction},
		Properties: shoal.Metadata{
			PropertyFoldedAt:    f.FoldedAt.UTC().Format(time.RFC3339Nano),
			PropertyFoldedCount: strconv.Itoa(len(canonical.Members)),
			PropertyRetrieved:   strconv.Itoa(len(retrieved)),
			PropertyCited:       strconv.Itoa(len(cited)),
		},
	}
	setIfPresent(node.Properties, PropertySummaryDigest, canonical.SummaryDigest)
	setVisibility(node.Properties, visibility)

	edges := make([]graph.Edge, 0, len(canonical.Members)+len(touched))
	for _, member := range canonical.Members {
		edges = append(edges, provenanceEdge(EdgeFolds, foldID, member.SessionID))
	}
	// Retrieved and cited stay distinct on the fold itself, so the fold's own
	// edges answer "what was this summary allowed to see" separately from
	// "what did it actually rest on".
	for _, id := range retrieved {
		edges = append(edges, provenanceEdge(EdgeRetrieved, foldID, id))
	}
	for _, id := range cited {
		edges = append(edges, provenanceEdge(EdgeCited, foldID, id))
	}
	sortEdges(edges)
	if err := node.Validate(); err != nil {
		return FoldSubgraph{}, err
	}
	for _, edge := range edges {
		if err := edge.Validate(); err != nil {
			return FoldSubgraph{}, err
		}
	}
	return FoldSubgraph{
		ID:               foldID,
		Nodes:            []graph.Node{node},
		Edges:            edges,
		Visibility:       visibility,
		RetrievedNodeIDs: retrieved,
		CitedNodeIDs:     cited,
		TouchedNodeIDs:   touched,
		TouchedEdgeIDs:   touchedEdges,
	}, nil
}

// IsInteractionID reports whether an identity was minted inside the reserved
// interaction namespace. Identity carries the namespace so a caller cannot
// smuggle a derived node into a position that expects a source node without
// the graph being consulted.
func IsInteractionID(id shoal.ID) bool {
	return len(string(id)) > len(KindPrefix) &&
		string(id)[:len(KindPrefix)] == KindPrefix
}

// requireSourceNodeID refuses an identity that is self-evidently an
// interaction node where a source node is expected.
//
// This is a weaker check than the one Explorer's visibility resolver applies,
// and it is not a substitute for it. It can only recognize IDs that carry the
// reserved prefix; shoal.ID is otherwise opaque, and a caller-supplied session
// ID such as "session-one" is indistinguishable from a source node ID here.
// The authoritative guard is kind-based and lives in the resolver, which looks
// the node up in the corpus and rejects any interaction kind. This restatement
// exists so that a caller assembling a fold without that resolver still fails
// on the obvious case rather than silently succeeding.
func requireSourceNodeID(id shoal.ID) error {
	if IsInteractionID(id) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fold cannot treat an interaction node as source evidence",
		)
	}
	return nil
}

// validateSummaryDigest requires a summary reference to be a SHA-256 digest in
// lowercase hex, never free text. The shape is enforced rather than trusted so
// a caller cannot use the field to smuggle prompt or answer text, or a
// model-chosen correlation string, into a node payload.
func validateSummaryDigest(digest string) error {
	return validateDigest("fold summary digest", digest, true)
}
