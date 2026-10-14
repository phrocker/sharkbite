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

package explorer

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FoldRequest asks for a content-addressed fold over recorded interaction
// sessions.
//
// The request names sessions only. The provenance that is folded is read from
// what those sessions actually recorded, never from the caller, so a caller
// cannot understate what a session was shown and thereby widen the fold's
// visibility.
type FoldRequest struct {
	SessionIDs []shoal.ID
	// SummaryDigest is the SHA-256 digest, lowercase hex, of the summary text
	// this fold stands for. The summary text is held out-of-band and is never
	// persisted: it is derived from evidence, and an interaction record may
	// carry identities, digests, counts, and node IDs only. May be empty for a
	// purely structural fold.
	SummaryDigest string
}

// FoldResult describes a materialized fold.
type FoldResult struct {
	FoldID shoal.ID
	// Created is false when the same input had already been folded. Fold
	// identity is content-addressed, so refolding the same sessions with the
	// same summary digest is idempotent rather than a conflict.
	Created        bool
	Visibility     string
	FoldedAt       time.Time
	MemberCount    int
	RetrievedCount int
	CitedCount     int
}

// FoldSummary describes one stored fold without materializing its subgraph.
type FoldSummary struct {
	FoldID        shoal.ID
	FoldedAt      time.Time
	Visibility    string
	SummaryDigest string
	MemberCount   int
	NodeCount     int
	EdgeCount     int
	Deleted       bool
	DeletedAt     time.Time
}

type FoldSummaryPage struct {
	Folds     []FoldSummary
	NextAfter shoal.ID
}

const MaxFoldSummaryPageSize uint32 = 1000

type persistedFold struct {
	FoldID        shoal.ID
	Members       []interaction.FoldMember
	SummaryDigest string
	Nodes         []graph.Node
	Edges         []graph.Edge
	Visibility    string
	FoldedAt      time.Time
	Deleted       bool
	DeletedAt     time.Time
}

