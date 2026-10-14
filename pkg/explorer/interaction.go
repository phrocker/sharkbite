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

// InteractionSummary describes one recorded or tombstoned interaction session
// without materializing its subgraph.
type InteractionSummary struct {
	SessionID                shoal.ID
	InferenceID              shoal.ID
	RecordedAt               time.Time
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
	AuthorizationFingerprint shoal.ID
	AuthorizationExpiresAt   time.Time
	AuthorizationOperation   string
	OntologySchemaID         shoal.ID
	OntologyVersionID        shoal.ID
	EmbeddingSpaceID         shoal.ID
	EmbeddingSpaceDigest     string
	EmbeddingSpaceCount      int
	Operation                interaction.Operation
	Actor                    interaction.ActorContext
	Reason                   interaction.Reason
	// CorrelationID is the typed session's decision correlation (#532). It is
	// read from the typed record, so a legacy record without one, and a
	// tombstone, carry none.
	CorrelationID shoal.ID
	Visibility    string
	NodeCount     int
	EdgeCount     int
	Deleted       bool
	DeletedAt     time.Time
}

// InteractionRecord is the bulk/point authorization view of one durable
// interaction. TouchedNodeIDs is available even for legacy records whose typed
// Session payload predates hydration support.
type InteractionRecord struct {
	Summary        InteractionSummary
	Session        interaction.Session
	TouchedNodeIDs []shoal.ID
	TouchedEdgeIDs []shoal.ID
}

type InteractionRecordPage struct {
	Records   []InteractionRecord
	NextAfter shoal.ID
}

const MaxInteractionRecordPageSize uint32 = 1000

type persistedInteraction struct {
	SessionID                shoal.ID
	Session                  interaction.Session
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
	AuthorizationFingerprint shoal.ID
	AuthorizationExpiresAt   time.Time
	AuthorizationOperation   string
	OntologySchemaID         shoal.ID
	OntologyVersionID        shoal.ID
	EmbeddingSpaceID         shoal.ID
	Operation                interaction.Operation
	Actor                    interaction.ActorContext
	Reason                   interaction.Reason
	EdgeProvenanceComplete   bool
	Nodes                    []graph.Node
	Edges                    []graph.Edge
	Visibility               string
	RecordedAt               time.Time
	Deleted                  bool
	DeletedAt                time.Time
}

type persistedInteractionSink struct {
	CheckedAt time.Time
}

// InteractionWriter is the durable append boundary consumed by production
// inference recorders. Implementations must fail closed on an unavailable
// sink and preserve indeterminate-commit errors unless the exact write can be
// read back.
type InteractionWriter interface {
	EnsureInteractionSink(context.Context) error
	RecordInteraction(context.Context, interaction.Session) error
}

// InteractionResultWriter extends InteractionWriter for product adapters that
// must receive the exact canonical session accepted for persistence.
type InteractionResultWriter interface {
	InteractionWriter
	RecordInteractionResult(
		context.Context, interaction.Session,
	) (interaction.Session, error)
}

// InteractionReader is the explicit opt-in surface for derived interaction
// data. These methods are intentionally absent from Client, so source
// retrieval cannot begin returning derived nodes by interface expansion.
type InteractionReader interface {
	Interactions(context.Context) ([]InteractionSummary, error)
	InteractionRecords(context.Context) ([]InteractionRecord, error)
	InteractionRecord(context.Context, shoal.ID) (InteractionRecord, error)
	Interaction(context.Context, shoal.ID) (interaction.Session, error)
	InteractionSubgraph(context.Context, shoal.ID) (Neighborhood, error)
}

// EnsureInteractionSink verifies at setup time that this corpus can durably
// record interactions. Capture is part of serving an inference, so a corpus
// that cannot accept an interaction record must refuse to serve one at all,
// with a clear diagnostic here rather than an opaque failure at first write.
func (e *Explorer) EnsureInteractionSink(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return err
	}
	if e.readOnly {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"corpus is open read-only and cannot record interactions; "+
				"inference requires a writable interaction sink",
		)
	}
	if err := e.writeInteractionRecord(
		interactionSinkRow,
		embeddedRecordInteractionSink,
		persistedInteractionSink{CheckedAt: time.Now().UTC()},
	); err != nil {
		return shoal.WrapError(
			shoal.ErrorUnavailable,
			"corpus has no writable interaction sink; "+
				"inference requires a writable interaction sink",
			err,
		)
	}
	return nil
}

// RecordInteraction durably stores one interaction session as reserved
// interaction.* nodes and edges in this corpus.
//
// The record's visibility is the conjunction of every visibility label of
// every source node the session touched, retrieved as well as cited. It is
// never derived from the asker's grants. If any touched node cannot be
// resolved, nothing is written.
func (e *Explorer) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	_, err := e.recordInteractionResult(ctx, session)
	return err
}

