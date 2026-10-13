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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const extractionSourceEdgeType = "extracted_entity"

// ExtractionRequest selects one already-ingested document revision for an
// explicit ontology-constrained extraction pass.
type ExtractionRequest struct {
	DocumentID      shoal.ID
	RevisionID      shoal.ID
	Version         ontology.OntologyVersion
	Instructions    string
	EntityNamespace string
	Limits          extraction.Limits
	Generator       model.TextGenerator
	// ContextNodes, when set, selects which existing entity nodes (given in
	// ascending ID order) the extraction may see as graph context and ground
	// entities on. It returns the visible subset. An authorizing caller sets
	// it to what its principal may read, so neither the generator's prompt
	// nor which entities the result reports as reused depends on a document
	// that principal cannot read (#700). It runs without the explorer's lock
	// held.
	ContextNodes func(context.Context, []shoal.ID) ([]shoal.ID, error)
}

// ExtractionResult describes the graph publication performed by an explicit
// extraction action.
type ExtractionResult struct {
	DocumentID          shoal.ID
	RevisionID          shoal.ID
	ExtractionID        shoal.ID
	EntityCount         int
	RelationCount       int
	GraphNodeCount      int
	GraphEdgeCount      int
	CreatedEntities     int
	ReusedEntities      int
	EntityNodeIDs       []shoal.ID
	RelationshipEdgeIDs []shoal.ID
	GraphNodes          []graph.Node
	GraphEdges          []graph.Edge
	Snapshot            Snapshot
	Plan                extraction.PublicationPlan
	record              persistedExtraction
}

type persistedExtraction struct {
	ID                shoal.ID
	DocumentID        shoal.ID
	RevisionID        shoal.ID
	OntologySchemaID  shoal.ID
	OntologyVersionID shoal.ID
	Nodes             []graph.Node
	Edges             []graph.Edge
	Assertions        []persistedExtractionAssertion
	PublishedAt       time.Time
}

func (e *Explorer) loadExtractionRecord(row, qualifier, encoded []byte) error {
	if !strings.EqualFold(string(qualifier), recordCQV2) {
		return nil
	}
	var record persistedExtraction
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordExtraction, &record,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode explorer extraction", err)
	}
	if err := validatePersistedExtraction(record); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored explorer extraction is invalid", err)
	}
	if string(row) != string(extractionRecordRow(record.ID)) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored explorer extraction row is invalid")
	}
	// ExtractDocument rewrites this stable row in place with PutLatest, so
	// every version of an extraction is the same full key and load's raw
	// scan sees all of them. The engine surfaces identical keys newest
	// first (memtable order within a source, source recency across files;
	// see iterrt.MergingIterator), so the FIRST version seen is the latest
	// publication. Later ones are superseded (#692).
	if _, loaded := e.extractions[record.ID]; loaded {
		return nil
	}
	copy := record
	e.extractions[record.ID] = &copy
	return nil
}

func validatePersistedExtraction(record persistedExtraction) error {
	for name, id := range map[string]shoal.ID{
		"extraction ID":                  record.ID,
		"extraction document ID":         record.DocumentID,
		"extraction revision ID":         record.RevisionID,
		"extraction ontology schema ID":  record.OntologySchemaID,
		"extraction ontology version ID": record.OntologyVersionID,
	} {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
	}
	if record.PublishedAt.IsZero() {
		return fmt.Errorf("published time is missing")
	}
	for _, node := range record.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
		if interaction.IsInteractionID(node.ID) ||
			interaction.IsInteractionKind(node.Kind) {
			return fmt.Errorf(
				"extraction cannot use the reserved interaction node namespace")
		}
	}
	nodes := make(map[shoal.ID]struct{}, len(record.Nodes)+1)
	nodes[record.DocumentID] = struct{}{}
	for _, node := range record.Nodes {
		nodes[node.ID] = struct{}{}
	}
	for _, edge := range record.Edges {
		if err := edge.Validate(); err != nil {
			return err
		}
		if interaction.IsInteractionID(edge.ID) ||
			interaction.IsInteractionEdgeType(edge.Type) {
			return fmt.Errorf(
				"extraction cannot use the reserved interaction edge namespace")
		}
		if _, ok := nodes[edge.From]; !ok {
			return fmt.Errorf("extraction edge source is outside the extraction graph")
		}
		if _, ok := nodes[edge.To]; !ok {
			return fmt.Errorf("extraction edge target is outside the extraction graph")
		}
	}
	for _, assertion := range record.Assertions {
		if _, err := restoreAssertion(assertion); err != nil {
			return err
		}
	}
	return nil
}

