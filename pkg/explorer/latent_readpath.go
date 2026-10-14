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
	"sort"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// DefaultMaxLatentDerivedAssertionsPerGraphRead is the explicit safety
	// bound for projecting compaction-materialized latent links into one graph
	// read. Exceeding it is an error, never silent truncation.
	DefaultMaxLatentDerivedAssertionsPerGraphRead uint32 = 4096

	latentAssertionEdgePropertyOrigin       = "ontology.assertion.origin"
	latentAssertionEdgePropertyAssertionID  = "ontology.assertion.id"
	latentAssertionEdgePropertyDerivationID = "ontology.assertion.derivation.id"

	producerPropertyProvider           = "ontology.producer.provider"
	producerPropertyModel              = "ontology.producer.model"
	producerPropertyModelVersion       = "ontology.producer.model_version"
	producerPropertyPrompt             = "ontology.producer.prompt"
	producerPropertyPromptVersion      = "ontology.producer.prompt_version"
	producerPropertyExtractor          = "ontology.producer.extractor"
	producerPropertyExtractorVersion   = "ontology.producer.extractor_version"
	producerPropertyProvenanceMetadata = "ontology.producer.provenance.metadata"
	producerPropertyEmbeddingModel     = "ontology.producer.embedding_model"
	producerPropertyEmbeddingVersion   = "ontology.producer.embedding_model_version"
	producerPropertySimilarityMetric   = "ontology.producer.similarity_metric"
	producerPropertyThreshold          = "ontology.producer.threshold"
	producerPropertyTessellationCell   = "ontology.producer.tessellation_cell"
	producerPropertyIteratorName       = "ontology.producer.iterator_name"
	producerPropertyIteratorOptions    = "ontology.producer.iterator_options"
)

// DefaultLatentLinkAssertionProjection returns the built-in ontology identity
// used when a corpus has latent link cells but no caller-supplied projection.
func DefaultLatentLinkAssertionProjection() (LatentLinkAssertionProjection, error) {
	concept, err := ontology.NewConceptDefinition(
		"latent-node", "Latent Node", "Node eligible for latent linking", nil, nil)
	if err != nil {
		return LatentLinkAssertionProjection{}, err
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"latent-similar-to",
		"Latent Similar To",
		"Embedding similarity above the configured latent-edge threshold",
		[]shoal.ID{concept.ID()},
		[]shoal.ID{concept.ID()},
		nil,
		false,
		nil,
	)
	if err != nil {
		return LatentLinkAssertionProjection{}, err
	}
	provenance, err := ontology.NewExtractionProvenance(
		"shoal",
		"latent-edge-projection",
		"v1",
		"latent-link-projection",
		"v1",
		"ProjectLatentLinkAssertions",
		"v1",
		nil,
	)
	if err != nil {
		return LatentLinkAssertionProjection{}, err
	}
	return LatentLinkAssertionProjection{
		EmbeddingModel:        "unknown-embedding-model",
		EmbeddingModelVersion: "unknown-embedding-version",
		SimilarityThreshold:   0.85,
		Predicate:             relationship.ID(),
		SubjectType:           concept.ID(),
		ObjectType:            concept.ID(),
		Provenance:            provenance,
		AssertionMetadata:     shoal.Metadata{"projection": "latent-link"},
		EvidenceMetadata:      shoal.Metadata{"source": "latent-edge"},
	}, nil
}

func latentLinkProjectionForOptions(
	projection LatentLinkAssertionProjection,
) (LatentLinkAssertionProjection, error) {
	if projection.Predicate == "" &&
		projection.EmbeddingModel == "" &&
		projection.Provenance.Provider() == "" {
		var err error
		projection, err = DefaultLatentLinkAssertionProjection()
		if err != nil {
			return LatentLinkAssertionProjection{}, err
		}
	}
	return normalizeLatentProjection(projection)
}