func (e *Explorer) recordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	if err := contextError(ctx); err != nil {
		return interaction.Session{}, err
	}
	canonical, err := session.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	session = canonical
	requiredVisibility := interaction.RequiredVisibility(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return interaction.Session{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		return interaction.Session{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return interaction.Session{}, err
	}
	if _, exists := e.interactions[session.ID]; !exists {
		if err := e.reconcilePersistedInteractionLocked(session.ID); err != nil {
			return interaction.Session{}, err
		}
	}
	if existing, ok := e.interactions[session.ID]; ok {
		if !visibilityCovered(
			existing.Visibility,
			interaction.Expression(requiredVisibility),
		) {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorConflict,
				"interaction retry requires stricter output visibility",
			)
		}
		if err := interactionRetryResult(*existing, session); err != nil {
			return interaction.Session{}, err
		}
		// The exact record is already durable, so a cancellation observed
		// here must never be reported as a rollback.
		reconciled := cloneInteractionSession(existing.Session)
		if err := contextError(ctx); err != nil {
			return reconciled, MarkCommittedInteraction(err)
		}
		return reconciled, nil
	}
	// Sessions and folds are distinct maps but share one node namespace in the
	// corpus graph, so an ID taken by either would silently overwrite the other
	// during a graph rebuild and leave two records claiming one identity.
	if _, ok := e.folds[session.ID]; ok {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorConflict,
			"interaction session ID is already used by a fold",
		)
	}
	// The authorized boundary validates the pin before entering the durable
	// writer. Repeat the exact-state check while holding the graph/write lock
	// so a concurrent source mutation cannot race between validation,
	// visibility materialization, and persistence. Legacy direct callers with
	// an unregistered descriptive pin retain their existing behavior.
	if _, trusted := e.snapshotHistory[string(session.SnapshotID)]; trusted {
		references, err := session.EvidenceReferences()
		if err != nil {
			return interaction.Session{}, err
		}
		if err := e.validateEvidenceSnapshotLocked(
			session.SnapshotID,
			session.SnapshotAsOf,
			session.TouchedNodeIDs(),
			session.TouchedEdgeIDs(),
			references,
		); err != nil {
			return interaction.Session{}, err
		}
	}
	subgraph, err := session.SubgraphWithEvidence(
		e.visibilityResolverLocked(),
		e.edgeVisibilityResolverLocked(),
	)
	if err != nil {
		return interaction.Session{}, err
	}
	subgraph, err = interaction.ConjoinSubgraphVisibility(
		subgraph, requiredVisibility)
	if err != nil {
		return interaction.Session{}, err
	}
	reservedNodes := append([]graph.Node(nil), subgraph.Nodes...)
	reservedNodes = append(reservedNodes, graph.Node{
		ID: interaction.TombstoneID(session.ID),
	})
	if err := e.requireInteractionGraphIDsAvailableLocked(
		reservedNodes, subgraph.Edges,
	); err != nil {
		return interaction.Session{}, err
	}
	record := persistedInteraction{
		SessionID:                session.ID,
		Session:                  session,
		SnapshotID:               session.SnapshotID,
		SnapshotAsOf:             session.SnapshotAsOf,
		AuthorizationFingerprint: session.AuthorizationFingerprint,
		AuthorizationExpiresAt:   session.AuthorizationExpiresAt,
		AuthorizationOperation:   session.AuthorizationOperation,
		OntologySchemaID:         session.OntologySchemaID,
		OntologyVersionID:        session.OntologyVersionID,
		EmbeddingSpaceID:         session.EmbeddingSpaceID,
		Operation:                session.Operation,
		Actor:                    session.Actor,
		Reason:                   session.Reason,
		EdgeProvenanceComplete:   true,
		Nodes:                    subgraph.Nodes,
		Edges:                    subgraph.Edges,
		Visibility:               interaction.Expression(subgraph.Visibility),
		RecordedAt:               session.RecordedAt.UTC(),
	}

	if err := validatePersistedInteraction(record); err != nil {
		return interaction.Session{}, err
	}
	accepted, err := e.createInteractionRecord(
		interactionRecordRow(session.ID), embeddedRecordInteraction, record,
	)
	if err != nil {
		return interaction.Session{}, err
	}
	if !accepted {
		if err := e.reconcilePersistedInteractionLocked(session.ID); err != nil {
			return interaction.Session{}, err
		}
		existing, ok := e.interactions[session.ID]
		if !ok {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"interaction create was rejected without a durable winner",
			)
		}
		if !visibilityCovered(
			existing.Visibility,
			interaction.Expression(requiredVisibility),
		) {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorConflict,
				"interaction retry requires stricter output visibility",
			)
		}
		if err := interactionRetryResult(*existing, session); err != nil {
			return interaction.Session{}, err
		}
		// The durable winner already holds this exact record, so a
		// cancellation observed here is a post-commit failure.
		reconciled := cloneInteractionSession(existing.Session)
		if err := contextError(ctx); err != nil {
			return reconciled, MarkCommittedInteraction(err)
		}
		return reconciled, nil
	}
	e.reserveInteractionRecordGraphIDsLocked(
		record.SessionID, record.Nodes, record.Edges)
	e.interactions[session.ID] = &record
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return cloneInteractionSession(record.Session),
			MarkCommittedInteraction(err)
	}
	acceptedSession := cloneInteractionSession(record.Session)
	if err := contextError(ctx); err != nil {
		return acceptedSession, MarkCommittedInteraction(err)
	}
	return acceptedSession, nil
}