// FoldInteractions folds one or more recorded sessions into a single derived
// summary vertex, following the public fold semantics of phrocker/sag.
//
// The fold is an interaction.fold node in the reserved namespace, so it is
// excluded from retrieval and from graph expansion exactly as a session is: a
// model can never retrieve a fold, and can never cite one as though it were a
// source document.
//
// The fold's visibility is the conjunction of every folded session's own
// visibility and every label of every source node those sessions touched,
// retrieved as well as cited. Folding therefore only ever narrows visibility.
// Publishing a redacted public summary is a separate, explicit, reviewed
// action and is deliberately not implemented here.
//
// Folding is an operator action, not part of serving an inference, so it never
// sits on the request latency path.
func (e *Explorer) FoldInteractions(
	ctx context.Context, request FoldRequest,
) (FoldResult, error) {
	if err := contextError(ctx); err != nil {
		return FoldResult{}, err
	}
	sessionIDs, err := normalizeFoldSessionIDs(request.SessionIDs)
	if err != nil {
		return FoldResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return FoldResult{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		return FoldResult{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return FoldResult{}, err
	}

	members := make([]interaction.FoldMember, 0, len(sessionIDs))
	edgeProvenanceComplete := true
	for _, sessionID := range sessionIDs {
		record, ok, err := e.foldInteractionRecordLocked(sessionID)
		if err != nil {
			return FoldResult{}, err
		}
		if !ok {
			return FoldResult{}, shoal.NewError(
				shoal.ErrorNotFound, "interaction session not found")
		}
		visibility, err := interaction.ParseVisibility(record.Visibility)
		if err != nil {
			return FoldResult{}, err
		}
		touched := interaction.TouchedNodes(record.Nodes, record.Edges)
		member := interaction.FoldMember{
			SessionID:        sessionID,
			RetrievedNodeIDs: touched.RetrievedNodeIDs,
			CitedNodeIDs:     touched.CitedNodeIDs,
			Visibility:       visibility,
		}
		if !record.EdgeProvenanceComplete {
			edgeProvenanceComplete = false
		} else {
			member.TouchedEdgeIDs = record.Session.TouchedEdgeIDs()
		}
		members = append(members, member)
	}

	fold := interaction.Fold{
		Members:       members,
		SummaryDigest: request.SummaryDigest,
		FoldedAt:      time.Now().UTC(),
	}
	canonical, err := fold.Canonical()
	if err != nil {
		return FoldResult{}, err
	}
	foldID, err := canonical.ID()
	if err != nil {
		return FoldResult{}, err
	}
	legacyID, err := legacyFoldID(canonical)
	if err != nil {
		return FoldResult{}, err
	}
	if _, exists := e.folds[foldID]; !exists {
		if err := e.reconcilePersistedFoldLocked(foldID); err != nil {
			return FoldResult{}, err
		}
	}
	if existing, ok := e.folds[foldID]; ok {
		return e.foldIdempotentResultLocked(*existing, canonical)
	}
	if legacyID != foldID {
		if _, exists := e.folds[legacyID]; !exists {
			if err := e.reconcilePersistedFoldLocked(legacyID); err != nil {
				return FoldResult{}, err
			}
		}
		if existing, ok := e.folds[legacyID]; ok {
			return e.foldIdempotentResultLocked(*existing, canonical)
		}
	}
	if !edgeProvenanceComplete {
		var compatible *persistedFold
		for _, existing := range e.folds {
			existingFold := interaction.Fold{
				Members:       cloneFoldMembers(existing.Members),
				SummaryDigest: existing.SummaryDigest,
				FoldedAt:      existing.FoldedAt,
			}
			existingLegacyID, legacyErr := legacyFoldID(existingFold)
			if legacyErr != nil || existingLegacyID != legacyID {
				continue
			}
			if compatible != nil && compatible.FoldID != existing.FoldID {
				return FoldResult{}, shoal.NewError(
					shoal.ErrorConflict,
					"multiple folds match legacy interaction provenance",
				)
			}
			compatible = existing
		}
		if compatible != nil {
			return e.foldIdempotentResultLocked(*compatible, canonical)
		}
		return FoldResult{}, shoal.NewError(
			shoal.ErrorConflict,
			"fold member source-edge provenance is unavailable",
		)
	}
	for _, sessionID := range sessionIDs {
		record, ok := e.interactions[sessionID]
		if !ok {
			return FoldResult{}, shoal.NewError(
				shoal.ErrorNotFound, "interaction session not found")
		}
		if record.Deleted {
			return FoldResult{}, shoal.NewError(
				shoal.ErrorConflict,
				"interaction session was explicitly deleted and cannot be folded",
			)
		}
	}
	subgraph, err := canonical.SubgraphWithEvidence(
		e.visibilityResolverLocked(), e.edgeVisibilityResolverLocked())
	if err != nil {
		return FoldResult{}, err
	}
	// A fold and a session must never share an identity: both are written into
	// the same corpus node namespace, so a collision would drop one of them at
	// the next graph rebuild.
	if _, ok := e.interactions[subgraph.ID]; ok {
		return FoldResult{}, shoal.NewError(
			shoal.ErrorConflict,
			"fold identity is already used by an interaction session",
		)
	}
	reservedNodes := append([]graph.Node(nil), subgraph.Nodes...)
	reservedNodes = append(reservedNodes, graph.Node{
		ID: interaction.TombstoneID(subgraph.ID),
	})
	if err := e.requireInteractionGraphIDsAvailableLocked(
		reservedNodes, subgraph.Edges,
	); err != nil {
		return FoldResult{}, err
	}
	record := persistedFold{
		FoldID:        subgraph.ID,
		Members:       canonical.Members,
		SummaryDigest: canonical.SummaryDigest,
		Nodes:         subgraph.Nodes,
		Edges:         subgraph.Edges,
		Visibility:    interaction.Expression(subgraph.Visibility),
		FoldedAt:      fold.FoldedAt,
	}
	if err := validatePersistedFold(record); err != nil {
		return FoldResult{}, err
	}
	accepted, err := e.createInteractionRecord(
		foldRecordRow(record.FoldID), embeddedRecordFold, record,
	)
	if err != nil {
		return FoldResult{}, err
	}
	if !accepted {
		if err := e.reconcilePersistedFoldLocked(record.FoldID); err != nil {
			return FoldResult{}, err
		}
		existing, ok := e.folds[record.FoldID]
		if !ok {
			return FoldResult{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"fold create was rejected without a durable winner",
			)
		}
		return e.foldIdempotentResultLocked(*existing, canonical)
	}
	e.reserveInteractionRecordGraphIDsLocked(
		record.FoldID, record.Nodes, record.Edges)
	e.folds[record.FoldID] = &record
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return FoldResult{}, MarkCommittedInteraction(err)
	}
	return FoldResult{
		FoldID:         record.FoldID,
		Created:        true,
		Visibility:     record.Visibility,
		FoldedAt:       record.FoldedAt,
		MemberCount:    len(record.Members),
		RetrievedCount: len(subgraph.RetrievedNodeIDs),
		CitedCount:     len(subgraph.CitedNodeIDs),
	}, nil
}

func (e *Explorer) validateFoldRetryLocked(record persistedFold) error {
	if err := e.requireLiveFoldMembersLocked(record); err != nil {
		return err
	}
	current, err := e.currentSubgraphVisibilityLocked(record.Nodes, record.Edges)
	if err != nil {
		return err
	}
	sets := [][]string{}
	if current != "" {
		labels, err := interaction.ParseVisibility(current)
		if err != nil {
			return err
		}
		sets = append(sets, labels)
	}
	resolveEdge := e.edgeVisibilityResolverLocked()
	for _, member := range record.Members {
		session, ok := e.interactions[member.SessionID]
		if !ok || session.Deleted {
			return shoal.NewError(
				shoal.ErrorConflict,
				"fold member session was explicitly deleted",
			)
		}
		edgeIDs := member.TouchedEdgeIDs
		if len(edgeIDs) == 0 && session.EdgeProvenanceComplete {
			edgeIDs = session.Session.TouchedEdgeIDs()
		}
		for _, edgeID := range edgeIDs {
			labels, err := resolveEdge(edgeID)
			if err != nil {
				return err
			}
			sets = append(sets, labels)
		}
	}
	labels, err := interaction.Conjoin(sets...)
	if err != nil {
		return err
	}
	if !visibilityCovered(record.Visibility, interaction.Expression(labels)) {
		return staleDerivedVisibilityError()
	}
	return nil
}

// requireLiveFoldMembersLocked refuses to expose a fold's provenance once any
// member session has been explicitly deleted. Deletion normally cannot happen
// while a live fold references the session, but a fold whose create returned an
// indeterminate commit is durable while absent from the in-memory index that
// guard consults, so the member can be deleted before the fold is reconciled.
// A deleted session must not keep a rehydratable copy of what it recorded.
func (e *Explorer) requireLiveFoldMembersLocked(record persistedFold) error {
	for _, member := range record.Members {
		// Absence is treated like deletion: every recorded session, including
		// its tombstone, is loaded at open, so a member that is not indexed
		// cannot be shown to still exist.
		if existing, ok := e.interactions[member.SessionID]; !ok ||
			existing.Deleted {
			return shoal.NewError(
				shoal.ErrorConflict,
				"fold member session was explicitly deleted",
			)
		}
	}
	return nil
}

func (e *Explorer) currentFoldVisibilityLocked(
	record persistedFold,
) (string, error) {
	current, err := e.currentSubgraphVisibilityLocked(record.Nodes, record.Edges)
	if err != nil {
		return "", err
	}
	sets := [][]string{}
	if current != "" {
		labels, err := interaction.ParseVisibility(current)
		if err != nil {
			return "", err
		}
		sets = append(sets, labels)
	}
	resolveEdge := e.edgeVisibilityResolverLocked()
	edgeIDs, err := e.foldSourceEdgeIDsLocked(record)
	if err != nil {
		return "", err
	}
	for _, edgeID := range edgeIDs {
		labels, err := resolveEdge(edgeID)
		if err != nil {
			return "", err
		}
		sets = append(sets, labels)
	}
	labels, err := interaction.Conjoin(sets...)
	if err != nil {
		return "", err
	}
	return interaction.Expression(labels), nil
}

func (e *Explorer) foldSourceEdgeIDsLocked(
	record persistedFold,
) ([]shoal.ID, error) {
	var edgeIDs []shoal.ID
	for _, member := range record.Members {
		session, ok := e.interactions[member.SessionID]
		if !ok || session.Deleted {
			return nil, shoal.NewError(
				shoal.ErrorConflict,
				"fold member session was explicitly deleted",
			)
		}
		memberEdges := member.TouchedEdgeIDs
		if len(memberEdges) == 0 {
			if !session.EdgeProvenanceComplete {
				return nil, shoal.NewError(
					shoal.ErrorConflict,
					"fold source-edge provenance cannot be reconstructed",
				)
			}
			memberEdges = session.Session.TouchedEdgeIDs()
		}
		edgeIDs = append(edgeIDs, memberEdges...)
	}
	sort.Slice(edgeIDs, func(i, j int) bool {
		return shoal.CompareID(edgeIDs[i], edgeIDs[j]) < 0
	})
	result := edgeIDs[:0]
	for _, edgeID := range edgeIDs {
		if len(result) == 0 || result[len(result)-1] != edgeID {
			result = append(result, edgeID)
		}
	}
	return result, nil
}

func legacyFoldID(fold interaction.Fold) (shoal.ID, error) {
	legacy := fold
	legacy.Members = cloneFoldMembers(fold.Members)
	for index := range legacy.Members {
		legacy.Members[index].TouchedEdgeIDs = nil
	}
	return legacy.ID()
}

func (e *Explorer) foldInteractionRecordLocked(
	sessionID shoal.ID,
) (*persistedInteraction, bool, error) {
	record, ok := e.interactions[sessionID]
	if ok && !record.Deleted {
		return record, true, nil
	}
	historical, found, err := e.lookupPersistedLiveInteraction(sessionID)
	if err != nil || !found {
		return nil, found, err
	}
	return &historical, true, nil
}

// RehydrateFold unfolds a stored fold back into the provenance it replaced.
//
// Rehydration is lossless with respect to provenance: every folded session is
// returned with its retrieved set and its cited set kept apart, exactly as the
// session recorded them. Collapsing the two would understate what the model
// was shown and make the visibility conjunction unsound.
//
// It fails closed. A fold whose stored content no longer hashes to its own
// identity, a deleted fold, or a fold whose member session was explicitly
// deleted is refused rather than partially returned.
func (e *Explorer) RehydrateFold(
	ctx context.Context, foldID shoal.ID,
) (interaction.Fold, error) {
	if err := contextError(ctx); err != nil {
		return interaction.Fold{}, err
	}
	if err := shoal.ValidateRequiredID("fold ID", foldID); err != nil {
		return interaction.Fold{}, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return interaction.Fold{}, err
	}
	defer e.mu.RUnlock()
	record, ok := e.folds[foldID]
	if !ok {
		return interaction.Fold{}, shoal.NewError(
			shoal.ErrorNotFound, "fold not found")
	}
	if record.Deleted {
		return interaction.Fold{}, shoal.NewError(
			shoal.ErrorConflict, "fold was explicitly deleted")
	}
	if err := e.requireLiveFoldMembersLocked(*record); err != nil {
		return interaction.Fold{}, err
	}
	current, err := e.currentFoldVisibilityLocked(*record)
	if err != nil {
		return interaction.Fold{}, err
	}
	if !visibilityCovered(record.Visibility, current) {
		return interaction.Fold{}, staleDerivedVisibilityError()
	}
	fold := interaction.Fold{
		Members:       cloneFoldMembers(record.Members),
		SummaryDigest: record.SummaryDigest,
		FoldedAt:      record.FoldedAt,
	}
	storedHasEdgeProvenance := false
	for _, member := range fold.Members {
		if len(member.TouchedEdgeIDs) > 0 {
			storedHasEdgeProvenance = true
			break
		}
	}
	fold.Members, err = e.foldMembersWithSourceEdgesLocked(fold.Members)
	if err != nil {
		return interaction.Fold{}, err
	}
	derived, err := fold.ID()
	if err != nil {
		return interaction.Fold{}, err
	}
	legacy, legacyErr := legacyFoldID(fold)
	legacyMatches := !storedHasEdgeProvenance && legacy == foldID
	if legacyErr != nil || (derived != foldID && !legacyMatches) {
		return interaction.Fold{}, shoal.NewError(
			shoal.ErrorInternal,
			"stored fold does not hash to its own identity",
		)
	}
	return fold, nil
}

// Folds lists stored folds. Like Interactions, this is an explicit
// kind-scoped query: a fold is never returned by Retrieve and never
// discovered by expanding a source node's neighborhood.
func (e *Explorer) Folds(ctx context.Context) ([]FoldSummary, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	summaries := make([]FoldSummary, 0, len(e.folds))
	for _, record := range e.folds {
		if !record.Deleted {
			current, err := e.currentFoldVisibilityLocked(*record)
			if err != nil || !visibilityCovered(record.Visibility, current) {
				// Fail closed at read time: a live fold whose evidence was
				// reclassified to a stricter label after it was folded is
				// withheld rather than served under its stale, now
				// under-labelled visibility. A merely loosened source still
				// covers the stored label and is kept. See issue #273.
				continue
			}
		}
		summaries = append(summaries, FoldSummary{
			FoldID:        record.FoldID,
			FoldedAt:      record.FoldedAt,
			Visibility:    record.Visibility,
			SummaryDigest: record.SummaryDigest,
			MemberCount:   len(record.Members),
			NodeCount:     len(record.Nodes),
			EdgeCount:     len(record.Edges),
			Deleted:       record.Deleted,
			DeletedAt:     record.DeletedAt,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return shoal.CompareID(summaries[i].FoldID, summaries[j].FoldID) < 0
	})
	return summaries, nil
}

// FoldSubgraph returns one stored fold's node and edges. This is the explicit
// traversal entry point for a fold; it is never reached implicitly.
func (e *Explorer) FoldSubgraph(
	ctx context.Context, foldID shoal.ID,
) (Neighborhood, error) {
	if err := contextError(ctx); err != nil {
		return Neighborhood{}, err
	}
	if err := shoal.ValidateRequiredID("fold ID", foldID); err != nil {
		return Neighborhood{}, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return Neighborhood{}, err
	}
	defer e.mu.RUnlock()
	record, ok := e.folds[foldID]
	if !ok {
		return Neighborhood{}, shoal.NewError(shoal.ErrorNotFound, "fold not found")
	}
	if !record.Deleted {
		current, err := e.currentFoldVisibilityLocked(*record)
		if err != nil {
			return Neighborhood{}, err
		}
		if !visibilityCovered(record.Visibility, current) {
			return Neighborhood{}, staleDerivedVisibilityError()
		}
		if err := e.requireLiveFoldMembersLocked(*record); err != nil {
			return Neighborhood{}, err
		}
	}
	result := Neighborhood{
		Nodes: make([]graph.Node, 0, len(record.Nodes)),
		Edges: make([]graph.Edge, 0, len(record.Edges)),
	}
	for _, node := range record.Nodes {
		result.Nodes = append(result.Nodes, cloneNode(node))
	}
	for _, edge := range record.Edges {
		result.Edges = append(result.Edges, cloneEdge(edge))
	}
	return result, nil
}

// DeleteFold removes a fold's node and edges and leaves an
// interaction.tombstone in their place, matching the retention rule for
// sessions: explicit deletion only, no TTL, and the deletion is itself
// auditable.
func (e *Explorer) DeleteFold(
	ctx context.Context, foldID shoal.ID,
) (interaction.Tombstone, error) {
	if err := contextError(ctx); err != nil {
		return interaction.Tombstone{}, err
	}
	if err := shoal.ValidateRequiredID("fold ID", foldID); err != nil {
		return interaction.Tombstone{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return interaction.Tombstone{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		return interaction.Tombstone{}, err
	}
	if err := e.reconcilePersistedFoldLocked(foldID); err != nil {
		return interaction.Tombstone{}, err
	}
	existing, ok := e.folds[foldID]
	if !ok {
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorNotFound, "fold not found")
	}
	if existing.Deleted {
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorConflict, "fold is already deleted")
	}
	visibility, err := interaction.ParseVisibility(existing.Visibility)
	if err != nil {
		return interaction.Tombstone{}, err
	}
	tombstone := interaction.Tombstone{
		SessionID:  foldID,
		DeletedAt:  time.Now().UTC(),
		NodeCount:  len(existing.Nodes),
		EdgeCount:  len(existing.Edges),
		Visibility: visibility,
	}
	node, err := tombstone.Node()
	if err != nil {
		return interaction.Tombstone{}, err
	}
	if existingNode, exists := e.graphNodes[node.ID]; exists {
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorConflict,
			"fold tombstone ID collides with existing graph node "+
				string(existingNode.ID),
		)
	}
	// The members are dropped, not retained beside the tombstone: a deleted
	// fold must not keep a rehydratable copy of what it folded.
	record := persistedFold{
		FoldID:     foldID,
		Nodes:      []graph.Node{node},
		Visibility: existing.Visibility,
		FoldedAt:   existing.FoldedAt,
		Deleted:    true,
		DeletedAt:  tombstone.DeletedAt,
	}
	if err := validatePersistedFold(record); err != nil {
		return interaction.Tombstone{}, err
	}
	accepted, err := e.deleteInteractionRecord(
		foldRecordRow(foldID), embeddedRecordFold, record,
	)
	if err != nil {
		return interaction.Tombstone{}, err
	}
	if !accepted {
		if err := e.reconcilePersistedFoldLocked(foldID); err != nil {
			return interaction.Tombstone{}, err
		}
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorConflict,
			"fold deletion lost a concurrent durable race",
		)
	}
	e.reserveInteractionRecordGraphIDsLocked(
		record.FoldID, record.Nodes, record.Edges)
	e.folds[foldID] = &record
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return interaction.Tombstone{}, MarkCommittedInteraction(err)
	}
	return tombstone, nil
}

