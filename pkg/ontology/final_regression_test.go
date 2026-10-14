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

func TestPropertyAssertionRejectsObjectType(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, _ := ontology.NewStringValue("value")
	if _, err := ontology.NewAssertion(
		"entity:person-1", fixture.title.ID(), value,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
		ontology.WithAssertionObjectType(fixture.project.ID()),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("property object type error = %v", err)
	}
}

func TestProposalOuterSchemaMetadataUsesExtractionBounds(t *testing.T) {
	fixture := newOntologyFixture(t)
	limits := ontology.DefaultExtractionLimits()
	limits.MaxMetadataEntries = 1
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, limits, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	outerSchema, err := ontology.NewOntologySchema(
		fixture.schema.Key(), fixture.schema.Name(), fixture.schema.Description(),
		shoal.Metadata{"first": "1", "second": "2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	nextVersion, err := ontology.NewOntologyVersion(
		fixture.schema, "outer-metadata", testTime.Add(time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposal(
		outerSchema, fixture.version, nextVersion, "author", "update",
		testTime.Add(2*time.Hour), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{proposal},
		testTime.Add(3*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("outer schema metadata bound error = %v", err)
	}
}

func TestResultIdentityIncludesProposalOuterSchema(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	changedSchema, err := ontology.NewOntologySchema(
		fixture.schema.Key(), "Changed name", fixture.schema.Description(),
		fixture.schema.Metadata(),
	)
	if err != nil {
		t.Fatal(err)
	}
	nextVersion, err := ontology.NewOntologyVersion(
		fixture.schema, "outer-identity", testTime.Add(time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstProposal, err := ontology.NewGovernedProposal(
		fixture.schema, fixture.version, nextVersion, "author", "update",
		testTime.Add(2*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := ontology.NewGovernedProposal(
		changedSchema, fixture.version, nextVersion, "author", "update",
		testTime.Add(2*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := testTime.Add(3 * time.Hour)
	first, err := ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{firstProposal}, completedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{secondProposal}, completedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() == second.ID() {
		t.Fatal("result identity omitted proposal outer schema content")
	}
}