func (e *Explorer) requireInteractionGraphIDsAvailableLocked(
	nodes []graph.Node, edges []graph.Edge,
) error {
	for _, node := range nodes {
		if _, exists := e.interactionNodeIDs[node.ID]; exists {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction node ID is already reserved "+string(node.ID),
			)
		}
		if existing, ok := e.graphNodes[node.ID]; ok {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction node ID collides with existing graph node "+
					string(existing.ID),
			)
		}
	}
	for _, edge := range edges {
		if _, exists := e.interactionEdgeIDs[edge.ID]; exists {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction edge ID is already reserved "+string(edge.ID),
			)
		}
		if existing, ok := e.graphEdges[edge.ID]; ok {
			return shoal.NewError(
				shoal.ErrorConflict,
				"interaction edge ID collides with existing graph edge "+
					string(existing.ID),
			)
		}
	}
	return nil
}

func (e *Explorer) requireSourceGraphIDsAvailableLocked(
	nodes []graph.Node, edges []graph.Edge,
) error {
	for _, node := range nodes {
		if _, exists := e.interactionNodeIDs[node.ID]; exists {
			return shoal.NewError(
				shoal.ErrorConflict,
				"source node ID collides with an interaction node",
			)
		}
	}
	for _, edge := range edges {
		if _, exists := e.interactionEdgeIDs[edge.ID]; exists {
			return shoal.NewError(
				shoal.ErrorConflict,
				"source edge ID collides with an interaction edge",
			)
		}
	}
	return nil
}

func (e *Explorer) reserveInteractionGraphIDsLocked(
	nodes []graph.Node, edges []graph.Edge,
) {
	for _, node := range nodes {
		e.interactionNodeIDs[node.ID] = struct{}{}
	}
	for _, edge := range edges {
		e.interactionEdgeIDs[edge.ID] = struct{}{}
	}
}

func (e *Explorer) reserveInteractionRecordGraphIDsLocked(
	recordID shoal.ID, nodes []graph.Node, edges []graph.Edge,
) {
	e.reserveInteractionGraphIDsLocked(nodes, edges)
	e.interactionNodeIDs[recordID] = struct{}{}
	e.interactionNodeIDs[interaction.TombstoneID(recordID)] = struct{}{}
}

func (e *Explorer) reconcilePersistedInteractionLocked(
	sessionID shoal.ID,
) error {
	record, found, err := e.lookupPersistedInteraction(sessionID)
	if err != nil || !found {
		return err
	}
	if current, ok := e.interactions[sessionID]; ok {
		if persistedInteractionsEqual(*current, record) ||
			(current.Deleted && !record.Deleted) {
			return nil
		}
		if !current.Deleted && !record.Deleted {
			return shoal.NewError(
				shoal.ErrorConflict,
				"durable interaction session has different live content",
			)
		}
	}
	copy := record
	e.reserveInteractionRecordGraphIDsLocked(
		copy.SessionID, copy.Nodes, copy.Edges)
	e.interactions[sessionID] = &copy
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return MarkCommittedInteraction(err)
	}
	return nil
}

func (e *Explorer) reconcilePersistedFoldLocked(foldID shoal.ID) error {
	record, found, err := e.lookupPersistedFold(foldID)
	if err != nil || !found {
		return err
	}
	if current, ok := e.folds[foldID]; ok {
		if persistedFoldsEqual(*current, record) ||
			(current.Deleted && !record.Deleted) {
			return nil
		}
		if !current.Deleted && !record.Deleted {
			return shoal.NewError(
				shoal.ErrorConflict,
				"durable fold has different live content",
			)
		}
	}
	copy := record
	e.reserveInteractionRecordGraphIDsLocked(
		copy.FoldID, copy.Nodes, copy.Edges)
	e.folds[foldID] = &copy
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return MarkCommittedInteraction(err)
	}
	return nil
}

func persistedInteractionsEqual(
	left, right persistedInteraction,
) bool {
	switch {
	case left.Session.ID == "" && right.Session.ID == "":
	case left.Session.ID == "" || right.Session.ID == "":
		return false
	default:
		leftSession, leftErr := left.Session.Canonical()
		rightSession, rightErr := right.Session.Canonical()
		if leftErr != nil || rightErr != nil {
			return false
		}
		left.Session = leftSession
		right.Session = rightSession
	}
	if len(left.Nodes) == 0 {
		left.Nodes = nil
	}
	if len(right.Nodes) == 0 {
		right.Nodes = nil
	}
	if len(left.Edges) == 0 {
		left.Edges = nil
	}
	if len(right.Edges) == 0 {
		right.Edges = nil
	}
	return reflect.DeepEqual(left, right)
}

func interactionRetryResult(
	existing persistedInteraction, session interaction.Session,
) error {
	if existing.Deleted {
		return shoal.NewError(
			shoal.ErrorConflict,
			"interaction session ID was explicitly deleted and cannot be reused",
		)
	}
	existingCanonical, err := existing.Session.Canonical()
	retryCanonical := session
	retryCanonical.RecordedAt = existingCanonical.RecordedAt
	// Correlation is metadata, not content (#532): a retry under another
	// correlation is the same session, and keeps the first one recorded.
	retryCanonical.CorrelationID = existingCanonical.CorrelationID
	if err == nil && reflect.DeepEqual(existingCanonical, retryCanonical) {
		return nil
	}
	return shoal.NewError(
		shoal.ErrorConflict,
		"interaction session ID already exists with different content",
	)
}

