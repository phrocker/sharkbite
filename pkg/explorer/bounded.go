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

package explorer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Snapshot returns the cached logical corpus frontier.
func (e *Explorer) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return Snapshot{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return Snapshot{}, err
	}
	if err := e.registerSnapshotLocked(e.snapshot); err != nil {
		return Snapshot{}, err
	}
	return e.snapshot, nil
}

// ValidateSnapshot verifies that a snapshot pin names a genuine corpus
// frontier observed from this Explorer instance. Historical frontiers remain
// valid after unrelated content publications during the process lifetime.
func (e *Explorer) ValidateSnapshot(
	ctx context.Context, id shoal.ID, asOf time.Time, nodeIDs []shoal.ID,
) error {
	return e.ValidateEvidenceSnapshot(ctx, id, asOf, nodeIDs, nil, nil)
}

// ValidateEvidenceSnapshot verifies exact source nodes, edges, and assertions against the
// canonical state captured by a trusted snapshot. Reusing an ID after changing
// content, revision, labels, endpoints, or edge properties fails closed.
func (e *Explorer) ValidateEvidenceSnapshot(
	ctx context.Context,
	id shoal.ID,
	asOf time.Time,
	nodeIDs []shoal.ID,
	edgeIDs []shoal.ID,
	references []interaction.EvidenceReference,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("snapshot ID", id); err != nil {
		return err
	}
	if asOf.IsZero() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "snapshot time is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return err
	}
	return e.validateEvidenceSnapshotLocked(
		id, asOf, nodeIDs, edgeIDs, references)
}

func (e *Explorer) validateEvidenceSnapshotLocked(
	id shoal.ID,
	asOf time.Time,
	nodeIDs []shoal.ID,
	edgeIDs []shoal.ID,
	references []interaction.EvidenceReference,
) error {
	record, ok := e.snapshotHistory[string(id)]
	if !ok || !record.AsOf.Equal(asOf.UTC()) {
		return shoal.NewError(
			shoal.ErrorConflict, "snapshot pin is not a trusted corpus frontier")
	}
	nodeState, edgeState, assertionState, err := e.snapshotStateLocked(id)
	if err != nil {
		return err
	}
	for _, nodeID := range nodeIDs {
		node, current := e.graphNodes[nodeID]
		expected, present := nodeState[nodeID]
		actual, digestErr := snapshotObjectDigest(node)
		if !current || interaction.IsInteractionKind(node.Kind) || !present ||
			expected == "" || digestErr != nil || actual != expected {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction source does not match the pinned snapshot",
			)
		}
	}
	for _, edgeID := range edgeIDs {
		edge, current := e.graphEdges[edgeID]
		expected, present := edgeState[edgeID]
		actual, digestErr := snapshotObjectDigest(edge)
		if !current || interaction.IsInteractionID(edge.ID) || !present ||
			expected == "" || digestErr != nil || actual != expected {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction source edge does not match the pinned snapshot",
			)
		}
	}
	for _, reference := range references {
		if err := e.validateEvidenceReferenceLocked(reference); err != nil {
			return err
		}
		for _, assertion := range reference.Assertions {
			expected, present := assertionState[assertion.EdgeID]
			current, exists := e.graphAssertions[assertion.EdgeID]
			actual, digestErr := snapshotObjectDigest(current)
			if !present || !exists || expected == "" ||
				digestErr != nil || actual != expected {
				return shoal.NewError(
					shoal.ErrorConflict,
					"interaction assertion does not match the pinned snapshot",
				)
			}
		}
	}
	return nil
}

