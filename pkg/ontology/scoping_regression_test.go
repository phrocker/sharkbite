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

func TestExtractionScopesRequiredPropertiesToOwningTypes(t *testing.T) {
	fixture := newOntologyFixture(t)
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	projectOnly := mustProperty(
		t, "project-only", "Project only", "", ontology.ValueString,
		[]ontology.Constraint{required}, nil,
	)
	projectProperties := append(fixture.project.Properties(), projectOnly.ID())
	project, err := ontology.NewConceptDefinition(
		fixture.project.Key(), fixture.project.Name(), fixture.project.Description(),
		projectProperties, fixture.project.Metadata(),
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		fixture.schema, "scoped-required", testTime.Add(time.Hour),
		[]ontology.ConceptDefinition{fixture.person, project},
		fixture.version.Relationships(),
		append(fixture.version.Properties(), projectOnly), nil,
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
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{fixture.explicit}, nil,
		testTime.Add(2*time.Hour), nil,
	); err != nil {
		t.Fatalf("person assertion incorrectly required project property: %v", err)
	}

	value, _ := ontology.NewStringValue("project")
	projectAssertion, err := ontology.NewAssertion(
		"entity:project-1", fixture.title.ID(), value,
		ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
		fixture.provenance, nil,
		ontology.WithAssertionSubjectType(project.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{fixture.explicit, projectAssertion}, nil,
		testTime.Add(2*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("project required property error = %v", err)
	}
}