// foldsReferencingLocked reports the live folds that fold a given session.
func (e *Explorer) foldsReferencingLocked(sessionID shoal.ID) []shoal.ID {
	var referencing []shoal.ID
	for _, record := range e.folds {
		if record.Deleted {
			continue
		}
		for _, member := range record.Members {
			if member.SessionID == sessionID {
				referencing = append(referencing, record.FoldID)
				break
			}
		}
	}
	sort.Slice(referencing, func(i, j int) bool {
		return shoal.CompareID(referencing[i], referencing[j]) < 0
	})
	return referencing
}

func (e *Explorer) foldIdempotentResultLocked(
	existing persistedFold, candidate interaction.Fold,
) (FoldResult, error) {
	if existing.Deleted {
		return FoldResult{}, shoal.NewError(
			shoal.ErrorConflict,
			"fold identity was explicitly deleted and cannot be reused",
		)
	}
	if err := e.validateFoldRetryLocked(existing); err != nil {
		return FoldResult{}, err
	}
	members := cloneFoldMembers(existing.Members)
	candidateMembers := cloneFoldMembers(candidate.Members)
	candidateByID := make(map[shoal.ID]int, len(candidateMembers))
	for index, member := range candidateMembers {
		candidateByID[member.SessionID] = index
	}
	for index := range members {
		candidateIndex, ok := candidateByID[members[index].SessionID]
		if !ok {
			continue
		}
		if len(members[index].TouchedEdgeIDs) == 0 {
			members[index].TouchedEdgeIDs = append(
				[]shoal.ID(nil), candidateMembers[candidateIndex].TouchedEdgeIDs...)
		}
		if len(candidateMembers[candidateIndex].TouchedEdgeIDs) == 0 {
			candidateMembers[candidateIndex].TouchedEdgeIDs = append(
				[]shoal.ID(nil), members[index].TouchedEdgeIDs...)
		}
	}
	existingCanonical, err := (interaction.Fold{
		Members: members, SummaryDigest: existing.SummaryDigest,
		FoldedAt: existing.FoldedAt,
	}).Canonical()
	if err != nil || !reflect.DeepEqual(
		existingCanonical.Members, candidateMembers,
	) || existingCanonical.SummaryDigest != candidate.SummaryDigest {
		return FoldResult{}, shoal.NewError(
			shoal.ErrorConflict,
			"fold ID already exists with different content",
		)
	}
	touched := interaction.TouchedNodes(existing.Nodes, existing.Edges)
	return FoldResult{
		FoldID:         existing.FoldID,
		Created:        false,
		Visibility:     existing.Visibility,
		FoldedAt:       existing.FoldedAt,
		MemberCount:    len(existing.Members),
		RetrievedCount: len(touched.RetrievedNodeIDs),
		CitedCount:     len(touched.CitedNodeIDs),
	}, nil
}