func persistedFoldsEqual(left, right persistedFold) bool {
	leftFold, leftErr := (interaction.Fold{
		Members: left.Members, SummaryDigest: left.SummaryDigest,
		FoldedAt: left.FoldedAt,
	}).Canonical()
	rightFold, rightErr := (interaction.Fold{
		Members: right.Members, SummaryDigest: right.SummaryDigest,
		FoldedAt: right.FoldedAt,
	}).Canonical()
	if leftErr != nil || rightErr != nil {
		return false
	}
	left.Members = leftFold.Members
	right.Members = rightFold.Members
	if len(left.Nodes) == 0 {
		left.Nodes = nil
	}
	if len(right.Nodes) == 0 {
		right.Nodes = nil
	}
	if len(left.Edges) == 0 {
		left.Edges = nil
	}
	if len(right.Edges) == 0 {
		right.Edges = nil
	}
	return reflect.DeepEqual(left, right)
}

// RecordInteractionResult records a session and returns the exact canonical
// value accepted for persistence. The returned value is independently owned.
func (e *Explorer) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	return e.recordInteractionResult(ctx, session)
}

// DeleteInteraction removes one interaction session's nodes and edges and
// leaves a tombstone node in their place. Retention is explicit deletion only;
// there is no TTL, and a deletion is itself auditable.
func (e *Explorer) DeleteInteraction(
	ctx context.Context, sessionID shoal.ID,
) (interaction.Tombstone, error) {
	if err := contextError(ctx); err != nil {
		return interaction.Tombstone{}, err
	}
	if err := shoal.ValidateRequiredID(
		"interaction session ID", sessionID,
	); err != nil {
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
	if err := e.reconcilePersistedInteractionLocked(sessionID); err != nil {
		return interaction.Tombstone{}, err
	}
	existing, ok := e.interactions[sessionID]
	if !ok {
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorNotFound, "interaction session not found")
	}
	if existing.Deleted {
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorConflict, "interaction session is already deleted")
	}
	// A session that a live fold summarizes cannot be deleted out from under
	// it, because the fold would keep a rehydratable copy of provenance the
	// operator just asked to destroy. Delete the fold first.
	if folds := e.foldsReferencingLocked(sessionID); len(folds) > 0 {
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorConflict,
			"interaction session is folded into a summary; "+
				"delete the fold before deleting the session",
		)
	}
	visibility, err := interaction.ParseVisibility(existing.Visibility)
	if err != nil {
		return interaction.Tombstone{}, err
	}
	tombstone := interaction.Tombstone{
		SessionID:  sessionID,
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
			"interaction tombstone ID collides with existing graph node "+
				string(existingNode.ID),
		)
	}
	record := persistedInteraction{
		SessionID:                sessionID,
		SnapshotID:               existing.SnapshotID,
		SnapshotAsOf:             existing.SnapshotAsOf,
		AuthorizationFingerprint: existing.AuthorizationFingerprint,
		AuthorizationExpiresAt:   existing.AuthorizationExpiresAt,
		AuthorizationOperation:   existing.AuthorizationOperation,
		OntologySchemaID:         existing.OntologySchemaID,
		OntologyVersionID:        existing.OntologyVersionID,
		EmbeddingSpaceID:         existing.EmbeddingSpaceID,
		Operation:                existing.Operation,
		Actor:                    existing.Actor,
		Reason:                   existing.Reason,
		Nodes:                    []graph.Node{node},
		Visibility:               existing.Visibility,
		RecordedAt:               existing.RecordedAt,
		Deleted:                  true,
		DeletedAt:                tombstone.DeletedAt,
	}
	if err := validatePersistedInteraction(record); err != nil {
		return interaction.Tombstone{}, err
	}
	accepted, err := e.deleteInteractionRecord(
		interactionRecordRow(sessionID), embeddedRecordInteraction, record,
	)
	if err != nil {
		return interaction.Tombstone{}, err
	}
	if !accepted {
		if err := e.reconcilePersistedInteractionLocked(sessionID); err != nil {
			return interaction.Tombstone{}, err
		}
		return interaction.Tombstone{}, shoal.NewError(
			shoal.ErrorConflict,
			"interaction deletion lost a concurrent durable race",
		)
	}
	e.reserveInteractionRecordGraphIDsLocked(
		record.SessionID, record.Nodes, record.Edges)
	e.interactions[sessionID] = &record
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return interaction.Tombstone{}, MarkCommittedInteraction(err)
	}
	return tombstone, nil
}

