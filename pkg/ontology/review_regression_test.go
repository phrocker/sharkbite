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
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestExtractionRejectsEvidenceWithDifferentMetadata(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nested path metadata", func(t *testing.T) {
		fixture := newOntologyFixture(t)
		evidence, err := ontology.NewEvidenceRef(
			fixture.evidence.Citation(), fixture.evidence.Quote(), nil,
			ontology.WithEvidencePath(graph.Path{Nodes: []graph.Node{{
				ID: "node", Kind: "entity",
				Properties: shoal.Metadata{"first": "1", "second": "2"},
			}}}),
		)
		if err != nil {
			t.Fatal(err)
		}
		limits := ontology.DefaultExtractionLimits()
		limits.MaxMetadataEntries = 1
		if _, err := ontology.NewExtractionRequest(
			fixture.version, []ontology.EvidenceRef{evidence},
			"Extract cited facts.", fixture.provenance, limits, nil,
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("nested path metadata bound error = %v", err)
		}
	})
	altered, err := ontology.NewEvidenceRef(
		fixture.evidence.Citation(), fixture.evidence.Quote(),
		shoal.Metadata{"source": "altered"},
	)
	if err != nil {
		t.Fatal(err)
	}
	alteredRequest, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{altered},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID() == alteredRequest.ID() {
		t.Fatal("request identity did not include evidence metadata")
	}
	assertion := mustAssertion(
		t, fixture.person.ID(), fixture.title.ID(), ontology.AssertionExplicit,
		[]ontology.EvidenceRef{altered}, fixture.provenance,
	)
	_, err = ontology.NewExtractionResult(
		request, []ontology.Assertion{assertion}, nil,
		testTime.Add(time.Minute), nil,
	)
	assertInvalidArgument(t, err)
}

