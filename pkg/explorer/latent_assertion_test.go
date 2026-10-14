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
	"reflect"
	"strconv"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestProjectLatentLinkAssertionsPreservesDerivationEvidence(t *testing.T) {
	projection := latentProjectionFixture(t)
	cell := latentCell(
		"cell-17:entity:source", "entity:target", "0.9000000000000001")

	assertions, err := ProjectLatentLinkAssertions([]LatentLinkCell{cell}, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 1 {
		t.Fatalf("assertion count = %d, want 1", len(assertions))
	}

	assertion := assertions[0]
	if assertion.Origin() != ontology.AssertionDerived {
		t.Fatalf("origin = %q, want %q", assertion.Origin(), ontology.AssertionDerived)
	}
	if assertion.Subject() != "entity:source" {
		t.Fatalf("subject = %q, want entity:source", assertion.Subject())
	}
	if assertion.Predicate() != projection.Predicate {
		t.Fatalf("predicate = %q, want %q", assertion.Predicate(), projection.Predicate)
	}
	object, ok := assertion.Object().ReferenceValue()
	if !ok || object != "entity:target" {
		t.Fatalf("object = %q,%v want entity:target,true", object, ok)
	}
	if subjectType, ok := assertion.SubjectType(); !ok || subjectType != projection.SubjectType {
		t.Fatalf("subject type = %q,%v want %q,true", subjectType, ok, projection.SubjectType)
	}
	if objectType, ok := assertion.ObjectType(); !ok || objectType != projection.ObjectType {
		t.Fatalf("object type = %q,%v want %q,true", objectType, ok, projection.ObjectType)
	}

	evidence := assertion.Evidence()
	if len(evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(evidence))
	}
	if evidence[0].Citation() != (document.Citation{}) || evidence[0].Quote() != "" {
		t.Fatal("derived link evidence carried citation fields")
	}
	derivation, ok := evidence[0].Derivation()
	if !ok {
		t.Fatal("derived link evidence is not derivation-backed")
	}
	if derivation.EmbeddingModel() != projection.EmbeddingModel ||
		derivation.EmbeddingModelVersion() != projection.EmbeddingModelVersion ||
		derivation.SimilarityMetric() != LatentLinkSimilarityMetric ||
		derivation.Threshold() != projection.SimilarityThreshold ||
		derivation.TessellationCell() != "cell-17" ||
		derivation.SourceEndpoint() != "entity:source" ||
		derivation.TargetEndpoint() != "entity:target" ||
		derivation.IteratorName() != LatentLinkDefaultIteratorName {
		t.Fatalf("derivation did not preserve provenance: %+v", derivation)
	}
	options := derivation.IteratorOptions()
	for key, want := range map[string]string{
		"similarityThreshold": "0.85",
		"maxPairsPerCell":     "512",
		"maxCellBuffer":       "200",
		"edgeCF":              "link",
		"embeddingCF":         "V",
		"embeddingCQ":         "_embedding",
	} {
		if got := options[key]; got != want {
			t.Fatalf("iterator option %s = %q, want %q", key, got, want)
		}
	}
}

func TestProjectLatentLinkAssertionsPreservesExactScore(t *testing.T) {
	projection := latentProjectionFixture(t)
	projection.SimilarityThreshold = 0.8123456789012346
	cell := latentCell(
		"cell-score:entity:source", "entity:target", "0.9123456789012346")

	assertions, err := ProjectLatentLinkAssertions([]LatentLinkCell{cell}, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 1 {
		t.Fatalf("assertion count = %d, want 1", len(assertions))
	}
	derivation, ok := assertions[0].Evidence()[0].Derivation()
	if !ok {
		t.Fatal("missing derivation")
	}
	wantScore, err := strconv.ParseFloat("0.9123456789012346", 64)
	if err != nil {
		t.Fatal(err)
	}
	if derivation.Score() != shoal.Score(wantScore) {
		t.Fatalf("score = %.17g, want %.17g", derivation.Score(), wantScore)
	}
	if assertions[0].Confidence() != shoal.Score(wantScore) {
		t.Fatalf("confidence = %.17g, want %.17g", assertions[0].Confidence(), wantScore)
	}
	if derivation.Threshold() != projection.SimilarityThreshold {
		t.Fatalf("threshold = %.17g, want %.17g",
			derivation.Threshold(), projection.SimilarityThreshold)
	}
}

func TestProjectLatentLinkAssertionsDeterministicOrder(t *testing.T) {
	projection := latentProjectionFixture(t)
	cells := []LatentLinkCell{
		latentCell("cell-b:entity:c", "entity:z", "0.93"),
		latentCell("cell-a:entity:b", "entity:y", "0.92"),
		latentCell("cell-a:entity:a", "entity:x", "0.91"),
	}
	reordered := []LatentLinkCell{cells[2], cells[0], cells[1]}

	first := projectedAssertionIDs(t, projection, cells)
	second := projectedAssertionIDs(t, projection, reordered)
	third := projectedAssertionIDs(t, projection, cells)

	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, third) {
		t.Fatalf("nondeterministic IDs:\nfirst=%v\nsecond=%v\nthird=%v",
			first, second, third)
	}
	subjects := projectedSubjects(t, projection, cells)
	wantSubjects := []shoal.ID{"entity:a", "entity:b", "entity:c"}
	if !reflect.DeepEqual(subjects, wantSubjects) {
		t.Fatalf("subjects = %v, want %v", subjects, wantSubjects)
	}
}