// Interactions lists recorded interaction sessions. This is the explicit
// kind-scoped query; interaction records are never returned by Retrieve and
// are never discovered by graph expansion from a source node.
func (e *Explorer) Interactions(ctx context.Context) ([]InteractionSummary, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	summaries := make([]InteractionSummary, 0, len(e.interactions))
	for _, record := range e.interactions {
		if !record.Deleted {
			current, err := e.currentInteractionVisibilityLocked(*record)
			if err != nil || !visibilityCovered(record.Visibility, current) {
				// Fail closed at read time: a live session whose evidence was
				// reclassified to a stricter label after it was recorded is
				// withheld rather than served under its stale, now
				// under-labelled visibility. A merely loosened source still
				// covers the stored label and is kept. See issue #273.
				continue
			}
		}
		summaries = append(summaries, interactionSummary(*record))
	}
	sort.Slice(summaries, func(i, j int) bool {
		return shoal.CompareID(summaries[i].SessionID, summaries[j].SessionID) < 0
	})
	return summaries, nil
}

// InteractionRecords returns the explicit derived records in one read pass so
// authorization wrappers can batch source-policy resolution instead of
// performing one storage read and one policy round trip per interaction.
func (e *Explorer) InteractionRecords(
	ctx context.Context,
) ([]InteractionRecord, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	records := make([]InteractionRecord, 0, len(e.interactions))
	for _, stored := range e.interactions {
		if !stored.Deleted {
			current, err := e.currentInteractionVisibilityLocked(*stored)
			if err != nil || !visibilityCovered(stored.Visibility, current) {
				continue
			}
		}
		records = append(records, interactionRecord(*stored))
	}
	sort.Slice(records, func(i, j int) bool {
		return shoal.CompareID(
			records[i].Summary.SessionID,
			records[j].Summary.SessionID,
		) < 0
	})
	return records, nil
}

// InteractionRecord returns one explicit derived record without scanning the
// durable interaction history.
func (e *Explorer) InteractionRecord(
	ctx context.Context, sessionID shoal.ID,
) (InteractionRecord, error) {
	if err := contextError(ctx); err != nil {
		return InteractionRecord{}, err
	}
	if err := shoal.ValidateRequiredID(
		"interaction session ID", sessionID,
	); err != nil {
		return InteractionRecord{}, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return InteractionRecord{}, err
	}
	defer e.mu.RUnlock()
	stored, ok := e.interactions[sessionID]
	if !ok {
		return InteractionRecord{}, shoal.NewError(
			shoal.ErrorNotFound, "interaction session not found")
	}
	if !stored.Deleted {
		current, err := e.currentInteractionVisibilityLocked(*stored)
		if err != nil {
			return InteractionRecord{}, err
		}
		if !visibilityCovered(stored.Visibility, current) {
			return InteractionRecord{}, staleDerivedVisibilityError()
		}
	}
	return interactionRecord(*stored), nil
}

// Interaction returns the typed, redacted session record. This is an explicit
// derived-data view: default retrieval never reaches it. Source visibility is
// re-evaluated before hydration, so a later source tightening or revocation
// fails closed rather than serving the record under its observed label.
func (e *Explorer) Interaction(
	ctx context.Context, sessionID shoal.ID,
) (interaction.Session, error) {
	record, err := e.InteractionRecord(ctx, sessionID)
	if err != nil {
		return interaction.Session{}, err
	}
	if record.Summary.Deleted {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorConflict, "interaction session was explicitly deleted")
	}
	if record.Session.ID == "" {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"legacy interaction record has no typed session payload",
		)
	}
	return record.Session, nil
}

// InteractionSubgraph returns one recorded session's nodes and edges. This is
// the explicit traversal entry point; it is never reached implicitly.
func (e *Explorer) InteractionSubgraph(
	ctx context.Context, sessionID shoal.ID,
) (Neighborhood, error) {
	if err := contextError(ctx); err != nil {
		return Neighborhood{}, err
	}
	if err := shoal.ValidateRequiredID(
		"interaction session ID", sessionID,
	); err != nil {
		return Neighborhood{}, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return Neighborhood{}, err
	}
	defer e.mu.RUnlock()
	record, ok := e.interactions[sessionID]
	if !ok {
		return Neighborhood{}, shoal.NewError(
			shoal.ErrorNotFound, "interaction session not found")
	}
	if !record.Deleted {
		current, err := e.currentInteractionVisibilityLocked(*record)
		if err != nil {
			return Neighborhood{}, err
		}
		if !visibilityCovered(record.Visibility, current) {
			return Neighborhood{}, staleDerivedVisibilityError()
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

// visibilityResolverLocked resolves the declared visibility labels of a source
// node. It fails closed: an unknown node, or an interaction node presented as
// if it were source evidence, produces an error rather than an empty label set.
func (e *Explorer) visibilityResolverLocked() interaction.VisibilityResolver {
	return func(id shoal.ID) ([]string, error) {
		node, ok := e.graphNodes[id]
		if !ok {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"interaction touched node "+string(id)+
					", which is no longer in the corpus graph, so its visibility "+
					"cannot be derived; this usually means the node was removed or "+
					"replaced by a concurrent re-ingest between retrieval and "+
					"recording. The request fails by design rather than recording "+
					"an interaction with an under-labelled visibility. Retry the "+
					"request against the updated corpus",
			)
		}
		if interaction.IsInteractionKind(node.Kind) {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"interaction cannot treat another interaction node as source "+
					"evidence: node "+string(id)+" has kind "+node.Kind,
			)
		}
		return interaction.NodeVisibility(node)
	}
}

// edgeVisibilityResolverLocked resolves an exact source edge to the
// conjunction of the current declared visibility of both endpoints. Missing
// or interaction-owned edges fail closed. Authorization wrappers additionally
// re-evaluate the edge-local policy rule.
func (e *Explorer) edgeVisibilityResolverLocked() interaction.VisibilityResolver {
	resolveNode := e.visibilityResolverLocked()
	return func(id shoal.ID) ([]string, error) {
		edge, ok := e.graphEdges[id]
		if !ok {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"interaction touched edge "+string(id)+
					", which is no longer in the corpus graph",
			)
		}
		if interaction.IsInteractionID(edge.ID) {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"interaction cannot treat another interaction edge as source evidence",
			)
		}
		from, err := resolveNode(edge.From)
		if err != nil {
			return nil, err
		}
		to, err := resolveNode(edge.To)
		if err != nil {
			return nil, err
		}
		local, err := interaction.EdgeVisibility(edge)
		if err != nil {
			return nil, err
		}
		return interaction.Conjoin(from, to, local)
	}
}