// ExtractDocument runs extraction only when explicitly called, then publishes
// the validated publication plan as graph nodes and edges.
func (e *Explorer) ExtractDocument(
	ctx context.Context,
	request ExtractionRequest,
) (ExtractionResult, error) {
	planned, err := e.PlanExtractDocument(ctx, request)
	if err != nil {
		return ExtractionResult{}, err
	}
	return e.CommitExtraction(ctx, planned)
}

// PlanExtractDocument runs extraction and builds the graph publication without
// committing it, so authorized callers can register policy before graph state.
func (e *Explorer) PlanExtractDocument(
	ctx context.Context,
	request ExtractionRequest,
) (ExtractionResult, error) {
	if err := contextError(ctx); err != nil {
		return ExtractionResult{}, err
	}
	if err := request.Version.Validate(); err != nil {
		return ExtractionResult{}, fmt.Errorf("extraction ontology: %w", err)
	}
	if err := shoal.ValidateRequiredID("document ID", request.DocumentID); err != nil {
		return ExtractionResult{}, err
	}
	if err := shoal.ValidateOptionalID("revision ID", request.RevisionID); err != nil {
		return ExtractionResult{}, err
	}
	e.mu.Lock()
	if err := e.requireOpen(); err != nil {
		e.mu.Unlock()
		return ExtractionResult{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		e.mu.Unlock()
		return ExtractionResult{}, err
	}
	record, err := e.documentRecordLocked(request.DocumentID, request.RevisionID)
	if err != nil {
		e.mu.Unlock()
		return ExtractionResult{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		e.mu.Unlock()
		return ExtractionResult{}, err
	}
	contextNodes := e.extractionContextNodesLocked(request.EntityNamespace)
	snapshot := e.snapshot
	e.mu.Unlock()

	if request.ContextNodes != nil && len(contextNodes) > 0 {
		ids := make([]shoal.ID, len(contextNodes))
		for index, node := range contextNodes {
			ids[index] = node.ID
		}
		visible, err := request.ContextNodes(ctx, ids)
		if err != nil {
			return ExtractionResult{}, err
		}
		allowed := make(map[shoal.ID]struct{}, len(visible))
		for _, id := range visible {
			allowed[id] = struct{}{}
		}
		kept := contextNodes[:0]
		for _, node := range contextNodes {
			if _, ok := allowed[node.ID]; ok {
				kept = append(kept, node)
			}
		}
		contextNodes = kept
	}
	pack, err := extractionContext(
		record, request.Version, contextNodes, snapshot)
	if err != nil {
		return ExtractionResult{}, err
	}

	instructions := strings.TrimSpace(request.Instructions)
	if instructions == "" {
		instructions = "Extract ontology entities and relationships grounded in this uploaded document."
	}
	extractionRequest := extraction.Request{
		Version:         request.Version,
		Context:         pack,
		Instructions:    instructions,
		EntityNamespace: request.EntityNamespace,
		Limits:          request.Limits,
	}
	var extractionResult extraction.Result
	if request.Generator != nil {
		extractionResult, err = (extraction.Orchestrator{Generator: request.Generator}).
			Extract(ctx, extractionRequest)
	} else {
		extractionResult, err = (extraction.HeuristicExtractor{ConceptType: heuristicConcept(request.Version)}).
			Extract(ctx, extractionRequest)
	}
	if err != nil {
		return ExtractionResult{}, err
	}
	plan := extractionResult.PublicationPlan()
	published, err := extractionPublication(
		record, request.Version, request.EntityNamespace, plan, extractionResult)
	if err != nil {
		return ExtractionResult{}, err
	}
	return extractionSummaryLocked(plan, published, snapshot), nil
}

// CommitExtraction publishes a previously planned extraction.
func (e *Explorer) CommitExtraction(
	ctx context.Context,
	planned ExtractionResult,
) (ExtractionResult, error) {
	published := planned.record
	if err := validatePersistedExtraction(published); err != nil {
		return ExtractionResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return ExtractionResult{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		return ExtractionResult{}, err
	}
	current, err := e.documentRecordLocked(published.DocumentID, published.RevisionID)
	if err != nil {
		return ExtractionResult{}, err
	}
	if current.Revision.ID != published.RevisionID {
		return ExtractionResult{}, shoal.NewError(
			shoal.ErrorConflict, "document changed before extraction publication")
	}
	if err := e.requireSourceGraphIDsAvailableLocked(
		published.Nodes, published.Edges,
	); err != nil {
		return ExtractionResult{}, err
	}
	// This stable row overwrite is load-bearing; TestExtractDocumentRerunReusesSkillEntities pins idempotent re-publication.
	if err := e.writeRecord(
		extractionRecordRow(published.ID), embeddedRecordExtraction, published,
	); err != nil {
		return ExtractionResult{}, err
	}
	copy := published
	e.extractions[published.ID] = &copy
	if e.graphInitialized {
		// The extraction record is durable, so a rebuild failure is
		// indeterminate (#687), keeping its code.
		if err := e.rebuildCurrentGraphLocked(); err != nil {
			return ExtractionResult{}, MarkIndeterminateCommit(err)
		}
	}
	return extractionSummaryLocked(planned.Plan, published, e.snapshot), nil
}

func (e *Explorer) documentRecordLocked(
	documentID, revisionID shoal.ID,
) (*persistedDocument, error) {
	revisions := e.documents[documentID]
	if revisionID == "" {
		return latestRevision(revisions)
	}
	if record := revisions[revisionID]; record != nil {
		return record, nil
	}
	return nil, shoal.NewError(shoal.ErrorNotFound, "document revision not found")
}

// extractionContextNodesLocked returns copies of the existing entity nodes of
// entityNamespace, in ascending ID order: the graph context an extraction may
// ground its entities on.
func (e *Explorer) extractionContextNodesLocked(entityNamespace string) []graph.Node {
	nodeIDs := make([]shoal.ID, 0, len(e.graphNodes))
	for id, node := range e.graphNodes {
		if node.Properties[extraction.GraphPropertyEntityKey] == "" ||
			node.Properties[extraction.GraphPropertyOntologyConceptID] == "" {
			continue
		}
		if entityNamespace != "" &&
			node.Properties[extraction.GraphPropertyEntityNamespace] != entityNamespace {
			continue
		}
		// This existing entity context is load-bearing; TestExtractDocumentRerunReusesSkillEntities pins rerun reuse instead of duplicate creation.
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool {
		return shoal.CompareID(nodeIDs[i], nodeIDs[j]) < 0
	})
	nodes := make([]graph.Node, len(nodeIDs))
	for index, id := range nodeIDs {
		nodes[index] = cloneNode(e.graphNodes[id])
	}
	return nodes
}

// extractionContext builds an extraction's context pack from the document
// and the existing entity nodes it may ground on. It reads no explorer state.
func extractionContext(
	record *persistedDocument,
	version ontology.OntologyVersion,
	contextNodes []graph.Node,
	snapshot Snapshot,
) (inference.ContextPack, error) {
	identity, err := inference.NewOntologyIdentity(version)
	if err != nil {
		return inference.ContextPack{}, err
	}
	citation := document.Citation{
		DocumentID: record.Document.ID,
		RevisionID: record.Revision.ID,
		SectionID:  record.Document.RootSectionID,
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0},
			End:   document.SourcePosition{Offset: int64(len(record.Source.Content))},
		},
	}
	anchor, err := inference.NewDocumentAnchor(citation, record.Source.Content)
	if err != nil {
		return inference.ContextPack{}, err
	}
	anchors := []inference.EvidenceAnchor{anchor}
	for _, node := range contextNodes {
		anchor, err := inference.NewGraphAnchor(graph.Path{
			Nodes: []graph.Node{cloneNode(node)},
		})
		if err != nil {
			return inference.ContextPack{}, err
		}
		anchors = append(anchors, anchor)
	}
	snapshotPin, err := inference.NewSnapshotPin(shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		return inference.ContextPack{}, err
	}
	authPin, err := inference.NewAuthPin("embedded-extraction", snapshot.AsOf)
	if err != nil {
		return inference.ContextPack{}, err
	}
	return inference.NewContextPack(
		record.Document.Title, anchors, &identity, snapshotPin, authPin, nil,
	)
}

func extractionPublication(
	source *persistedDocument,
	version ontology.OntologyVersion,
	entityNamespace string,
	plan extraction.PublicationPlan,
	result extraction.Result,
) (persistedExtraction, error) {
	id, err := ontology.NewStableID(
		"document-extraction",
		string(source.Document.ID),
		string(source.Revision.ID),
		string(version.ID()),
	)
	if err != nil {
		return persistedExtraction{}, err
	}
	concepts := conceptsByID(version)
	relationships := relationshipsByID(version)
	nodes := make([]graph.Node, 0, len(plan.Entities))
	for _, entity := range plan.Entities {
		concept := concepts[entity.TypeID]
		node := graph.Node{
			ID:     entity.ID,
			Kind:   "ontology:" + concept.Key(),
			Labels: []string{concept.Key(), entityDisplayName(entity)},
			Properties: shoal.Metadata{
				extraction.GraphPropertyOntologyConceptID:  string(entity.TypeID),
				extraction.GraphPropertyOntologyConceptKey: concept.Key(),
				extraction.GraphPropertyEntityKey:          entity.Key,
				"confidence":                               fmt.Sprintf("%.6f", entity.Confidence),
			},
		}
		if entityNamespace != "" {
			node.Properties[extraction.GraphPropertyEntityNamespace] = entityNamespace
		}
		sort.Strings(node.Labels)
		// A shared entity carries only its identity (#700): every
		// extraction of it, by any extractor, publishes the same node.
		node, _ = CanonicalExtractedNode(node)
		if err := node.Validate(); err != nil {
			return persistedExtraction{}, err
		}
		nodes = append(nodes, node)
	}
	edges := make([]graph.Edge, 0, len(plan.Entities)+len(plan.Edges))
	for _, entity := range plan.Entities {
		edgeID, err := ontology.NewStableID(
			"extraction-source-edge",
			string(source.Document.ID),
			string(source.Revision.ID),
			string(entity.ID),
		)
		if err != nil {
			return persistedExtraction{}, err
		}
		edge := graph.Edge{
			ID: edgeID, From: source.Document.ID, To: entity.ID,
			Type: extractionSourceEdgeType, Weight: entity.Confidence,
			Properties: shoal.Metadata{
				extraction.GraphPropertyEntityKey: entity.Key,
				"ontology_concept_id":             string(entity.TypeID),
			},
		}
		if err := edge.Validate(); err != nil {
			return persistedExtraction{}, err
		}
		edges = append(edges, edge)
	}
	assertions, err := relationshipAssertions(result.OntologyResult().Assertions(), plan.Edges)
	if err != nil {
		return persistedExtraction{}, err
	}
	for _, relation := range plan.Edges {
		relationship := relationships[relation.TypeID]
		edge := graph.Edge{
			ID: relation.ID, From: relation.From, To: relation.To,
			Type: relationship.Key(), Weight: relation.Confidence,
			Properties: shoal.Metadata{
				"ontology_relationship_id": string(relation.TypeID),
			},
		}
		// Likewise a shared relation; its confidence stays on its assertion.
		edge, _ = CanonicalExtractedRelation(edge)
		if err := edge.Validate(); err != nil {
			return persistedExtraction{}, err
		}
		edges = append(edges, edge)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return shoal.CompareID(nodes[i].ID, nodes[j].ID) < 0
	})
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
	return persistedExtraction{
		ID: id, DocumentID: source.Document.ID, RevisionID: source.Revision.ID,
		OntologySchemaID: version.Schema().ID(), OntologyVersionID: version.ID(),
		Nodes: nodes, Edges: edges, Assertions: assertions,
		PublishedAt: source.Revision.CreatedAt.UTC().Add(time.Nanosecond),
	}, nil
}

