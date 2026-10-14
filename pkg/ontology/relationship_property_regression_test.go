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

func TestExtractionRequiresPropertiesOnRelationshipInstances(t *testing.T) {
	fixture := newOntologyFixture(t)
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	weight := mustProperty(
		t, "weight", "Weight", "", ontology.ValueNumber,
		[]ontology.Constraint{required}, nil,
	)
	relationship, err := ontology.NewRelationshipDefinition(
		"weighted", "Weighted", "", []shoal.ID{fixture.person.ID()},
		[]shoal.ID{fixture.project.ID()}, []shoal.ID{weight.ID()}, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		fixture.schema, "relationship-property", testTime.Add(time.Hour),
		fixture.version.Concepts(),
		append(fixture.version.Relationships(), relationship),
		append(fixture.version.Properties(), weight), nil,
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
	reference, err := ontology.NewReferenceValue("entity:project-1")
	if err != nil {
		t.Fatal(err)
	}
	bareRelationship, err := ontology.NewAssertion(
		"entity:person-1", relationship.ID(), reference,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
		ontology.WithAssertionObjectType(fixture.project.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{bareRelationship}, nil,
		testTime.Add(2*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing relationship property error = %v", err)
	}

	value, _ := ontology.NewNumberValue(0.5)
	unrelatedProperty, err := ontology.NewAssertion(
		"relationship:unrelated", weight.ID(), value,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(relationship.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{bareRelationship, unrelatedProperty}, nil,
		testTime.Add(2*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("unrelated relationship property error = %v", err)
	}
}