func (e *Explorer) validateEvidenceReferenceLocked(
	reference interaction.EvidenceReference,
) error {
	canonical, err := reference.Canonical()
	if err != nil {
		return err
	}
	switch canonical.Kind {
	case interaction.EvidenceDocument:
		revisions := e.documents[canonical.Citation.DocumentID]
		record := revisions[canonical.Citation.RevisionID]
		if record == nil {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction citation revision is unavailable",
			)
		}
		quote, err := document.ResolveCitationQuote(
			record.Source.Content,
			record.Document,
			record.Revision,
			record.Sections,
			record.Spans,
			canonical.Citation,
		)
		if err != nil {
			return shoal.WrapError(
				shoal.ErrorConflict,
				"interaction citation is not authoritative",
				err,
			)
		}
		anchor, err := inference.NewDocumentAnchor(
			canonical.Citation, quote)
		if err != nil || anchor.ID() != canonical.AnchorID {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction citation anchor identity is not authoritative",
			)
		}
		sectionID, spanID, err := resolveCitationSourceRoles(
			record, canonical.Citation, quote)
		if err != nil {
			return shoal.WrapError(
				shoal.ErrorConflict,
				"interaction citation source roles are not authoritative",
				err,
			)
		}
		expectedNodeIDs := []shoal.ID{
			canonical.Citation.DocumentID, sectionID, spanID,
		}
		sort.Slice(expectedNodeIDs, func(i, j int) bool {
			return shoal.CompareID(expectedNodeIDs[i], expectedNodeIDs[j]) < 0
		})
		if !equalIDs(canonical.NodeIDs, expectedNodeIDs) {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction citation source roles are not authoritative",
			)
		}
	case interaction.EvidenceGraph:
		path := graph.Path{
			Nodes: make([]graph.Node, len(canonical.NodeIDs)),
			Edges: make([]graph.Edge, len(canonical.EdgeIDs)),
		}
		authoritativeAssertions := make(map[interaction.AssertionReference]struct{})
		for index, nodeID := range canonical.NodeIDs {
			node, ok := e.graphNodes[nodeID]
			if !ok || interaction.IsInteractionKind(node.Kind) {
				return shoal.NewError(
					shoal.ErrorConflict,
					"interaction graph evidence node is unavailable",
				)
			}
			path.Nodes[index] = cloneNode(node)
		}
		for index, edgeID := range canonical.EdgeIDs {
			edge, ok := e.graphEdges[edgeID]
			if !ok || interaction.IsInteractionID(edge.ID) {
				return shoal.NewError(
					shoal.ErrorConflict,
					"interaction graph evidence edge is unavailable",
				)
			}
			path.Edges[index] = cloneEdge(edge)
			if assertion, ok := e.graphAssertions[edgeID]; ok {
				authoritativeAssertions[interaction.AssertionReference{
					AssertionID: assertion.ID(), EdgeID: edgeID,
					Origin: assertion.Origin(),
				}] = struct{}{}
			}
		}
		for _, assertion := range canonical.Assertions {
			if _, ok := authoritativeAssertions[assertion]; !ok {
				return shoal.NewError(
					shoal.ErrorConflict,
					"interaction graph assertion is not authoritative",
				)
			}
			if err := e.validateAssertionReferenceLocked(
				assertion); err != nil {
				return err
			}
			delete(authoritativeAssertions, assertion)
		}
		if len(authoritativeAssertions) != 0 {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction graph evidence omits authoritative assertions",
			)
		}
		anchor, err := inference.NewGraphAnchorWithAssertions(
			path, canonical.Assertions)
		if err != nil || anchor.ID() != canonical.AnchorID {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction graph anchor identity is not authoritative",
			)
		}
	default:
		return shoal.NewError(
			shoal.ErrorConflict,
			"interaction evidence kind is not authoritative",
		)
	}
	return nil
}

func resolveCitationSourceRoles(
	record *persistedDocument,
	citation document.Citation,
	quote string,
) (shoal.ID, shoal.ID, error) {
	if citation.SpanID != "" {
		for _, span := range record.Spans {
			if span.ID == citation.SpanID {
				return span.SectionID, span.ID, nil
			}
		}
		return "", "", shoal.NewError(
			shoal.ErrorNotFound, "cited span was not found")
	}

	var matched document.Span
	for _, span := range record.Spans {
		if span.SectionID != citation.SectionID ||
			span.Range.Start.Offset > citation.Range.Start.Offset ||
			span.Range.End.Offset < citation.Range.End.Offset {
			continue
		}
		start := citation.Range.Start.Offset - span.Range.Start.Offset
		end := citation.Range.End.Offset - span.Range.Start.Offset
		if start < 0 || end < start || end > int64(len(span.Text)) ||
			span.Text[start:end] != quote {
			continue
		}
		if matched.ID != "" {
			return "", "", shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation resolves to more than one source span",
			)
		}
		matched = span
	}
	if matched.ID == "" {
		return "", "", shoal.NewError(
			shoal.ErrorNotFound,
			"citation does not resolve to an exact source span",
		)
	}
	return matched.SectionID, matched.ID, nil
}