func (e *Explorer) currentInteractionVisibilityLocked(
	record persistedInteraction,
) (string, error) {
	if !record.EdgeProvenanceComplete {
		return "", shoal.NewError(
			shoal.ErrorUnavailable,
			"interaction source-edge provenance is incomplete",
		)
	}
	references, err := record.Session.EvidenceReferences()
	if err != nil {
		return "", err
	}
	for _, reference := range references {
		if err := e.validateEvidenceReferenceLocked(reference, true); err != nil {
			return "", err
		}
	}
	touched := interaction.TouchedNodes(record.Nodes, record.Edges)
	nodeIDs := append(
		append([]shoal.ID(nil), touched.RetrievedNodeIDs...),
		touched.CitedNodeIDs...,
	)
	nodeVisibility, err := e.conjoinNodeVisibilityLocked(nodeIDs)
	if err != nil {
		return "", err
	}
	resolveEdge := e.edgeVisibilityResolverLocked()
	sets := [][]string{nodeVisibility, record.Session.RequiredVisibility}
	for _, edgeID := range record.Session.TouchedEdgeIDs() {
		labels, err := resolveEdge(edgeID)
		if err != nil {
			return "", err
		}
		sets = append(sets, labels)
	}
	visibility, err := interaction.Conjoin(sets...)
	if err != nil {
		return "", err
	}
	return interaction.Expression(visibility), nil
}

// currentSubgraphVisibilityLocked re-derives, from the current corpus graph,
// the visibility a stored interaction or fold record now requires: the
// conjunction over the current declared labels of every source node it
// retrieved or cited, recovered from the record's own persisted subgraph. It
// fails closed if any touched node can no longer be resolved, exactly as the
// write path does, so a read never serves an under-labelled derived record.
//
// This is sound for folds as well as sessions: at write time a fold's stored
// visibility is the conjunction of every folded session's visibility and every
// touched source label, and each folded session's visibility is itself the
// conjunction over that session's touched labels, which are a subset of the
// fold's touched labels. The touched-label conjunction therefore reproduces the
// stored value when nothing has moved. The caller must hold at least e.mu.RLock
// and the current graph must be initialized.
func (e *Explorer) currentSubgraphVisibilityLocked(
	nodes []graph.Node, edges []graph.Edge,
) (string, error) {
	touched := interaction.TouchedNodes(nodes, edges)
	ids := make(
		[]shoal.ID, 0,
		len(touched.RetrievedNodeIDs)+len(touched.CitedNodeIDs),
	)
	ids = append(ids, touched.RetrievedNodeIDs...)
	ids = append(ids, touched.CitedNodeIDs...)
	labels, err := e.conjoinNodeVisibilityLocked(ids)
	if err != nil {
		return "", err
	}
	return interaction.Expression(labels), nil
}

// conjoinNodeVisibilityLocked resolves every node's current declared visibility
// and returns their conjunction, failing closed on the first node that cannot
// be resolved. The caller must hold at least e.mu.RLock.
func (e *Explorer) conjoinNodeVisibilityLocked(
	ids []shoal.ID,
) ([]string, error) {
	resolve := e.visibilityResolverLocked()
	sets := make([][]string, 0, len(ids))
	for _, id := range ids {
		labels, err := resolve(id)
		if err != nil {
			return nil, err
		}
		normalized, err := interaction.Conjoin(labels)
		if err != nil {
			return nil, err
		}
		sets = append(sets, normalized)
	}
	return interaction.Conjoin(sets...)
}

// staleDerivedVisibilityError is the fail-closed refusal returned by a fold or
// interaction read path when a live record's stored visibility no longer covers
// the visibility its touched source nodes require now. Serving the stored value
// would disclose a record whose evidence was reclassified to a stricter label
// after the record was written; see issue #273.
func staleDerivedVisibilityError() error {
	return shoal.NewError(
		shoal.ErrorUnavailable,
		"derived record visibility no longer covers current source labels; "+
			"its evidence was reclassified to a stricter label after the record "+
			"was written, so the record must be refolded or re-recorded before "+
			"it can be served",
	)
}

