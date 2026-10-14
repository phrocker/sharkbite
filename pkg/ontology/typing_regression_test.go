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
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestExtractionValidatesOntologyTypeApplicability(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := ontology.NewNumberValue(0.5)
	wrongPropertyOwner, err := ontology.NewAssertion(
		"entity:person-1", fixture.score.ID(), value,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{wrongPropertyOwner}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("property ownership error = %v", err)
	}

	reference, err := ontology.NewReferenceValue("entity:person-2")
	if err != nil {
		t.Fatal(err)
	}
	reversedRelationship, err := ontology.NewAssertion(
		"entity:project-1", fixture.worksOn.ID(), reference,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.project.ID()),
		ontology.WithAssertionObjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{reversedRelationship}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("relationship endpoint error = %v", err)
	}
}

func TestExtractionPreflightsCountsBeforeMaterialization(t *testing.T) {
	fixture := newOntologyFixture(t)
	limits := ontology.DefaultExtractionLimits()
	limits.MaxEvidence = 1
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence, fixture.evidence},
		"Extract cited facts.", fixture.provenance, limits, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("request count preflight error = %v", err)
	}

	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	oversized := make([]ontology.Assertion, request.Limits().MaxAssertions+1)
	if _, err := ontology.NewExtractionResult(
		request, oversized, nil, testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("result count preflight error = %v", err)
	}
}

func TestExtractionResultIdentityIncludesNestedMetadata(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := fixture.explicit.Object().StringValue()
	object, _ := ontology.NewStringValue(value)
	alteredAssertion, err := ontology.NewAssertion(
		fixture.explicit.Subject(), fixture.explicit.Predicate(), object,
		fixture.explicit.Origin(), fixture.explicit.Confidence(),
		fixture.explicit.Evidence(), fixture.explicit.Provenance(),
		shoal.Metadata{"review": "changed"},
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := testTime.Add(time.Minute)
	first, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{fixture.explicit}, nil, completedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{alteredAssertion}, nil, completedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() == second.ID() {
		t.Fatal("result identity omitted assertion metadata")
	}

	nextVersion, err := ontology.NewOntologyVersion(
		fixture.schema, "metadata-next", testTime.Add(2*time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstProposal, err := ontology.NewGovernedProposal(
		fixture.schema, fixture.version, nextVersion, "author", "update",
		testTime.Add(3*time.Hour), shoal.Metadata{"review": "one"})
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := ontology.NewGovernedProposal(
		fixture.schema, fixture.version, nextVersion, "author", "update",
		testTime.Add(3*time.Hour), shoal.Metadata{"review": "two"})
	if err != nil {
		t.Fatal(err)
	}
	first, err = ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{firstProposal}, completedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err = ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{secondProposal}, completedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() == second.ID() {
		t.Fatal("result identity omitted proposal metadata")
	}
}