func TestExtractionAppliesPropertyValueAndCardinalityConstraints(t *testing.T) {
	fixture := newOntologyFixture(t)
	allowed, err := ontology.NewAllowedValuesConstraint(
		[]ontology.Value{ontology.NewIntegerValue(10), ontology.NewIntegerValue(20)})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("minimum count and unique values", func(t *testing.T) {
		fixture := newOntologyFixture(t)
		minimum, err := ontology.NewCountConstraint(ontology.ConstraintMinimumCount, 2)
		if err != nil {
			t.Fatal(err)
		}
		requiredProperty := mustProperty(
			t, "required-output", "Required output", "", ontology.ValueString,
			[]ontology.Constraint{minimum}, nil,
		)
		request := requestWithProperty(
			t, fixture, requiredProperty, []ontology.EvidenceRef{fixture.evidence})
		if _, err := ontology.NewExtractionResult(
			request, []ontology.Assertion{fixture.explicit}, nil,
			testTime.Add(time.Minute), nil,
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("zero-count minimum error = %v", err)
		}

		unique, err := ontology.NewFlagConstraint(ontology.ConstraintUnique)
		if err != nil {
			t.Fatal(err)
		}
		uniqueProperty := mustProperty(
			t, "unique-code", "Unique code", "", ontology.ValueString,
			[]ontology.Constraint{unique}, nil,
		)
		request = requestWithProperty(
			t, fixture, uniqueProperty, []ontology.EvidenceRef{fixture.evidence})
		value, _ := ontology.NewStringValue("shared")
		first, _ := ontology.NewAssertion(
			"entity:one", uniqueProperty.ID(), value, ontology.AssertionExplicit, 1,
			[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
			ontology.WithAssertionSubjectType(fixture.person.ID()),
		)
		second, _ := ontology.NewAssertion(
			"entity:two", uniqueProperty.ID(), value, ontology.AssertionExplicit, 1,
			[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
			ontology.WithAssertionSubjectType(fixture.person.ID()),
		)
		if _, err := ontology.NewExtractionResult(
			request, []ontology.Assertion{first, second}, nil,
			testTime.Add(time.Minute), nil,
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("duplicate unique value error = %v", err)
		}
	})
	numeric := mustProperty(
		t, "bounded-score", "Bounded score", "", ontology.ValueInteger,
		[]ontology.Constraint{allowed}, nil,
	)
	request := requestWithProperty(
		t, fixture, numeric, []ontology.EvidenceRef{fixture.evidence})
	bad, err := ontology.NewAssertion(
		"entity:person-1", numeric.ID(), ontology.NewIntegerValue(15),
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{bad}, nil, testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("disallowed property value error = %v", err)
	}

	pattern, err := ontology.NewPatternConstraint(`^[A-Z]+$`)
	if err != nil {
		t.Fatal(err)
	}
	maxCount, err := ontology.NewCountConstraint(ontology.ConstraintMaximumCount, 1)
	if err != nil {
		t.Fatal(err)
	}
	code := mustProperty(
		t, "code", "Code", "", ontology.ValueString,
		[]ontology.Constraint{pattern, maxCount}, nil,
	)
	secondEvidence := mustEvidence(t, 8, 12, "more")
	request = requestWithProperty(
		t, fixture, code, []ontology.EvidenceRef{fixture.evidence, secondEvidence})
	lower, _ := ontology.NewStringValue("lower")
	patternMismatch, err := ontology.NewAssertion(
		"entity:person-1", code.ID(), lower, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{patternMismatch}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("pattern mismatch error = %v", err)
	}

	firstValue, _ := ontology.NewStringValue("ONE")
	secondValue, _ := ontology.NewStringValue("TWO")
	first, _ := ontology.NewAssertion(
		"entity:person-1", code.ID(), firstValue, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	second, _ := ontology.NewAssertion(
		"entity:person-1", code.ID(), secondValue, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{secondEvidence}, fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{first, second}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("maximum count error = %v", err)
	}
}

func TestExtractionRejectsProposalForDifferentBaseVersion(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("different schema", func(t *testing.T) {
		fixture := newOntologyFixture(t)
		otherSchema, err := ontology.NewOntologySchema("other", "Other", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		otherBase, err := ontology.NewOntologyVersion(
			otherSchema, "1", testTime.Add(time.Hour), nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		nextVersion, err := ontology.NewOntologyVersion(
			fixture.schema, "next-schema-check", testTime.Add(2*time.Hour),
			fixture.version.Concepts(), fixture.version.Relationships(),
			fixture.version.Properties(), nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ontology.NewGovernedProposal(
			fixture.schema, otherBase, nextVersion, "author", "invalid base",
			testTime.Add(3*time.Hour), nil,
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("cross-schema base error = %v", err)
		}
	})
	unrelatedBase, err := ontology.NewOntologyVersion(
		fixture.schema, "unrelated", testTime.Add(time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := ontology.NewOntologyVersion(
		fixture.schema, "proposed", testTime.Add(2*time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposal(
		fixture.schema, unrelatedBase, proposed, "extractor",
		"propose update", testTime.Add(3*time.Hour), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{proposal},
		testTime.Add(4*time.Hour), nil,
	)
	assertInvalidArgument(t, err)
}

func TestExtractionPayloadBudgetBoundsVariableText(t *testing.T) {
	fixture := newOntologyFixture(t)
	limits := ontology.DefaultExtractionLimits()
	limits.MaxPayloadBytes = 64 << 10
	huge := strings.Repeat("x", int(limits.MaxPayloadBytes)+1)

	largeProperty := mustProperty(
		t, "large", "Large", huge, ontology.ValueString, nil, nil)
	version, err := ontology.NewOntologyVersion(
		fixture.schema, "large", testTime.Add(time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		append(fixture.version.Properties(), largeProperty), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionRequest(
		version, []ontology.EvidenceRef{fixture.evidence}, "Extract.",
		fixture.provenance, limits, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("schema payload error = %v", err)
	}

	largeProvenance, err := ontology.NewExtractionProvenance(
		"provider", huge, "1", "prompt", "1", "extractor", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence}, "Extract.",
		largeProvenance, limits, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("provenance payload error = %v", err)
	}

	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence}, "Extract.",
		fixture.provenance, limits, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	largeValue, _ := ontology.NewStringValue(huge)
	assertion, err := ontology.NewAssertion(
		"entity:person-1", fixture.title.ID(), largeValue,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{assertion}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("assertion payload error = %v", err)
	}

	nextVersion, err := ontology.NewOntologyVersion(
		fixture.schema, "next", testTime.Add(2*time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposal(
		fixture.schema, fixture.version, nextVersion, "extractor", huge,
		testTime.Add(3*time.Hour), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{proposal},
		testTime.Add(4*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("proposal payload error = %v", err)
	}
}

func requestWithProperty(
	t *testing.T, fixture ontologyFixture, property ontology.PropertyDefinition,
	evidence []ontology.EvidenceRef,
) ontology.ExtractionRequest {
	t.Helper()
	version, err := ontology.NewOntologyVersion(
		fixture.schema, "property-test", testTime.Add(time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		append(fixture.version.Properties(), property), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ontology.NewExtractionRequest(
		version, evidence, "Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
