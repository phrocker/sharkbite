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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestProducerDerivationEdgeRequiresCanonicalReconstruction(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer corpus.Close()
	source, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///producer-proof-source.txt",
		MediaType: explorer.MediaTypeText, Content: "source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///producer-proof-target.txt",
		MediaType: explorer.MediaTypeText, Content: "target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.PutLatentLinkCells(ctx, []explorer.LatentLinkCell{{
		Row:             []byte("cell-a:" + string(source.Document.ID)),
		ColumnFamily:    []byte("link"),
		ColumnQualifier: []byte(target.Document.ID),
		Timestamp:       1, Value: []byte("0.91"),
	}}); err != nil {
		t.Fatal(err)
	}
	projection, err := explorer.DefaultLatentLinkAssertionProjection()
	if err != nil {
		t.Fatal(err)
	}
	result, err := corpus.BoundedNeighborhood(
		ctx, explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{source.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{string(projection.Predicate)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Neighborhood.Assertions) != 1 {
		t.Fatalf("assertions = %d", len(result.Neighborhood.Assertions))
	}
	assertion := result.Neighborhood.Assertions[0]
	if err := validateTrustedDerivedAssertions(
		map[shoal.ID]ontology.Assertion{assertion.ID(): assertion},
		map[shoal.ID]ontology.Assertion{assertion.ID(): assertion},
	); err != nil {
		t.Fatalf("canonical assertion rejected: %v", err)
	}
	metadata := assertion.Metadata()
	metadata["forged"] = "true"
	options := []ontology.AssertionOption{}
	if subjectType, present := assertion.SubjectType(); present {
		options = append(options, ontology.WithAssertionSubjectType(subjectType))
	}
	if objectType, present := assertion.ObjectType(); present {
		options = append(options, ontology.WithAssertionObjectType(objectType))
	}
	if identity, present := assertion.Ontology(); present {
		options = append(options, ontology.WithAssertionOntology(identity))
	}
	forgedClaim, err := ontology.NewAssertion(
		assertion.Subject(), assertion.Predicate(), assertion.Object(),
		assertion.Origin(), assertion.Confidence(), assertion.Evidence(),
		assertion.Provenance(), metadata, options...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if forgedClaim.ID() != assertion.ID() {
		t.Fatal("fixture metadata unexpectedly changed assertion identity")
	}
	if err := validateTrustedDerivedAssertions(
		map[shoal.ID]ontology.Assertion{assertion.ID(): forgedClaim},
		map[shoal.ID]ontology.Assertion{assertion.ID(): assertion},
	); err == nil {
		t.Fatal("forged assertion metadata was accepted")
	}
	if err := validateTrustedDerivedAssertions(
		map[shoal.ID]ontology.Assertion{assertion.ID(): assertion},
		map[shoal.ID]ontology.Assertion{},
	); err == nil {
		t.Fatal("missing trusted assertion was accepted")
	}
	producer, assertionNode, edge, ok, err :=
		explorer.ProducerGraphElementsForAssertion(assertion)
	if err != nil || !ok {
		t.Fatalf("canonical producer projection = %v, %v", ok, err)
	}
	raw := map[shoal.ID]graph.Node{
		producer.ID: producer, assertionNode.ID: assertionNode,
	}
	if !producerDerivationEdgeMatches(edge, raw, assertion) {
		t.Fatal("canonical producer projection was rejected")
	}
	forged := producer
	forged.Properties = cloneMetadata(forged.Properties)
	forged.Properties["forged"] = "true"
	raw[producer.ID] = forged
	if producerDerivationEdgeMatches(edge, raw, assertion) {
		t.Fatal("forged producer node was accepted")
	}
	raw[producer.ID] = producer
	forgedAssertion := assertionNode
	forgedAssertion.Properties = cloneMetadata(forgedAssertion.Properties)
	forgedAssertion.Properties["forged"] = "true"
	raw[assertionNode.ID] = forgedAssertion
	if producerDerivationEdgeMatches(edge, raw, assertion) {
		t.Fatal("forged derived assertion node was accepted")
	}
	raw[assertionNode.ID] = assertionNode
	forgedEdge := edge
	forgedEdge.Properties = cloneMetadata(forgedEdge.Properties)
	forgedEdge.Properties["forged"] = "true"
	if producerDerivationEdgeMatches(forgedEdge, raw, assertion) {
		t.Fatal("forged produced edge was accepted")
	}
}

func TestTrustedDerivedAssertionComparisonNormalizesEmptyMetadata(t *testing.T) {
	derivation, err := ontology.NewAssertionDerivation(
		"embedding-model", "v1", "cosine", 0.8, "cell-1", 0.9,
		"source", "target", "iterator", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	nilEvidence, err := ontology.NewDerivationEvidenceRef(derivation, nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyEvidence, err := ontology.NewDerivationEvidenceRef(
		derivation, shoal.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	nilProvenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	emptyProvenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1",
		shoal.Metadata{},
	)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"test-node", "Test Node", "A test node", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"test-relation", "Test Relation", "Relates test nodes",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue("target")
	if err != nil {
		t.Fatal(err)
	}
	nilMetadata, err := ontology.NewAssertion(
		"source", relationship.ID(), value, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{nilEvidence}, nilProvenance, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	emptyMetadata, err := ontology.NewAssertion(
		"source", relationship.ID(), value, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{emptyEvidence}, emptyProvenance, shoal.Metadata{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if nilMetadata.ID() != emptyMetadata.ID() {
		t.Fatal("empty metadata changed canonical assertion identity")
	}
	if err := validateTrustedDerivedAssertions(
		map[shoal.ID]ontology.Assertion{nilMetadata.ID(): nilMetadata},
		map[shoal.ID]ontology.Assertion{emptyMetadata.ID(): emptyMetadata},
	); err != nil {
		t.Fatalf("semantic empty metadata comparison = %v", err)
	}
	presentEmpty, err := ontology.NewAssertion(
		"source", relationship.ID(), value, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{nilEvidence}, nilProvenance,
		shoal.Metadata{"present": ""},
	)
	if err != nil {
		t.Fatal(err)
	}
	if presentEmpty.ID() != nilMetadata.ID() {
		t.Fatal("non-identity metadata changed assertion identity")
	}
	if err := validateTrustedDerivedAssertions(
		map[shoal.ID]ontology.Assertion{nilMetadata.ID(): presentEmpty},
		map[shoal.ID]ontology.Assertion{nilMetadata.ID(): nilMetadata},
	); err == nil {
		t.Fatal("present empty metadata key matched an absent key")
	}
}