func (e *Explorer) validateAssertionReferenceLocked(
	reference interaction.AssertionReference,
) error {
	if err := shoal.ValidateRequiredID(
		"interaction assertion ID", reference.AssertionID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"interaction assertion edge ID", reference.EdgeID); err != nil {
		return err
	}
	assertion, found := e.graphAssertions[reference.EdgeID]
	if !found || assertion.ID() != reference.AssertionID ||
		assertion.Origin() != reference.Origin {
		return shoal.NewError(
			shoal.ErrorConflict,
			"interaction assertion does not match current authoritative data",
		)
	}
	edge, ok := e.graphEdges[reference.EdgeID]
	if !ok {
		return shoal.NewError(
			shoal.ErrorConflict,
			"interaction assertion edge is no longer present",
		)
	}
	const (
		assertionEdgeIDProperty = "shoal.graph.edge_id"
		assertionIDProperty     = "ontology.assertion.id"
	)
	target, hasTarget := assertion.Object().ReferenceValue()
	matches := shoal.ID(
		assertion.Metadata()[assertionEdgeIDProperty],
	) == reference.EdgeID ||
		(hasTarget && assertion.ID() == reference.EdgeID &&
			edge.From == assertion.Subject() &&
			edge.To == target &&
			edge.Type == string(assertion.Predicate()) &&
			math.Float64bits(float64(edge.Weight)) ==
				math.Float64bits(float64(assertion.Confidence()))) ||
		shoal.ID(edge.Properties[assertionIDProperty]) == assertion.ID()
	if !matches {
		return shoal.NewError(
			shoal.ErrorConflict,
			"interaction assertion is not authoritative for its source edge",
		)
	}
	return nil
}

func (e *Explorer) registerSnapshotLocked(snapshot Snapshot) error {
	id := shoal.ID(snapshot.ID)
	currentNodes, currentEdges, currentAssertions, err := e.currentSourceStateLocked()
	if err != nil {
		return err
	}
	nodeStates, removedNodes := snapshotObjectDelta(
		e.latestSnapshotNodeDigests, currentNodes)
	edgeStates, removedEdges := snapshotObjectDelta(
		e.latestSnapshotEdgeDigests, currentEdges)
	assertionStates, removedAssertions := snapshotObjectDelta(
		e.latestSnapshotAssertionDigests, currentAssertions)
	record := persistedSnapshot{
		ID: id, AsOf: snapshot.AsOf.UTC(), ParentID: e.latestSnapshotID,
		AddedNodeIDs:            snapshotStateIDs(nodeStates),
		RemovedNodeIDs:          removedNodes,
		NodeStates:              nodeStates,
		RemovedEdgeIDs:          removedEdges,
		EdgeStates:              edgeStates,
		AssertionStates:         assertionStates,
		RemovedAssertionEdgeIDs: removedAssertions,
	}
	if existing, ok := e.snapshotHistory[snapshot.ID]; ok {
		nodes, edges, assertions, err := e.snapshotStateLocked(id)
		if err == nil && existing.AsOf.Equal(record.AsOf) &&
			snapshotObjectMapsEqual(nodes, currentNodes) &&
			snapshotObjectMapsEqual(edges, currentEdges) &&
			snapshotObjectMapsEqual(assertions, currentAssertions) {
			e.latestSnapshotID = id
			e.setLatestSnapshotState(currentNodes, currentEdges, currentAssertions)
			return nil
		}
		return shoal.NewError(
			shoal.ErrorInternal,
			"snapshot ID has conflicting observation times",
		)
	}
	if e.readOnly {
		e.snapshotHistory[snapshot.ID] = record
		e.latestSnapshotID = id
		e.setLatestSnapshotState(currentNodes, currentEdges, currentAssertions)
		return nil
	}
	accepted, err := e.conditionalInteractionRecord(
		snapshotRecordRow(id),
		embeddedRecordSnapshot,
		record,
		recordCQV2,
		false,
	)
	if err != nil {
		return err
	}
	if !accepted {
		var winner persistedSnapshot
		found, err := e.lookupEmbeddedRecord(
			snapshotRecordRow(id), embeddedRecordSnapshot, &winner)
		if err != nil {
			return err
		}
		winner.AsOf = winner.AsOf.UTC()
		if !found || winner.ID != id || !winner.AsOf.Equal(record.AsOf) {
			return shoal.NewError(
				shoal.ErrorConflict,
				"snapshot ID is already registered with different content",
			)
		}
		e.snapshotHistory[snapshot.ID] = winner
		nodes, edges, assertions, err := e.snapshotStateLocked(id)
		if err != nil ||
			!snapshotObjectMapsEqual(nodes, currentNodes) ||
			!snapshotObjectMapsEqual(edges, currentEdges) ||
			!snapshotObjectMapsEqual(assertions, currentAssertions) {
			return shoal.NewError(
				shoal.ErrorConflict,
				"snapshot ID is already registered with different membership",
			)
		}
		record = winner
	}
	e.snapshotHistory[snapshot.ID] = record
	e.latestSnapshotID = id
	e.setLatestSnapshotState(currentNodes, currentEdges, currentAssertions)
	return nil
}

func snapshotObjectMapsEqual(
	left, right map[shoal.ID]string,
) bool {
	if len(left) != len(right) {
		return false
	}
	for id, digest := range left {
		if right[id] != digest {
			return false
		}
	}
	return true
}