// PutLatentLinkCells persists link cells emitted by latent-edge compaction so
// graph reads can expose them as authorized derived ontology assertions.
func (e *Explorer) PutLatentLinkCells(
	ctx context.Context,
	cells []LatentLinkCell,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if len(cells) == 0 {
		return nil
	}
	mutations := make([]*cclient.Mutation, 0, len(cells))
	for _, cell := range cells {
		if len(cell.Row) == 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "latent link cell row is required")
		}
		if cell.Timestamp == cclient.MutationLatestTimestamp {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"latent link cell timestamp must be source-derived and explicit",
			)
		}
		mutation, err := cclient.NewMutation(cell.Row)
		if err != nil {
			return shoal.WrapError(
				shoal.ErrorInvalidArgument, "create latent link mutation", err)
		}
		if cell.Deleted {
			mutation.Delete(
				cell.ColumnFamily,
				cell.ColumnQualifier,
				cell.ColumnVisibility,
				cell.Timestamp,
			)
		} else {
			mutation.Put(
				cell.ColumnFamily,
				cell.ColumnQualifier,
				cell.ColumnVisibility,
				cell.Timestamp,
				cell.Value,
			)
		}
		mutations = append(mutations, mutation)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return err
	}
	if err := e.requireWritableLocked(); err != nil {
		return err
	}
	if err := e.engine.Write(explorerTable, mutations); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "write latent link cells", err)
	}
	e.graphInitialized = false
	return nil
}

func (e *Explorer) latentLinkAssertionsLocked() ([]ontology.Assertion, error) {
	if e.maxLatentAssertions == 0 {
		return nil, nil
	}
	cells, err := e.scanLatentLinkCellsLocked()
	if err != nil {
		return nil, err
	}
	assertions, err := ProjectLatentLinkAssertions(cells, e.latentLinkProjection)
	if err != nil {
		return nil, err
	}
	if uint32(len(assertions)) > e.maxLatentAssertions {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"latent-derived assertion count exceeds the graph read bound",
		)
	}
	return assertions, nil
}

func (e *Explorer) scanLatentLinkCellsLocked() ([]LatentLinkCell, error) {
	linkCF := []byte(e.latentLinkProjection.LinkColumnFamily)
	scanner, err := e.engine.Scan(explorerTable, iterrt.InfiniteRange(), engine.ScanOptions{
		ColumnFamilies:          [][]byte{linkCF},
		ColumnFamiliesInclusive: true,
		Stack: []iterrt.IterSpec{
			{
				Name: iterrt.IterDeleting,
				Options: map[string]string{
					iterrt.DeletingOptionPropagate: "false",
				},
			},
			// Load-bearing: TestLatentLinkGraphReadSkipsTombstonedCells pins
			// that resolved graph reads cannot resurrect an older live link
			// hidden by a newer tombstone.
			{Name: iterrt.IterVersioning},
		},
	})
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "scan latent link cells", err)
	}
	defer scanner.Close()

	cells := make([]LatentLinkCell, 0)
	for scanner.Next() {
		if uint32(len(cells)) == e.maxLatentAssertions {
			// Load-bearing: TestLatentLinkGraphReadErrorsAtExplicitCap pins
			// that abundant latent-derived edges fail closed instead of
			// returning a silently truncated assertion set.
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"latent-derived assertion count exceeds the graph read bound",
			)
		}
		key := scanner.Key()
		cells = append(cells, LatentLinkCell{
			Row:              append([]byte(nil), key.Row...),
			ColumnFamily:     append([]byte(nil), key.ColumnFamily...),
			ColumnQualifier:  append([]byte(nil), key.ColumnQualifier...),
			ColumnVisibility: append([]byte(nil), key.ColumnVisibility...),
			Timestamp:        key.Timestamp,
			Deleted:          key.Deleted,
			Value:            append([]byte(nil), scanner.Value()...),
		})
		if err := scanner.Advance(); err != nil {
			return nil, shoal.WrapError(
				shoal.ErrorInternal, "advance latent link scan", err)
		}
	}
	return cells, nil
}

func latentAssertionGraphEdge(
	assertion ontology.Assertion,
) (graph.Edge, bool, error) {
	if assertion.Origin() != ontology.AssertionDerived {
		return graph.Edge{}, false, nil
	}
	target, ok := assertion.Object().ReferenceValue()
	if !ok {
		return graph.Edge{}, false, nil
	}
	evidence := assertion.Evidence()
	if len(evidence) != 1 {
		return graph.Edge{}, false, nil
	}
	derivation, ok := evidence[0].Derivation()
	if !ok {
		return graph.Edge{}, false, nil
	}
	edge := graph.Edge{
		ID:     assertion.ID(),
		From:   assertion.Subject(),
		To:     target,
		Type:   string(assertion.Predicate()),
		Weight: assertion.Confidence(),
		Properties: shoal.Metadata{
			latentAssertionEdgePropertyOrigin:       string(assertion.Origin()),
			latentAssertionEdgePropertyAssertionID:  string(assertion.ID()),
			latentAssertionEdgePropertyDerivationID: string(derivation.ID()),
			"ontology.assertion.derivation.score": strconv.FormatFloat(
				float64(derivation.Score()), 'g', -1, 64),
		},
	}
	if err := edge.Validate(); err != nil {
		return graph.Edge{}, false, err
	}
	return edge, true, nil
}