func TestProjectLatentLinkAssertionsSkipsNonLinkCells(t *testing.T) {
	projection := latentProjectionFixture(t)
	assertions, err := ProjectLatentLinkAssertions([]LatentLinkCell{
		{
			Row:             []byte{0xff},
			ColumnFamily:    []byte("V"),
			ColumnQualifier: []byte("_embedding"),
			Value:           []byte("not-a-score"),
		},
		latentCell("cell-a:entity:a", "entity:b", "0.91"),
	}, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 1 || assertions[0].Subject() != "entity:a" {
		t.Fatalf("assertions = %+v, want only projected link cell", assertions)
	}
}

func TestProjectLatentLinkAssertionsSkipsDeletedLinkCells(t *testing.T) {
	projection := latentProjectionFixture(t)
	deleted := latentCell("cell-a:entity:deleted", "entity:target", "0.95")
	deleted.Deleted = true

	assertions, err := ProjectLatentLinkAssertions(
		[]LatentLinkCell{deleted, latentCell("cell-a:entity:live", "entity:target", "0.91")},
		projection,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 1 || assertions[0].Subject() != "entity:live" {
		t.Fatalf("assertions = %+v, want only live link cell", assertions)
	}
}

// TestProjectLatentLinkAssertionsSkipsLabelledCells pins #569: a link cell
// carrying a compound cell visibility is not projected, because nothing past
// the projection carries that label — the derivation, evidence and assertion
// have no visibility, and the resulting graph edge is gated only by its
// endpoints' AccessRules.
//
// An unlabelled cell in the same call is projected, so the refusal turns on
// the visibility and not on the projection having been broken.
func TestProjectLatentLinkAssertionsSkipsLabelledCells(t *testing.T) {
	projection := latentProjectionFixture(t)
	labelled := latentCell("cell-a:entity:labelled", "entity:target", "0.95")
	labelled.ColumnVisibility = []byte("A&B")

	assertions, err := ProjectLatentLinkAssertions(
		[]LatentLinkCell{
			labelled,
			latentCell("cell-a:entity:plain", "entity:target", "0.91"),
		},
		projection,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Named rather than counted: a count alone passes on a projection that
	// dropped the wrong cell, and this is the pair that distinguishes
	// "labelled cells are refused" from "the projection is broken".
	if len(assertions) != 1 || assertions[0].Subject() != "entity:plain" {
		subjects := make([]shoal.ID, len(assertions))
		for i, assertion := range assertions {
			subjects[i] = assertion.Subject()
		}
		t.Fatalf("projected subjects = %v, want only [entity:plain]: a "+
			"labelled relationship projected here reaches a reader "+
			"authorized on both endpoints, which is the #569 leak", subjects)
	}
}

func TestProjectLatentLinkAssertionsPreservesCustomLinkColumnFamily(t *testing.T) {
	projection := latentProjectionFixture(t)
	projection.LinkColumnFamily = "edge.custom:"
	cell := latentCell("cell-a:entity:a", "entity:b", "0.91")
	cell.ColumnFamily = []byte("edge.custom:")

	assertions, err := ProjectLatentLinkAssertions([]LatentLinkCell{cell}, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 1 {
		t.Fatalf("assertion count = %d, want 1", len(assertions))
	}
	derivation, ok := assertions[0].Evidence()[0].Derivation()
	if !ok {
		t.Fatal("missing derivation")
	}
	if got := derivation.IteratorOptions()["edgeCF"]; got != "edge.custom:" {
		t.Fatalf("edgeCF option = %q, want edge.custom:", got)
	}
}

func TestProjectLatentLinkAssertionsRejectsConflictingIteratorOptions(t *testing.T) {
	cell := latentCell("cell-a:entity:a", "entity:b", "0.91")

	thresholdConflict := latentProjectionFixture(t)
	thresholdConflict.IteratorOptions["similarityThreshold"] = "0.86"
	_, err := ProjectLatentLinkAssertions([]LatentLinkCell{cell}, thresholdConflict)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("threshold conflict error = %v, want invalid argument", err)
	}

	columnFamilyConflict := latentProjectionFixture(t)
	columnFamilyConflict.LinkColumnFamily = "link"
	columnFamilyConflict.IteratorOptions["edgeCF"] = "other-link"
	_, err = ProjectLatentLinkAssertions([]LatentLinkCell{cell}, columnFamilyConflict)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("column family conflict error = %v, want invalid argument", err)
	}
}

func TestProjectLatentLinkAssertionsDegenerateInputs(t *testing.T) {
	projection := latentProjectionFixture(t)

	empty, err := ProjectLatentLinkAssertions(nil, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty input produced %d assertions", len(empty))
	}

	single, err := ProjectLatentLinkAssertions([]LatentLinkCell{
		latentCell("cell-z:missing-source", "missing-target", "0.91"),
	}, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(single) != 1 || single[0].Subject() != "missing-source" {
		t.Fatalf("unknown endpoint assertion = %+v", single)
	}

	self, err := ProjectLatentLinkAssertions([]LatentLinkCell{
		latentCell("cell-z:entity:self", "entity:self", "0.91"),
	}, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(self) != 1 {
		t.Fatalf("self link assertion count = %d, want 1", len(self))
	}
	target, ok := self[0].Object().ReferenceValue()
	if !ok || self[0].Subject() != target {
		t.Fatalf("self link assertion = %+v", self)
	}
}

func latentProjectionFixture(t *testing.T) LatentLinkAssertionProjection {
	t.Helper()
	concept, err := ontology.NewConceptDefinition(
		"latent-node", "Latent Node", "Node eligible for semantic linking", nil, nil)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
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
		t.Fatal(err)
	}
	return LatentLinkAssertionProjection{
		EmbeddingModel:        "text-embedding-3-large",
		EmbeddingModelVersion: "2026-08-01",
		SimilarityThreshold:   0.85,
		Predicate:             relationship.ID(),
		SubjectType:           concept.ID(),
		ObjectType:            concept.ID(),
		IteratorOptions: shoal.Metadata{
			"maxPairsPerCell": "512",
		},
		Provenance:        provenance,
		AssertionMetadata: shoal.Metadata{"projection": "latent-link"},
		EvidenceMetadata:  shoal.Metadata{"source": "latent-edge"},
	}
}

func latentCell(row, target, score string) LatentLinkCell {
	return LatentLinkCell{
		Row:             []byte(row),
		ColumnFamily:    []byte("link"),
		ColumnQualifier: []byte(target),
		Timestamp:       42,
		Value:           []byte(score),
	}
}

func projectedAssertionIDs(
	t *testing.T,
	projection LatentLinkAssertionProjection,
	cells []LatentLinkCell,
) []shoal.ID {
	t.Helper()
	assertions, err := ProjectLatentLinkAssertions(cells, projection)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]shoal.ID, len(assertions))
	for i, assertion := range assertions {
		ids[i] = assertion.ID()
	}
	return ids
}

func projectedSubjects(
	t *testing.T,
	projection LatentLinkAssertionProjection,
	cells []LatentLinkCell,
) []shoal.ID {
	t.Helper()
	assertions, err := ProjectLatentLinkAssertions(cells, projection)
	if err != nil {
		t.Fatal(err)
	}
	subjects := make([]shoal.ID, len(assertions))
	for i, assertion := range assertions {
		subjects[i] = assertion.Subject()
	}
	return subjects
}