func (e *Explorer) restoreLatestSnapshotLocked() error {
	var latest persistedSnapshot
	found := false
	for _, record := range e.snapshotHistory {
		if !found || record.AsOf.After(latest.AsOf) ||
			(record.AsOf.Equal(latest.AsOf) &&
				shoal.CompareID(record.ID, latest.ID) > 0) {
			latest = record
			found = true
		}
	}
	if !found {
		return nil
	}
	nodes, edges, assertions, err := e.snapshotStateLocked(latest.ID)
	if err != nil {
		return err
	}
	e.latestSnapshotID = latest.ID
	e.setLatestSnapshotState(nodes, edges, assertions)
	return nil
}

func (e *Explorer) currentSourceStateLocked() (
	map[shoal.ID]string,
	map[shoal.ID]string,
	map[shoal.ID]string,
	error,
) {
	nodes := make(map[shoal.ID]string, len(e.graphNodes))
	for id, node := range e.graphNodes {
		if !interaction.IsInteractionKind(node.Kind) {
			digest, err := snapshotObjectDigest(node)
			if err != nil {
				return nil, nil, nil, err
			}
			nodes[id] = digest
		}
	}
	edges := make(map[shoal.ID]string, len(e.graphEdges))
	assertions := make(map[shoal.ID]string, len(e.graphAssertions))
	for id, edge := range e.graphEdges {
		from, fromOK := e.graphNodes[edge.From]
		to, toOK := e.graphNodes[edge.To]
		if interaction.IsInteractionID(id) || !fromOK || !toOK ||
			interaction.IsInteractionKind(from.Kind) ||
			interaction.IsInteractionKind(to.Kind) {
			continue
		}
		digest, err := snapshotObjectDigest(edge)
		if err != nil {
			return nil, nil, nil, err
		}
		edges[id] = digest
		if assertion, ok := e.graphAssertions[id]; ok {
			digest, err := snapshotObjectDigest(assertion)
			if err != nil {
				return nil, nil, nil, err
			}
			assertions[id] = digest
		}
	}
	return nodes, edges, assertions, nil
}