// ProducerGraphElementsForAssertion reconstructs the canonical provenance
// nodes and produced edge for a derived assertion from its trusted derivation
// record. Callers must compare all three values exactly before accepting a
// projection supplied by an untrusted graph reader.
func ProducerGraphElementsForAssertion(
	assertion ontology.Assertion,
) (graph.Node, graph.Node, graph.Edge, bool, error) {
	if assertion.Origin() != ontology.AssertionDerived {
		return graph.Node{}, graph.Node{}, graph.Edge{}, false, nil
	}
	target, ok := assertion.Object().ReferenceValue()
	if !ok {
		return graph.Node{}, graph.Node{}, graph.Edge{}, false, nil
	}
	derivation, ok := derivedAssertionDerivation(assertion)
	if !ok {
		return graph.Node{}, graph.Node{}, graph.Edge{}, false, nil
	}
	producer, err := producerGraphNode(assertion, derivation)
	if err != nil {
		return graph.Node{}, graph.Node{}, graph.Edge{}, false, err
	}
	assertionNode := graph.Node{
		ID:     assertion.ID(),
		Kind:   graph.NodeKindDerivedAssertion,
		Labels: []string{string(assertion.Predicate())},
		Properties: shoal.Metadata{
			latentAssertionEdgePropertyOrigin:       string(assertion.Origin()),
			latentAssertionEdgePropertyAssertionID:  string(assertion.ID()),
			latentAssertionEdgePropertyDerivationID: string(derivation.ID()),
			"ontology.assertion.subject":            string(assertion.Subject()),
			"ontology.assertion.predicate":          string(assertion.Predicate()),
			"ontology.assertion.object.reference":   string(target),
		},
	}
	if err := assertionNode.Validate(); err != nil {
		return graph.Node{}, graph.Node{}, graph.Edge{}, false, err
	}
	edgeID := shoal.ID(stableID(
		"edge", string(producer.ID), graph.EdgeTypeProduced, string(assertion.ID())))
	derivationEdge := graph.Edge{
		ID:     edgeID,
		From:   producer.ID,
		To:     assertion.ID(),
		Type:   graph.EdgeTypeProduced,
		Weight: 1,
		Properties: shoal.Metadata{
			latentAssertionEdgePropertyAssertionID:  string(assertion.ID()),
			latentAssertionEdgePropertyDerivationID: string(derivation.ID()),
		},
	}
	if err := derivationEdge.Validate(); err != nil {
		return graph.Node{}, graph.Node{}, graph.Edge{}, false, err
	}
	return producer, assertionNode, derivationEdge, true, nil
}

// DerivedAssertions returns canonical derived assertions by assertion ID from
// the current graph. Missing or non-derived IDs are omitted so callers can
// fail closed without learning whether another assertion kind exists.
func (e *Explorer) DerivedAssertions(
	ctx context.Context, ids []shoal.ID,
) (map[shoal.ID]ontology.Assertion, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := e.acquireReadWithGraph(); err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()
	result := make(map[shoal.ID]ontology.Assertion, len(ids))
	for _, id := range ids {
		if err := shoal.ValidateRequiredID(
			"derived assertion ID", id); err != nil {
			return nil, err
		}
		assertion, ok := e.graphAssertions[id]
		if !ok || assertion.ID() != id ||
			assertion.Origin() != ontology.AssertionDerived {
			continue
		}
		result[id] = assertion
	}
	return result, nil
}