// visibilityCovered reports whether a record's stored visibility still
// dominates the visibility its evidence requires now: every label the current
// source labels demand is already carried by the stored expression. When true,
// no reader authorized by the stored (served) visibility is one the current
// labels would deny, so the record is safe to serve even though its evidence
// moved. Visibility is a pure conjunction of labels, so domination is exactly
// current-label-set ⊆ stored-label-set.
//
// A tightened source introduces a label the stored visibility lacks and is not
// covered: the issue #273 fail-open, still refused. A loosened or declassified
// source only drops labels and stays covered, so the record keeps being served
// under its stricter stored visibility instead of becoming permanently
// unreadable. Comparison is by authorization semantics, never string equality,
// but it is strictly narrower than "any mismatch": a new label always refuses.
func visibilityCovered(storedExpr, currentExpr string) bool {
	stored, err := interaction.ParseVisibility(storedExpr)
	if err != nil {
		return false
	}
	current, err := interaction.ParseVisibility(currentExpr)
	if err != nil {
		return false
	}
	storedSet := make(map[string]struct{}, len(stored))
	for _, label := range stored {
		storedSet[label] = struct{}{}
	}
	for _, label := range current {
		if _, ok := storedSet[label]; !ok {
			return false
		}
	}
	return true
}

// acquireReadWithGraph takes a read lock with the current graph guaranteed to
// be built, so concurrent readers of interactions, folds and provenance run in
// parallel. The current graph is never de-initialized once built, so only the
// first reader after an open or reopen pays the one-time build under the write
// lock; every reader after that proceeds directly under the read lock. On a nil
// return the caller holds e.mu.RLock and must release it; on a non-nil error no
// lock is held.
func (e *Explorer) acquireReadWithGraph() error {
	e.mu.RLock()
	if err := e.requireOpen(); err != nil {
		e.mu.RUnlock()
		return err
	}
	if e.graphInitialized {
		if e.graphErr != nil {
			err := e.graphErr
			e.mu.RUnlock()
			return err
		}
		return nil
	}
	e.mu.RUnlock()

	// First reader after open: build the current graph once under the write
	// lock. A concurrent reader may win this race; ensureGraphLocked then
	// short-circuits on the already-set graphInitialized flag.
	e.mu.Lock()
	if err := e.requireOpen(); err != nil {
		e.mu.Unlock()
		return err
	}
	if err := e.ensureGraphLocked(); err != nil {
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()

	// Re-acquire the read lock for the read itself. The graph is only ever
	// built, never torn down, so it is still initialized here even if another
	// goroutine rebuilt it (for example an interleaved ingest) in between.
	e.mu.RLock()
	if err := e.requireOpen(); err != nil {
		e.mu.RUnlock()
		return err
	}
	if e.graphErr != nil {
		err := e.graphErr
		e.mu.RUnlock()
		return err
	}
	return nil
}

func (e *Explorer) requireWritableLocked() error {
	if e.readOnly {
		return shoal.NewError(
			shoal.ErrorUnavailable, "corpus is open read-only")
	}
	return nil
}

func validatePersistedInteraction(record persistedInteraction) error {
	if err := shoal.ValidateRequiredID(
		"interaction session ID", record.SessionID,
	); err != nil {
		return err
	}
	if record.RecordedAt.IsZero() {
		return shoal.NewError(
			shoal.ErrorInternal, "stored interaction time is missing")
	}
	if record.Operation != "" {
		if err := record.Operation.Validate(); err != nil {
			return err
		}
	}
	if err := record.Actor.Validate(); err != nil {
		return err
	}
	if err := record.Reason.Validate(); err != nil {
		return err
	}
	if _, err := interaction.ParseVisibility(record.Visibility); err != nil {
		return err
	}
	if record.Deleted && record.DeletedAt.IsZero() {
		return shoal.NewError(
			shoal.ErrorInternal, "stored interaction deletion time is missing")
	}
	if !record.Deleted && record.Session.ID != "" {
		if err := record.Session.Validate(); err != nil {
			return err
		}
		if record.Session.ID != record.SessionID ||
			!record.Session.RecordedAt.UTC().Equal(record.RecordedAt.UTC()) {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored interaction typed session does not match its envelope",
			)
		}
		if record.Session.SnapshotID != record.SnapshotID ||
			!record.Session.SnapshotAsOf.UTC().Equal(record.SnapshotAsOf.UTC()) ||
			record.Session.AuthorizationFingerprint !=
				record.AuthorizationFingerprint ||
			!record.Session.AuthorizationExpiresAt.UTC().Equal(
				record.AuthorizationExpiresAt.UTC()) ||
			record.Session.AuthorizationOperation !=
				record.AuthorizationOperation ||
			record.Session.OntologySchemaID != record.OntologySchemaID ||
			record.Session.OntologyVersionID != record.OntologyVersionID ||
			record.Session.EmbeddingSpaceID != record.EmbeddingSpaceID {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored interaction execution pins do not match its envelope",
			)
		}
		if record.Session.Operation != record.Operation ||
			record.Session.Actor.SubjectID != record.Actor.SubjectID ||
			record.Session.Actor.ActorID != record.Actor.ActorID ||
			record.Session.Actor.ClientID != record.Actor.ClientID ||
			!equalIDs(
				record.Session.Actor.OnBehalfOf, record.Actor.OnBehalfOf) ||
			record.Session.Reason != record.Reason {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored interaction actor metadata does not match its envelope",
			)
		}
		if !visibilityCovered(
			record.Visibility,
			interaction.Expression(record.Session.RequiredVisibility),
		) {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored interaction visibility omits its required output restriction",
			)
		}
	}
	if len(record.Nodes) == 0 {
		return shoal.NewError(
			shoal.ErrorInternal, "stored interaction has no nodes")
	}
	for _, node := range record.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
		if !interaction.IsInteractionKind(node.Kind) {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored interaction record contains a non-interaction node",
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
				"stored interaction record contains a non-interaction edge",
			)
		}
	}
	return nil
}

