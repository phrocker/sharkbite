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

package ontology_test

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var _ interface{ Validate() error } = ontology.AssertionDerivation{}

func TestDerivedAssertionCarriesDerivationEvidence(t *testing.T) {
	fixture := newOntologyFixture(t)
	evidence := mustDerivationEvidence(t)
	assertion := mustAssertion(
		t, fixture.person.ID(), fixture.title.ID(), ontology.AssertionDerived,
		[]ontology.EvidenceRef{evidence}, fixture.provenance,
	)
	if assertion.Origin() != ontology.AssertionDerived {
		t.Fatalf("origin = %q, want %q", assertion.Origin(), ontology.AssertionDerived)
	}
	returned := assertion.Evidence()
	if len(returned) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(returned))
	}
	derivation, ok := returned[0].Derivation()
	if !ok {
		t.Fatal("derived assertion evidence is not derivation-backed")
	}
	if derivation.ID() != mustDerivation(t).ID() {
		t.Fatal("derived assertion did not preserve derivation identity")
	}
}

func TestDerivedAssertionRejectsCitationEvidence(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ontology.NewAssertion(
		"entity:person-1", fixture.title.ID(), value,
		ontology.AssertionDerived, 1,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
	)
	assertInvalidArgumentContains(
		t, err, "derived assertion requires derivation evidence")
}

func TestDerivedAssertionRejectsMultipleDerivations(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewDerivationEvidenceRef(
		newDerivationVariant(t, "score", shoal.Score(0.92)), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ontology.NewAssertion(
		"entity:person-1", fixture.title.ID(), value,
		ontology.AssertionDerived, 1,
		[]ontology.EvidenceRef{mustDerivationEvidence(t), second},
		fixture.provenance, nil,
	)
	assertInvalidArgumentContains(
		t, err, "derived assertion requires exactly one derivation evidence")
}

func TestCitedAssertionsRejectDerivationEvidence(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	evidence := mustDerivationEvidence(t)
	for _, origin := range []ontology.AssertionOrigin{
		ontology.AssertionExplicit,
		ontology.AssertionInferred,
	} {
		t.Run(string(origin), func(t *testing.T) {
			_, err := ontology.NewAssertion(
				"entity:person-1", fixture.title.ID(), value,
				origin, 1, []ontology.EvidenceRef{evidence}, fixture.provenance, nil,
			)
			assertInvalidArgumentContains(
				t, err, "cited assertion cannot use derivation evidence")
		})
	}
}

func TestAssertionRejectsMissingEvidenceByOrigin(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		origin ontology.AssertionOrigin
		want   string
	}{
		"cited": {
			origin: ontology.AssertionExplicit,
			want:   "assertion requires cited evidence",
		},
		"derived": {
			origin: ontology.AssertionDerived,
			want:   "derived assertion requires derivation evidence",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ontology.NewAssertion(
				"entity:person-1", fixture.title.ID(), value,
				test.origin, 1, nil, fixture.provenance, nil,
			)
			assertInvalidArgumentContains(t, err, test.want)
		})
	}
}

func TestAssertionRejectsUnknownOriginFailClosed(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []ontology.AssertionOrigin{"", "unknown"} {
		t.Run(string(origin), func(t *testing.T) {
			_, err := ontology.NewAssertion(
				"entity:person-1", fixture.title.ID(), value,
				origin, 1, []ontology.EvidenceRef{fixture.evidence},
				fixture.provenance, nil,
			)
			assertInvalidArgumentContains(t, err, "invalid assertion origin")
		})
	}
}

func TestExtractionRequestRejectsDerivationEvidence(t *testing.T) {
	fixture := newOntologyFixture(t)
	_, err := ontology.NewExtractionRequest(
		fixture.version,
		[]ontology.EvidenceRef{mustDerivationEvidence(t)},
		"Extract cited facts.",
		fixture.provenance,
		ontology.DefaultExtractionLimits(),
		nil,
	)
	assertInvalidArgumentContains(
		t, err, "extraction request requires cited evidence")
}

func TestAssertionDerivationIDIsCanonicalAndContentDerived(t *testing.T) {
	first := mustDerivation(t)
	second := mustDerivation(t)
	if first.ID() != second.ID() {
		t.Fatalf("identical derivations produced IDs %q and %q", first.ID(), second.ID())
	}
	for name, derivation := range map[string]ontology.AssertionDerivation{
		"embedding model":         newDerivationVariant(t, "embedding-model", "text-embedding-4"),
		"embedding model version": newDerivationVariant(t, "embedding-model-version", "2026-09-01"),
		"similarity metric":       newDerivationVariant(t, "similarity-metric", "dot-product"),
		"threshold":               newDerivationVariant(t, "threshold", shoal.Score(0.81)),
		"tessellation cell":       newDerivationVariant(t, "tessellation-cell", "cell:42"),
		"score":                   newDerivationVariant(t, "score", shoal.Score(0.94)),
		"source endpoint":         newDerivationVariant(t, "source-endpoint", shoal.ID("entity:person-2")),
		"target endpoint":         newDerivationVariant(t, "target-endpoint", shoal.ID("entity:project-2")),
		"iterator name":           newDerivationVariant(t, "iterator-name", "other-iterator"),
		"iterator options":        newDerivationVariant(t, "iterator-options", shoal.Metadata{"maxPairs": "1024"}),
	} {
		t.Run(name, func(t *testing.T) {
			if derivation.ID() == first.ID() {
				t.Fatalf("%s change did not change derivation ID %q", name, first.ID())
			}
		})
	}
}