func (e *Explorer) foldMembersWithSourceEdgesLocked(
	members []interaction.FoldMember,
) ([]interaction.FoldMember, error) {
	result := cloneFoldMembers(members)
	for index := range result {
		if len(result[index].TouchedEdgeIDs) != 0 {
			continue
		}
		session, ok := e.interactions[result[index].SessionID]
		if !ok || session.Deleted || !session.EdgeProvenanceComplete {
			return nil, shoal.NewError(
				shoal.ErrorConflict,
				"fold source-edge provenance cannot be reconstructed",
			)
		}
		result[index].TouchedEdgeIDs = session.Session.TouchedEdgeIDs()
	}
	return result, nil
}

func normalizeFoldSessionIDs(ids []shoal.ID) ([]shoal.ID, error) {
	if len(ids) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fold requires at least one session ID")
	}
	if len(ids) > interaction.MaxFoldMembers {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fold exceeds the public member bound")
	}
	seen := make(map[shoal.ID]struct{}, len(ids))
	normalized := make([]shoal.ID, 0, len(ids))
	for _, id := range ids {
		if err := shoal.ValidateRequiredID(
			"interaction session ID", id,
		); err != nil {
			return nil, err
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return shoal.CompareID(normalized[i], normalized[j]) < 0
	})
	return normalized, nil
}