func relationshipAssertions(
	assertions []ontology.Assertion,
	edges []extraction.Edge,
) ([]persistedExtractionAssertion, error) {
	pending := make(map[string]shoal.ID, len(edges))
	for _, edge := range edges {
		pending[relationshipAssertionKey(edge.FromContractID, edge.TypeID, edge.ToContractID)] = edge.ID
	}
	out := make([]persistedExtractionAssertion, 0, len(edges))
	for _, assertion := range assertions {
		reference, ok := assertion.Object().ReferenceValue()
		if !ok {
			continue
		}
		edgeID, ok := pending[relationshipAssertionKey(assertion.Subject(), assertion.Predicate(), reference)]
		if !ok {
			continue
		}
		persisted, err := persistAssertion(edgeID, assertion)
		if err != nil {
			return nil, err
		}
		out = append(out, persisted)
	}
	sort.Slice(out, func(i, j int) bool {
		return shoal.CompareID(out[i].EdgeID, out[j].EdgeID) < 0
	})
	return out, nil
}

func relationshipAssertionKey(subject, predicate, object shoal.ID) string {
	return string(subject) + "\x00" + string(predicate) + "\x00" + string(object)
}

func extractionSummaryLocked(
	plan extraction.PublicationPlan,
	record persistedExtraction,
	snapshot Snapshot,
) ExtractionResult {
	entityIDs := make([]shoal.ID, 0, len(plan.Entities))
	created, reused := 0, 0
	for _, entity := range plan.Entities {
		entityIDs = append(entityIDs, entity.ID)
		if entity.Action == extraction.ActionReference {
			reused++
		} else {
			created++
		}
	}
	sort.Slice(entityIDs, func(i, j int) bool {
		return shoal.CompareID(entityIDs[i], entityIDs[j]) < 0
	})
	relationshipIDs := make([]shoal.ID, 0, len(plan.Edges))
	for _, edge := range plan.Edges {
		relationshipIDs = append(relationshipIDs, edge.ID)
	}
	sort.Slice(relationshipIDs, func(i, j int) bool {
		return shoal.CompareID(relationshipIDs[i], relationshipIDs[j]) < 0
	})
	return ExtractionResult{
		DocumentID: record.DocumentID, RevisionID: record.RevisionID,
		ExtractionID: record.ID, EntityCount: len(plan.Entities),
		RelationCount: len(plan.Edges), GraphNodeCount: len(record.Nodes),
		GraphEdgeCount: len(record.Edges), CreatedEntities: created,
		ReusedEntities: reused, EntityNodeIDs: entityIDs,
		RelationshipEdgeIDs: relationshipIDs,
		GraphNodes:          cloneNodes(record.Nodes), GraphEdges: cloneEdges(record.Edges),
		Snapshot: snapshot, Plan: plan,
		record: record,
	}
}