func TestDerivationRejectsScoreBelowThreshold(t *testing.T) {
	_, err := newDerivation(t, derivationArgs{score: shoal.Score(0.79)})
	assertInvalidArgumentContains(
		t, err, "derivation score must meet or exceed threshold")
}

func TestDerivationConstructorDoesNotAliasCallerOptions(t *testing.T) {
	caller := shoal.Metadata{"maxPairs": "512"}
	derivation, err := newDerivation(t, derivationArgs{iteratorOptions: caller})
	if err != nil {
		t.Fatal(err)
	}
	before := derivation.ID()

	caller["maxPairs"] = "1"

	if got := derivation.IteratorOptions()["maxPairs"]; got != "512" {
		t.Fatalf("derivation observed caller mutation: maxPairs=%q, want 512", got)
	}
	if after := derivation.ID(); after != before {
		t.Fatalf("derivation ID changed after caller mutation: got %q, want %q", after, before)
	}
}

func TestDerivationAccessorDoesNotLeakInternalOptions(t *testing.T) {
	derivation, err := newDerivation(
		t, derivationArgs{iteratorOptions: shoal.Metadata{"maxPairs": "512"}})
	if err != nil {
		t.Fatal(err)
	}
	before := derivation.ID()

	derivation.IteratorOptions()["maxPairs"] = "1"

	if got := derivation.IteratorOptions()["maxPairs"]; got != "512" {
		t.Fatalf("accessor leaked internal map: maxPairs=%q, want 512", got)
	}
	if after := derivation.ID(); after != before {
		t.Fatalf("derivation ID changed after accessor mutation: got %q, want %q", after, before)
	}
}

func mustDerivationEvidence(t *testing.T) ontology.EvidenceRef {
	t.Helper()
	evidence, err := ontology.NewDerivationEvidenceRef(
		mustDerivation(t), shoal.Metadata{"source": "latent-edge"})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func mustDerivation(t *testing.T) ontology.AssertionDerivation {
	t.Helper()
	derivation, err := newDerivation(t, derivationArgs{})
	if err != nil {
		t.Fatal(err)
	}
	return derivation
}

type derivationArgs struct {
	embeddingModel        string
	embeddingModelVersion string
	similarityMetric      string
	threshold             shoal.Score
	tessellationCell      string
	score                 shoal.Score
	sourceEndpoint        shoal.ID
	targetEndpoint        shoal.ID
	iteratorName          string
	iteratorOptions       shoal.Metadata
}

func newDerivation(
	t *testing.T, args derivationArgs,
) (ontology.AssertionDerivation, error) {
	t.Helper()
	if args.embeddingModel == "" {
		args.embeddingModel = "text-embedding-3-large"
	}
	if args.embeddingModelVersion == "" {
		args.embeddingModelVersion = "2026-08-01"
	}
	if args.similarityMetric == "" {
		args.similarityMetric = "cosine"
	}
	if args.threshold == 0 {
		args.threshold = 0.8
	}
	if args.tessellationCell == "" {
		args.tessellationCell = "cell:17"
	}
	if args.score == 0 {
		args.score = 0.91
	}
	if args.sourceEndpoint == "" {
		args.sourceEndpoint = "entity:person-1"
	}
	if args.targetEndpoint == "" {
		args.targetEndpoint = "entity:project-1"
	}
	if args.iteratorName == "" {
		args.iteratorName = "LatentEdgeDiscoveryIterator"
	}
	if args.iteratorOptions == nil {
		args.iteratorOptions = shoal.Metadata{
			"emitBidirectional": "true",
			"maxPairs":          "512",
		}
	}
	return ontology.NewAssertionDerivation(
		args.embeddingModel,
		args.embeddingModelVersion,
		args.similarityMetric,
		args.threshold,
		args.tessellationCell,
		args.score,
		args.sourceEndpoint,
		args.targetEndpoint,
		args.iteratorName,
		args.iteratorOptions,
	)
}

func newDerivationVariant(
	t *testing.T, field string, value interface{},
) ontology.AssertionDerivation {
	t.Helper()
	args := derivationArgs{}
	switch field {
	case "embedding-model":
		args.embeddingModel = value.(string)
	case "embedding-model-version":
		args.embeddingModelVersion = value.(string)
	case "similarity-metric":
		args.similarityMetric = value.(string)
	case "threshold":
		args.threshold = value.(shoal.Score)
	case "tessellation-cell":
		args.tessellationCell = value.(string)
	case "score":
		args.score = value.(shoal.Score)
	case "source-endpoint":
		args.sourceEndpoint = value.(shoal.ID)
	case "target-endpoint":
		args.targetEndpoint = value.(shoal.ID)
	case "iterator-name":
		args.iteratorName = value.(string)
	case "iterator-options":
		args.iteratorOptions = value.(shoal.Metadata)
	default:
		t.Fatalf("unknown derivation variant field %q", field)
	}
	derivation, err := newDerivation(t, args)
	if err != nil {
		t.Fatal(err)
	}
	return derivation
}

func assertInvalidArgumentContains(t *testing.T, err error, want string) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v, want invalid argument containing %q", err, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want to contain %q", err, want)
	}
}