func snapshotObjectDelta(
	previous, current map[shoal.ID]string,
) ([]persistedSnapshotObject, []shoal.ID) {
	var states []persistedSnapshotObject
	var removed []shoal.ID
	for id, digest := range current {
		if previous[id] != digest {
			states = append(states, persistedSnapshotObject{
				ID: id, Digest: digest,
			})
		}
	}
	for id := range previous {
		if _, ok := current[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Slice(states, func(i, j int) bool {
		return shoal.CompareID(states[i].ID, states[j].ID) < 0
	})
	sort.Slice(removed, func(i, j int) bool {
		return shoal.CompareID(removed[i], removed[j]) < 0
	})
	return states, removed
}

func snapshotStateIDs(states []persistedSnapshotObject) []shoal.ID {
	ids := make([]shoal.ID, len(states))
	for index, state := range states {
		ids[index] = state.ID
	}
	return ids
}

func (e *Explorer) snapshotMembershipLocked(
	id shoal.ID,
) (map[shoal.ID]struct{}, error) {
	nodes, _, _, err := e.snapshotStateLocked(id)
	if err != nil {
		return nil, err
	}
	membership := make(map[shoal.ID]struct{}, len(nodes))
	for nodeID := range nodes {
		membership[nodeID] = struct{}{}
	}
	return membership, nil
}

func (e *Explorer) snapshotStateLocked(
	id shoal.ID,
) (map[shoal.ID]string, map[shoal.ID]string, map[shoal.ID]string, error) {
	var chain []persistedSnapshot
	seen := make(map[shoal.ID]struct{})
	for id != "" {
		if _, duplicate := seen[id]; duplicate {
			return nil, nil, nil, shoal.NewError(
				shoal.ErrorInternal, "snapshot history contains a cycle")
		}
		seen[id] = struct{}{}
		record, ok := e.snapshotHistory[string(id)]
		if !ok {
			return nil, nil, nil, shoal.NewError(
				shoal.ErrorInternal, "snapshot history is incomplete")
		}
		chain = append(chain, record)
		id = record.ParentID
	}
	nodes := make(map[shoal.ID]string)
	edges := make(map[shoal.ID]string)
	assertions := make(map[shoal.ID]string)
	for index := len(chain) - 1; index >= 0; index-- {
		for _, nodeID := range chain[index].RemovedNodeIDs {
			delete(nodes, nodeID)
		}
		if len(chain[index].NodeStates) == 0 {
			for _, nodeID := range chain[index].AddedNodeIDs {
				nodes[nodeID] = ""
			}
		} else {
			for _, state := range chain[index].NodeStates {
				nodes[state.ID] = state.Digest
			}
		}
		for _, edgeID := range chain[index].RemovedEdgeIDs {
			delete(edges, edgeID)
		}
		for _, state := range chain[index].EdgeStates {
			edges[state.ID] = state.Digest
		}
		for _, edgeID := range chain[index].RemovedAssertionEdgeIDs {
			delete(assertions, edgeID)
		}
		for _, state := range chain[index].AssertionStates {
			assertions[state.ID] = state.Digest
		}
	}
	return nodes, edges, assertions, nil
}

func (e *Explorer) setLatestSnapshotState(
	nodes, edges, assertions map[shoal.ID]string,
) {
	e.latestSnapshotNodeDigests = nodes
	e.latestSnapshotEdgeDigests = edges
	e.latestSnapshotAssertionDigests = assertions
}

// snapshotObjectDigest binds a graph object to the frontier that observed it.
// IDs and metadata are opaque byte strings, so the digest hashes raw
// length-prefixed bytes: a JSON encoding would silently fold every distinct
// invalid UTF-8 byte onto U+FFFD and let a mutated node or edge endpoint keep
// the digest it had in the pinned snapshot.
func snapshotObjectDigest(value any) (string, error) {
	hash := sha256.New()
	switch object := value.(type) {
	case graph.Node:
		writeSnapshotString(hash, "node")
		writeSnapshotString(hash, string(object.ID))
		writeSnapshotString(hash, object.Kind)
		labels := append([]string(nil), object.Labels...)
		sort.Strings(labels)
		writeSnapshotCount(hash, len(labels))
		for _, label := range labels {
			writeSnapshotString(hash, label)
		}
		writeSnapshotMetadata(hash, object.Properties)
	case graph.Edge:
		writeSnapshotString(hash, "edge")
		writeSnapshotString(hash, string(object.ID))
		writeSnapshotString(hash, string(object.From))
		writeSnapshotString(hash, string(object.To))
		writeSnapshotString(hash, object.Type)
		var weight [8]byte
		binary.BigEndian.PutUint64(
			weight[:], math.Float64bits(float64(object.Weight)))
		_, _ = hash.Write(weight[:])
		writeSnapshotMetadata(hash, object.Properties)
	case ontology.Assertion:
		writeSnapshotAssertion(hash, object)
	default:
		return "", shoal.NewError(
			shoal.ErrorInternal, "unknown snapshot object")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeSnapshotAssertion(writer snapshotWriter, assertion ontology.Assertion) {
	// The canonical ID binds typed values, origin, confidence, provenance,
	// ontology, and evidence identities. Annotation metadata is not part of
	// every identity, so bind it explicitly as well.
	writeSnapshotString(writer, "assertion")
	writeSnapshotString(writer, string(assertion.ID()))
	writeSnapshotString(writer, string(assertion.Origin()))
	writeSnapshotMetadata(writer, assertion.Metadata())
	writeSnapshotMetadata(writer, assertion.Provenance().Metadata())
	evidence := assertion.Evidence()
	writeSnapshotCount(writer, len(evidence))
	for _, item := range evidence {
		writeSnapshotString(writer, string(item.ID()))
		writeSnapshotMetadata(writer, item.Metadata())
	}
}

// BoundedNeighborhood expands the cached adjacency index without scanning or
// materializing an unbounded one-hop neighborhood.
func (e *Explorer) BoundedNeighborhood(
	ctx context.Context, request BoundedNeighborhoodRequest,
) (BoundedNeighborhood, error) {
	if err := contextError(ctx); err != nil {
		return BoundedNeighborhood{}, err
	}
	if request.Depth == 0 || request.Fanout == 0 || request.MaxNodes == 0 {
		return BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "bounded graph limits must be nonzero")
	}
	if request.Direction == "" {
		request.Direction = GraphDirectionBoth
	}
	switch request.Direction {
	case GraphDirectionBoth, GraphDirectionOutgoing, GraphDirectionIncoming:
	default:
		return BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown graph direction")
	}
	normalized, err := (NeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: request.Depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return BoundedNeighborhood{}, err
	}
	if uint32(len(normalized.NodeIDs)) > request.MaxNodes {
		return BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph seeds exceed max_nodes")
	}
	typeFilter := make(map[string]struct{}, len(normalized.EdgeTypes))
	for _, edgeType := range normalized.EdgeTypes {
		typeFilter[edgeType] = struct{}{}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return BoundedNeighborhood{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return BoundedNeighborhood{}, err
	}
	nodes := make(map[shoal.ID]graph.Node)
	seen := make(map[shoal.ID]struct{})
	frontier := make([]shoal.ID, 0, len(normalized.NodeIDs))
	for _, id := range normalized.NodeIDs {
		node, ok := e.graphNodes[id]
		if !ok {
			return BoundedNeighborhood{}, shoal.NewError(
				shoal.ErrorNotFound, "graph node not found")
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		nodes[id] = cloneNode(node)
		frontier = append(frontier, id)
	}
	edges := make(map[shoal.ID]graph.Edge)
	explicit := idSet(normalized.NodeIDs)
	interactionSeeds := make([]shoal.ID, 0)
	for _, id := range frontier {
		if interaction.IsInteractionKind(e.graphNodes[id].Kind) {
			interactionSeeds = append(interactionSeeds, id)
		}
	}
	truncated := false
	cursorEligible := len(normalized.NodeIDs) == 1 && normalized.Depth == 1
	nextAfter := request.AfterEdgeID
	continuation := false
	stopExpansion := false
	scannedEdges := uint32(0)
	for level := uint32(0); level < normalized.Depth && len(frontier) > 0; level++ {
		next := make([]shoal.ID, 0)
		for _, seed := range frontier {
			after := shoal.ID("")
			if level == 0 && len(normalized.NodeIDs) == 1 {
				after = request.AfterEdgeID
			}
			edgeIDs, limited := e.boundedEdgeIDs(
				seed, request.Direction, request.Fanout, after, interactionSeeds)
			if limited {
				truncated = true
				if cursorEligible {
					continuation = true
				}
			}
			for _, edgeID := range edgeIDs {
				if scannedEdges < math.MaxUint32 {
					scannedEdges++
				}
				edge := e.graphEdges[edgeID]
				if len(typeFilter) > 0 {
					if _, ok := typeFilter[edge.Type]; !ok {
						if cursorEligible {
							nextAfter = edgeID
						}
						continue
					}
				}
				if excludedInteractionEdge(e.graphNodes, explicit, edge) {
					if cursorEligible {
						nextAfter = edgeID
					}
					continue
				}
				other := edge.To
				if other == seed {
					other = edge.From
				}
				if _, exists := seen[other]; !exists {
					if uint32(len(seen)) >= request.MaxNodes {
						truncated = true
						if cursorEligible && nextAfter != request.AfterEdgeID {
							continuation = true
						}
						stopExpansion = true
						break
					}
					seen[other] = struct{}{}
					nodes[other] = cloneNode(e.graphNodes[other])
					next = append(next, other)
				}
				edges[edge.ID] = cloneEdge(edge)
				if cursorEligible {
					nextAfter = edgeID
				}
			}
			if stopExpansion {
				break
			}
		}
		if stopExpansion {
			break
		}
		frontier = next
	}
	result := Neighborhood{
		Nodes:      make([]graph.Node, 0, len(nodes)),
		Edges:      make([]graph.Edge, 0, len(edges)),
		Assertions: make([]ontology.Assertion, 0, len(edges)),
	}
	for _, node := range nodes {
		result.Nodes = append(result.Nodes, node)
	}
	for _, edge := range edges {
		result.Edges = append(result.Edges, edge)
	}
	result.Assertions = e.assertionsForEdgesLocked(edges)
	sort.Slice(result.Nodes, func(i, j int) bool {
		return shoal.CompareID(result.Nodes[i].ID, result.Nodes[j].ID) < 0
	})
	sort.Slice(result.Edges, func(i, j int) bool {
		return shoal.CompareID(result.Edges[i].ID, result.Edges[j].ID) < 0
	})
	if cursorEligible && nextAfter == request.AfterEdgeID {
		continuation = false
	}
	if !continuation {
		nextAfter = ""
	}
	bounded := BoundedNeighborhood{
		Neighborhood: result, Truncated: truncated,
		NextAfterEdgeID: nextAfter, Continuation: continuation,
		ScannedEdges: scannedEdges, ScannedEdgesKnown: true,
	}
	if cursorEligible {
		bounded.NodeAdjacency = e.nodeAdjacencySize(
			normalized.NodeIDs[0], request.Direction)
		bounded.NodeAdjacencyKnown = true
	}
	return bounded, nil
}

// nodeAdjacencySize is the number of adjacency entries a bounded scan of
// nodeID in this direction would examine from the start, in O(1): the
// lengths of the same indexes boundedEdgeIDs merges for a lone seed. A self
// edge counts once per direction it is indexed under.
func (e *Explorer) nodeAdjacencySize(nodeID shoal.ID, direction GraphDirection) uint64 {
	seedIsInteraction := interaction.IsInteractionKind(e.graphNodes[nodeID].Kind)
	size := uint64(0)
	if direction != GraphDirectionIncoming {
		size += uint64(len(e.outgoing[nodeID]))
		if seedIsInteraction {
			size += uint64(len(e.interactionOutgoing[nodeID]))
		}
	}
	if direction != GraphDirectionOutgoing {
		size += uint64(len(e.incoming[nodeID]))
		if seedIsInteraction {
			size += uint64(len(e.interactionIncoming[nodeID]))
		}
	}
	return size
}

// boundedEdgeIDs returns at most fanout adjacency entries of nodeID after the
// cursor, in edge ID order, and whether more remain. Interaction adjacency is
// excluded before fanout is charged (#698): a source node's scan examines
// only source edges, plus the edges joining it to an explicit interaction
// seed (the only interaction edges the bounded API serves from it). An
// interaction node examines its own adjacency. So neither the fanout a
// reader asks for nor the scan count can be consumed by interactions a
// caller did not seed.
func (e *Explorer) boundedEdgeIDs(
	nodeID shoal.ID,
	direction GraphDirection,
	fanout uint32,
	after shoal.ID,
	interactionSeeds []shoal.ID,
) ([]shoal.ID, bool) {
	lists := make([][]shoal.ID, 0, 4)
	seedIsInteraction := interaction.IsInteractionKind(e.graphNodes[nodeID].Kind)
	if direction != GraphDirectionIncoming {
		lists = append(lists, afterEdge(e.outgoing[nodeID], after))
		if seedIsInteraction {
			lists = append(lists, afterEdge(e.interactionOutgoing[nodeID], after))
		} else if extra := e.seededInteractionEdgeIDs(
			nodeID, interactionSeeds, true); len(extra) > 0 {
			lists = append(lists, afterEdge(extra, after))
		}
	}
	if direction != GraphDirectionOutgoing {
		lists = append(lists, afterEdge(e.incoming[nodeID], after))
		if seedIsInteraction {
			lists = append(lists, afterEdge(e.interactionIncoming[nodeID], after))
		} else if extra := e.seededInteractionEdgeIDs(
			nodeID, interactionSeeds, false); len(extra) > 0 {
			lists = append(lists, afterEdge(extra, after))
		}
	}
	return mergeEdgeIDLists(lists, fanout)
}

// seededInteractionEdgeIDs returns, sorted, the edges joining a source node
// to an explicit interaction seed: outgoing from nodeID when outgoing is
// true, incoming otherwise. Only the seeds' own (bounded) adjacency is read.
func (e *Explorer) seededInteractionEdgeIDs(
	nodeID shoal.ID, interactionSeeds []shoal.ID, outgoing bool,
) []shoal.ID {
	var result []shoal.ID
	for _, seed := range interactionSeeds {
		if seed == nodeID {
			continue
		}
		candidates := e.interactionOutgoing[seed]
		if outgoing {
			candidates = e.interactionIncoming[seed]
		}
		for _, edgeID := range candidates {
			edge := e.graphEdges[edgeID]
			if (outgoing && edge.From == nodeID) || (!outgoing && edge.To == nodeID) {
				result = append(result, edgeID)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return result
}

// mergeEdgeIDLists merges sorted edge ID lists without duplicates, stopping
// once it has fanout entries and knows whether another one remains.
func mergeEdgeIDLists(lists [][]shoal.ID, fanout uint32) ([]shoal.ID, bool) {
	positions := make([]int, len(lists))
	result := make([]shoal.ID, 0)
	for uint32(len(result)) <= fanout {
		best := -1
		for index, list := range lists {
			if positions[index] >= len(list) {
				continue
			}
			if best < 0 || shoal.CompareID(
				list[positions[index]], lists[best][positions[best]]) < 0 {
				best = index
			}
		}
		if best < 0 {
			break
		}
		next := lists[best][positions[best]]
		for index, list := range lists {
			if positions[index] < len(list) && list[positions[index]] == next {
				positions[index]++
			}
		}
		result = append(result, next)
	}
	if uint32(len(result)) > fanout {
		return result[:fanout], true
	}
	return result, false
}

func afterEdge(values []shoal.ID, after shoal.ID) []shoal.ID {
	if after == "" {
		return values
	}
	index := sort.Search(len(values), func(index int) bool {
		return shoal.CompareID(values[index], after) > 0
	})
	return values[index:]
}

func (e *Explorer) refreshSnapshotLocked() {
	hash := sha256.New()
	documentIDs := make([]shoal.ID, 0, len(e.documents))
	for id := range e.documents {
		documentIDs = append(documentIDs, id)
	}
	sort.Slice(documentIDs, func(i, j int) bool {
		return shoal.CompareID(documentIDs[i], documentIDs[j]) < 0
	})
	writeSnapshotString(hash, "documents")
	writeSnapshotCount(hash, len(documentIDs))
	asOf := e.snapshotAnchor
	for _, id := range documentIDs {
		record, err := latestRevision(e.documents[id])
		if err != nil || record == nil {
			continue
		}
		writeSnapshotString(hash, "document")
		writeSnapshotString(hash, string(record.Document.ID))
		writeSnapshotString(hash, string(record.Revision.ID))
		if record.PublishedAt.After(asOf) {
			asOf = record.PublishedAt
		}
	}
	nodeIDs := make([]shoal.ID, 0, len(e.graphNodes))
	for id, node := range e.graphNodes {
		// The snapshot is the content frontier. Interaction records are
		// written while inference is being served, so including them would
		// invalidate every concurrent reader's pinned snapshot.
		if interaction.IsInteractionKind(node.Kind) {
			continue
		}
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool {
		return shoal.CompareID(nodeIDs[i], nodeIDs[j]) < 0
	})
	writeSnapshotString(hash, "nodes")
	writeSnapshotCount(hash, len(nodeIDs))
	for _, id := range nodeIDs {
		node := e.graphNodes[id]
		writeSnapshotString(hash, "node")
		writeSnapshotString(hash, string(node.ID))
		writeSnapshotString(hash, node.Kind)
		writeSnapshotCount(hash, len(node.Labels))
		for _, label := range node.Labels {
			writeSnapshotString(hash, label)
		}
		writeSnapshotMetadata(hash, node.Properties)
	}
	edgeIDs := make([]shoal.ID, 0, len(e.graphEdges))
	for id, edge := range e.graphEdges {
		if interaction.IsInteractionEdgeType(edge.Type) {
			continue
		}
		edgeIDs = append(edgeIDs, id)
	}
	sort.Slice(edgeIDs, func(i, j int) bool {
		return shoal.CompareID(edgeIDs[i], edgeIDs[j]) < 0
	})
	writeSnapshotString(hash, "edges")
	writeSnapshotCount(hash, len(edgeIDs))
	for _, id := range edgeIDs {
		edge := e.graphEdges[id]
		writeSnapshotString(hash, "edge")
		writeSnapshotString(hash, string(edge.ID))
		writeSnapshotString(hash, string(edge.From))
		writeSnapshotString(hash, string(edge.To))
		writeSnapshotString(hash, edge.Type)
		var weight [8]byte
		binary.BigEndian.PutUint64(weight[:], math.Float64bits(float64(edge.Weight)))
		_, _ = hash.Write(weight[:])
		writeSnapshotMetadata(hash, edge.Properties)
		if record, ok := e.edges[id]; ok && record.PublishedAt.After(asOf) {
			asOf = record.PublishedAt
		}
	}
	assertionEdges := make([]shoal.ID, 0, len(e.graphAssertions))
	for _, edgeID := range edgeIDs {
		if _, ok := e.graphAssertions[edgeID]; ok {
			assertionEdges = append(assertionEdges, edgeID)
		}
	}
	writeSnapshotString(hash, "assertions")
	writeSnapshotCount(hash, len(assertionEdges))
	for _, edgeID := range assertionEdges {
		writeSnapshotString(hash, string(edgeID))
		writeSnapshotAssertion(hash, e.graphAssertions[edgeID])
	}
	for _, record := range e.extractions {
		if record != nil && record.PublishedAt.After(asOf) {
			asOf = record.PublishedAt
		}
	}
	for _, record := range e.graphMaterializations {
		if record != nil && record.PublishedAt.After(asOf) {
			asOf = record.PublishedAt
		}
	}
	sum := hash.Sum(nil)
	id := hex.EncodeToString(sum)
	// An equality frontier can recur after intervening changes. Reuse its
	// registered observation time rather than invalidating its historical pin.
	if registered, ok := e.snapshotHistory[id]; ok {
		asOf = registered.AsOf
	}
	e.snapshot = Snapshot{
		ID: id, AsOf: asOf.UTC(),
		Frontier: binary.BigEndian.Uint64(sum[:8]),
	}
}

type snapshotWriter interface {
	Write([]byte) (int, error)
}

func writeSnapshotString(writer snapshotWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write([]byte(value))
}

func writeSnapshotMetadata(writer snapshotWriter, metadata shoal.Metadata) {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeSnapshotCount(writer, len(keys))
	for _, key := range keys {
		writeSnapshotString(writer, key)
		writeSnapshotString(writer, metadata[key])
	}
}

func writeSnapshotCount(writer snapshotWriter, count int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(count))
	_, _ = writer.Write(encoded[:])
}