func cloneNodes(nodes []graph.Node) []graph.Node {
	out := make([]graph.Node, len(nodes))
	for i := range nodes {
		out[i] = cloneNode(nodes[i])
	}
	return out
}

func cloneEdges(edges []graph.Edge) []graph.Edge {
	out := make([]graph.Edge, len(edges))
	for i := range edges {
		out[i] = cloneEdge(edges[i])
	}
	return out
}

func heuristicConcept(version ontology.OntologyVersion) shoal.ID {
	for _, concept := range version.Concepts() {
		if concept.Key() == "skill" {
			return concept.ID()
		}
	}
	concepts := version.Concepts()
	if len(concepts) == 0 {
		return ""
	}
	return concepts[0].ID()
}

func conceptsByID(version ontology.OntologyVersion) map[shoal.ID]ontology.ConceptDefinition {
	result := make(map[shoal.ID]ontology.ConceptDefinition, len(version.Concepts()))
	for _, concept := range version.Concepts() {
		result[concept.ID()] = concept
	}
	return result
}

func relationshipsByID(version ontology.OntologyVersion) map[shoal.ID]ontology.RelationshipDefinition {
	result := make(map[shoal.ID]ontology.RelationshipDefinition, len(version.Relationships()))
	for _, relationship := range version.Relationships() {
		result[relationship.ID()] = relationship
	}
	return result
}