func cloneFoldMembers(members []interaction.FoldMember) []interaction.FoldMember {
	if len(members) == 0 {
		return nil
	}
	cloned := make([]interaction.FoldMember, 0, len(members))
	for _, member := range members {
		cloned = append(cloned, interaction.FoldMember{
			SessionID:        member.SessionID,
			RetrievedNodeIDs: append([]shoal.ID(nil), member.RetrievedNodeIDs...),
			CitedNodeIDs:     append([]shoal.ID(nil), member.CitedNodeIDs...),
			TouchedEdgeIDs:   append([]shoal.ID(nil), member.TouchedEdgeIDs...),
			Visibility:       append([]string(nil), member.Visibility...),
		})
	}
	return cloned
}

func validatePersistedFold(record persistedFold) error {
	if err := shoal.ValidateRequiredID("fold ID", record.FoldID); err != nil {
		return err
	}
	if record.FoldedAt.IsZero() {
		return shoal.NewError(shoal.ErrorInternal, "stored fold time is missing")
	}
	if _, err := interaction.ParseVisibility(record.Visibility); err != nil {
		return err
	}
	if record.Deleted && record.DeletedAt.IsZero() {
		return shoal.NewError(
			shoal.ErrorInternal, "stored fold deletion time is missing")
	}
	if !record.Deleted && len(record.Members) == 0 {
		return shoal.NewError(shoal.ErrorInternal, "stored fold has no members")
	}
	if len(record.Nodes) == 0 {
		return shoal.NewError(shoal.ErrorInternal, "stored fold has no nodes")
	}
	for _, node := range record.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
		if !interaction.IsInteractionKind(node.Kind) {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored fold record contains a non-interaction node",
			)
		}
	}
	for _, edge := range record.Edges {
		if err := validatePersistedEdge(edge); err != nil {
			return err
		}
		if !interaction.IsInteractionEdgeType(edge.Type) {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored fold record contains a non-interaction edge",
			)
		}
	}
	return nil
}
