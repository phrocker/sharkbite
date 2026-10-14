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

func TestUniqueNumberPropertyNormalizesIntegerAndNumberValues(t *testing.T) {
	fixture := newOntologyFixture(t)
	unique, err := ontology.NewFlagConstraint(ontology.ConstraintUnique)
	if err != nil {
		t.Fatal(err)
	}

	property := mustProperty(
		t, "unique-number", "Unique number", "", ontology.ValueNumber,
		[]ontology.Constraint{unique}, nil,
	)
	version, err := ontology.NewOntologyVersion(
		fixture.schema, "unique-number", testTime.Add(time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		append(fixture.version.Properties(), property), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := ontology.NewExtractionRequest(
		version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	number, err := ontology.NewNumberValue(1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ontology.NewAssertion(
		"entity:one", property.ID(), ontology.NewIntegerValue(1),
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewAssertion(
		"entity:two", property.ID(), number,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{first, second}, nil,
		testTime.Add(2*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mixed numeric uniqueness error = %v", err)
	}
}

func TestNumberAllowedValuesAcceptEquivalentIntegerAndNumberValues(t *testing.T) {
	fixture := newOntologyFixture(t)
	allowed, err := ontology.NewAllowedValuesConstraint(
		[]ontology.Value{ontology.NewIntegerValue(1)})
	if err != nil {
		t.Fatal(err)
	}
	property := mustProperty(
		t, "allowed-number", "Allowed number", "", ontology.ValueNumber,
		[]ontology.Constraint{allowed}, nil,
	)
	request := requestWithProperty(
		t, fixture, property, []ontology.EvidenceRef{fixture.evidence})
	number, err := ontology.NewNumberValue(1)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"entity:one", property.ID(), number, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{assertion}, nil,
		testTime.Add(2*time.Hour), nil,
	); err != nil {
		t.Fatalf("equivalent numeric allowed value rejected: %v", err)
	}
}