func entityDisplayName(entity extraction.Entity) string {
	for _, property := range entity.Properties {
		if property.Value.Type() != ontology.ValueString {
			continue
		}
		value, _ := property.Value.StringValue()
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return entity.Key
}

// ExtractionRecord is the provenance of one published extraction: the
// document revision that asserted it and the graph nodes and edges it
// published. The label migration (#570) uses it to find relations a document
// asserted that the policy catalog cannot attribute to it.
type ExtractionRecord struct {
	ID         shoal.ID
	DocumentID shoal.ID
	RevisionID shoal.ID
	NodeIDs    []shoal.ID
	EdgeIDs    []shoal.ID
}

// ExtractionRecords returns every stored extraction record, ordered by ID.
func (e *Explorer) ExtractionRecords(ctx context.Context) ([]ExtractionRecord, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	records := make([]ExtractionRecord, 0, len(e.extractions))
	for _, stored := range e.extractions {
		if stored == nil {
			continue
		}
		record := ExtractionRecord{
			ID:         stored.ID,
			DocumentID: stored.DocumentID,
			RevisionID: stored.RevisionID,
			NodeIDs:    make([]shoal.ID, 0, len(stored.Nodes)),
			EdgeIDs:    make([]shoal.ID, 0, len(stored.Edges)),
		}
		for _, node := range stored.Nodes {
			record.NodeIDs = append(record.NodeIDs, node.ID)
		}
		for _, edge := range stored.Edges {
			record.EdgeIDs = append(record.EdgeIDs, edge.ID)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool {
		return shoal.CompareID(records[left].ID, records[right].ID) < 0
	})
	return records, nil
}

// ExtractionAssertions returns, for each requested edge of the current graph
// that several contributing extractions assert, every one of their
// assertions, in extraction-ID order. The graph keeps one assertion per edge
// (the last such extraction's), and its evidence cites that extraction's
// document. An authorizing caller uses this trusted read to show each reader
// an assertion whose evidence that reader may read (#700). Edges asserted by
// one extraction alone, or by none, are omitted: their graph assertion is
// the only one.
func (e *Explorer) ExtractionAssertions(
	ctx context.Context, edgeIDs []shoal.ID,
) (map[shoal.ID][]ontology.Assertion, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	for _, id := range edgeIDs {
		if err := shoal.ValidateRequiredID("extraction edge ID", id); err != nil {
			return nil, err
		}
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	result := make(map[shoal.ID][]ontology.Assertion)
	for _, id := range edgeIDs {
		if set, ok := e.extractionAssertionSets[id]; ok {
			result[id] = append([]ontology.Assertion(nil), set...)
		}
	}
	return result, nil
}

// extractionAssertionSetsLocked indexes, once per graph build, every
// assertion of each relation edge that more than one contributing
// extraction asserts, under the contribution rule computeCurrentGraph
// applies.
func (e *Explorer) extractionAssertionSetsLocked(
	nodes map[shoal.ID]graph.Node, edges map[shoal.ID]graph.Edge,
) (map[shoal.ID][]ontology.Assertion, error) {
	extractionIDs := make([]shoal.ID, 0, len(e.extractions))
	for id := range e.extractions {
		extractionIDs = append(extractionIDs, id)
	}
	sort.Slice(extractionIDs, func(i, j int) bool {
		return shoal.CompareID(extractionIDs[i], extractionIDs[j]) < 0
	})
	all := make(map[shoal.ID][]ontology.Assertion)
	for _, id := range extractionIDs {
		record := e.extractions[id]
		if record == nil {
			continue
		}
		if _, ok := nodes[record.DocumentID]; !ok {
			continue
		}
		for _, persisted := range record.Assertions {
			if _, ok := edges[persisted.EdgeID]; !ok {
				continue
			}
			assertion, err := restoreAssertion(persisted)
			if err != nil {
				return nil, err
			}
			duplicate := false
			for _, known := range all[persisted.EdgeID] {
				if known.ID() == assertion.ID() {
					duplicate = true
					break
				}
			}
			if !duplicate {
				all[persisted.EdgeID] = append(all[persisted.EdgeID], assertion)
			}
		}
	}
	sets := make(map[shoal.ID][]ontology.Assertion)
	for edgeID, set := range all {
		if len(set) > 1 {
			sets[edgeID] = set
		}
	}
	return sets, nil
}

// edgeAssertionLocked returns the authoritative assertion reference names
// for its edge: the graph's own, or one of a shared relation's.
func (e *Explorer) edgeAssertionLocked(
	reference interaction.AssertionReference,
) (ontology.Assertion, bool) {
	matches := func(assertion ontology.Assertion) bool {
		return assertion.ID() == reference.AssertionID &&
			assertion.Origin() == reference.Origin
	}
	if assertion, ok := e.graphAssertions[reference.EdgeID]; ok && matches(assertion) {
		return assertion, true
	}
	for _, assertion := range e.extractionAssertionSets[reference.EdgeID] {
		if matches(assertion) {
			return assertion, true
		}
	}
	return ontology.Assertion{}, false
}