func producerGraphNode(
	assertion ontology.Assertion,
	derivation ontology.AssertionDerivation,
) (graph.Node, error) {
	provenance := assertion.Provenance()
	threshold := strconv.FormatFloat(float64(derivation.Threshold()), 'g', -1, 64)
	identity := []string{
		producerPropertyProvider, provenance.Provider(),
		producerPropertyModel, provenance.Model(),
		producerPropertyModelVersion, provenance.ModelVersion(),
		producerPropertyPrompt, provenance.Prompt(),
		producerPropertyPromptVersion, provenance.PromptVersion(),
		producerPropertyExtractor, provenance.Extractor(),
		producerPropertyExtractorVersion, provenance.ExtractorVersion(),
		producerPropertyEmbeddingModel, derivation.EmbeddingModel(),
		producerPropertyEmbeddingVersion, derivation.EmbeddingModelVersion(),
		producerPropertySimilarityMetric, derivation.SimilarityMetric(),
		producerPropertyThreshold, threshold,
		producerPropertyTessellationCell, derivation.TessellationCell(),
		producerPropertyIteratorName, derivation.IteratorName(),
	}
	// Load-bearing: TestProducerGraphNodeIDCanonicalizesMetadataAndOptions
	// pins that map iteration order cannot influence producer identity.
	identity = appendMetadataIdentityParts(
		identity, producerPropertyProvenanceMetadata, provenance.Metadata())
	// Load-bearing: TestProducerGraphNodeIDCanonicalizesMetadataAndOptions
	// pins that iteratorOptions are sorted before they feed a graph node ID.
	identity = appendMetadataIdentityParts(
		identity, producerPropertyIteratorOptions, derivation.IteratorOptions())
	id := shoal.ID(stableID("producer", identity...))
	node := graph.Node{
		ID:     id,
		Kind:   graph.NodeKindProducer,
		Labels: []string{provenance.Extractor(), derivation.IteratorName()},
		Properties: shoal.Metadata{
			producerPropertyProvider:           provenance.Provider(),
			producerPropertyModel:              provenance.Model(),
			producerPropertyModelVersion:       provenance.ModelVersion(),
			producerPropertyPrompt:             provenance.Prompt(),
			producerPropertyPromptVersion:      provenance.PromptVersion(),
			producerPropertyExtractor:          provenance.Extractor(),
			producerPropertyExtractorVersion:   provenance.ExtractorVersion(),
			producerPropertyProvenanceMetadata: metadataIdentityString(provenance.Metadata()),
			producerPropertyEmbeddingModel:     derivation.EmbeddingModel(),
			producerPropertyEmbeddingVersion:   derivation.EmbeddingModelVersion(),
			producerPropertySimilarityMetric:   derivation.SimilarityMetric(),
			producerPropertyThreshold:          threshold,
			producerPropertyTessellationCell:   derivation.TessellationCell(),
			producerPropertyIteratorName:       derivation.IteratorName(),
			producerPropertyIteratorOptions:    metadataIdentityString(derivation.IteratorOptions()),
		},
	}
	if err := node.Validate(); err != nil {
		return graph.Node{}, err
	}
	return node, nil
}

func derivedAssertionDerivation(
	assertion ontology.Assertion,
) (ontology.AssertionDerivation, bool) {
	evidence := assertion.Evidence()
	if len(evidence) != 1 {
		return ontology.AssertionDerivation{}, false
	}
	return evidence[0].Derivation()
}

func appendMetadataIdentityParts(
	parts []string,
	name string,
	metadata shoal.Metadata,
) []string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts = append(parts, name, strconv.Itoa(len(keys)))
	for _, key := range keys {
		parts = append(parts, key, metadata[key])
	}
	return parts
}

func metadataIdentityString(metadata shoal.Metadata) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString(strconv.Itoa(len(keys)))
	for _, key := range keys {
		builder.WriteByte('|')
		builder.WriteString(strconv.Itoa(len(key)))
		builder.WriteByte(':')
		builder.WriteString(key)
		value := metadata[key]
		builder.WriteByte('=')
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
	}
	return builder.String()
}

func (e *Explorer) assertionsForEdgesLocked(
	edges map[shoal.ID]graph.Edge,
) []ontology.Assertion {
	seen := make(map[shoal.ID]struct{}, len(edges))
	ids := make([]shoal.ID, 0, len(edges))
	for id := range edges {
		if _, ok := e.graphAssertions[id]; ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
			continue
		}
		edge := e.graphEdges[id]
		if edge.Type != graph.EdgeTypeProduced {
			continue
		}
		assertionID, ok := edge.Properties[latentAssertionEdgePropertyAssertionID]
		if !ok {
			continue
		}
		id := shoal.ID(assertionID)
		if _, ok := e.graphAssertions[id]; !ok {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sortIDs(ids)
	assertions := make([]ontology.Assertion, 0, len(ids))
	for _, id := range ids {
		assertions = append(assertions, e.graphAssertions[id])
	}
	return assertions
}

func sortIDs(ids []shoal.ID) {
	sort.Slice(ids, func(left, right int) bool {
		return shoal.CompareID(ids[left], ids[right]) < 0
	})
}