func cloneInteractionSession(session interaction.Session) interaction.Session {
	cloned := session
	cloned.Actor = cloneActorContext(session.Actor)
	cloned.EmbeddingSpaces.Identities = append(
		[]string(nil), session.EmbeddingSpaces.Identities...)
	cloned.RequiredVisibility = append(
		[]string(nil), session.RequiredVisibility...)
	cloned.SeedNodeIDs = append([]shoal.ID(nil), session.SeedNodeIDs...)
	cloned.SeedEvidence = cloneEvidenceReferences(session.SeedEvidence)
	cloned.CitedNodeIDs = append([]shoal.ID(nil), session.CitedNodeIDs...)
	cloned.CitedEvidence = cloneEvidenceReferences(session.CitedEvidence)
	cloned.Turns = make([]interaction.Turn, len(session.Turns))
	for index, turn := range session.Turns {
		cloned.Turns[index] = turn
		if turn.ToolCall != nil {
			call := *turn.ToolCall
			call.RetrievedNodeIDs = append(
				[]shoal.ID(nil), turn.ToolCall.RetrievedNodeIDs...)
			call.RetrievedEvidence = cloneEvidenceReferences(
				turn.ToolCall.RetrievedEvidence)
			cloned.Turns[index].ToolCall = &call
		}
	}
	return cloned
}

func cloneEvidenceReferences(
	references []interaction.EvidenceReference,
) []interaction.EvidenceReference {
	if len(references) == 0 {
		return nil
	}
	cloned := make([]interaction.EvidenceReference, len(references))
	for index, reference := range references {
		cloned[index] = reference
		cloned[index].NodeIDs = append(
			[]shoal.ID(nil), reference.NodeIDs...)
		cloned[index].EdgeIDs = append(
			[]shoal.ID(nil), reference.EdgeIDs...)
		cloned[index].Assertions = append(
			[]interaction.AssertionReference(nil), reference.Assertions...)
	}
	return cloned
}

func interactionSummary(record persistedInteraction) InteractionSummary {
	summary := InteractionSummary{
		SessionID:                record.SessionID,
		RecordedAt:               record.RecordedAt,
		SnapshotID:               record.SnapshotID,
		SnapshotAsOf:             record.SnapshotAsOf,
		AuthorizationFingerprint: record.AuthorizationFingerprint,
		AuthorizationExpiresAt:   record.AuthorizationExpiresAt,
		AuthorizationOperation:   record.AuthorizationOperation,
		OntologySchemaID:         record.OntologySchemaID,
		OntologyVersionID:        record.OntologyVersionID,
		EmbeddingSpaceID:         record.EmbeddingSpaceID,
		EmbeddingSpaceDigest:     record.Session.EmbeddingSpaces.Digest,
		EmbeddingSpaceCount:      len(record.Session.EmbeddingSpaces.Identities),
		Operation:                record.Operation,
		Actor:                    cloneActorContext(record.Actor),
		Reason:                   record.Reason,
		CorrelationID:            record.Session.CorrelationID,
		Visibility:               record.Visibility,
		NodeCount:                len(record.Nodes),
		EdgeCount:                len(record.Edges),
		Deleted:                  record.Deleted,
		DeletedAt:                record.DeletedAt,
	}
	if record.Operation.HasInference() {
		summary.InferenceID = interaction.InferenceID(record.SessionID)
	}
	return summary
}

func interactionRecord(record persistedInteraction) InteractionRecord {
	touched := interaction.TouchedNodes(record.Nodes, record.Edges)
	ids := append(
		append([]shoal.ID(nil), touched.RetrievedNodeIDs...),
		touched.CitedNodeIDs...,
	)
	return InteractionRecord{
		Summary:        interactionSummary(record),
		Session:        cloneInteractionSession(record.Session),
		TouchedNodeIDs: dedupeExplorerIDs(ids),
		TouchedEdgeIDs: record.Session.TouchedEdgeIDs(),
	}
}

func dedupeExplorerIDs(ids []shoal.ID) []shoal.ID {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[shoal.ID]struct{}, len(ids))
	result := make([]shoal.ID, 0, len(ids))
	for _, id := range ids {
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

func cloneActorContext(actor interaction.ActorContext) interaction.ActorContext {
	actor.OnBehalfOf = append([]shoal.ID(nil), actor.OnBehalfOf...)
	return actor
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
