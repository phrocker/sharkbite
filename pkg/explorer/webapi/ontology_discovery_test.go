// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package webapi

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOntologyDiscoveryStrictCandidate(t *testing.T) {
	for _, value := range []string{
		`{"rationale":"first","rationale":"second"}`,
		`{"rationale":"one"} {"rationale":"two"}`,
		`{"rationale":"one","publish":true}`,
		`{"proposed_version":{"unknown":true}}`,
		`{"morphisms":[{"kind":"rename","metadata":{"untrusted":"value"}}]}`,
		`{"rationale":"` + string([]byte{0xff}) + `"}`,
		strings.Repeat("x", MaxDiscoveryCandidateBytes+1),
		`{"open_questions":` + strings.Repeat("[", 34) + `0` + strings.Repeat("]", 34) + `}`,
	} {
		var candidate discoveryCandidate
		if err := decodeDiscoveryCandidate(value, &candidate); err == nil {
			t.Errorf("accepted invalid candidate: %.120s", value)
		}
	}
}

func TestOntologyDiscoveryPreservesMetadataAndZeroCountConstraint(t *testing.T) {
	constraint, err := ontology.NewCountConstraint(ontology.ConstraintMinimumCount, 0)
	if err != nil {
		t.Fatal(err)
	}
	property, err := ontology.NewPropertyDefinition("name", "Name", "A name", ontology.ValueString,
		[]ontology.Constraint{constraint}, shoal.Metadata{"property-context": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition("thing", "Thing", "A thing", []shoal.ID{property.ID()},
		shoal.Metadata{"concept-context": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	relation, err := ontology.NewRelationshipDefinition("related", "Related", "Related things",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, shoal.Metadata{"relation-context": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema("metadata-fixture", "Metadata fixture", "", shoal.Metadata{"schema-context": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	base, err := ontology.NewOntologyVersion(schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{concept}, []ontology.RelationshipDefinition{relation},
		[]ontology.PropertyDefinition{property}, shoal.Metadata{"version-context": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := discoveryBaseDraft(base)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := discoveryVersionFromDraft(base, draft)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base, rebuilt) {
		t.Fatalf("unchanged schema lost constraints, metadata, or identity:\nbase=%#v\nrebuilt=%#v", base, rebuilt)
	}
}

func TestOntologyDiscoveryEvaluationOutcomes(t *testing.T) {
	for _, expected := range []string{"improved", "no_improvement", "regression", "overlap", "duplicate", "unsupported_regression"} {
		t.Run(expected, func(t *testing.T) {
			evaluation := OntologyDiscoveryEvaluation{
				DatasetID: "fixture-dataset", PolicyID: "fixture-policy",
				CaseIDs: []string{"case-1", "case-2"}, EvidenceIDs: []string{"held-out-artifact"},
				Total: 2, BaselineCorrect: 1, CandidateCorrect: 1,
				Limitations: []string{"Synthetic fixture, not a real-model quality benchmark"},
			}

			switch expected {
			case "improved":
				evaluation.CandidateCorrect = 2
			case "regression":
				evaluation.CandidateCorrect = 0
			case "overlap":
				evaluation.EvidenceIDs = []string{"proposal-anchor"}
			case "duplicate":
				evaluation.CaseIDs = []string{"case-1", "case-1"}
			case "unsupported_regression":
				evaluation.CandidateCorrect, evaluation.CandidateUnsupported = 2, 1
			}
			err := validateDiscoveryEvaluation(&evaluation, []string{"proposal-anchor"}, nil)
			if expected == "overlap" || expected == "duplicate" {
				if err == nil {
					t.Fatal("non-independent evaluation accepted")
				}
				return
			}
			status := expected
			if expected == "unsupported_regression" {
				status = "regression"
			}
			if err != nil || evaluation.Status != status {
				t.Fatalf("evaluation status=%s expected=%s err=%v", evaluation.Status, status, err)
			}
		})
	}
}

func TestOntologyDiscoveryPreservesReferenceConstraints(t *testing.T) {
	for _, id := range []shoal.ID{"entity-1", "entity/opaque:reference"} {
		value, err := ontology.NewReferenceValue(id)
		if err != nil {
			t.Fatal(err)
		}
		constraint, err := ontology.NewAllowedValuesConstraint([]ontology.Value{value})
		if err != nil {
			t.Fatal(err)
		}
		property, err := ontology.NewPropertyDefinition("reference", "Reference", "", ontology.ValueReference,
			[]ontology.Constraint{constraint}, nil)
		if err != nil {
			t.Fatal(err)
		}
		concept, _ := ontology.NewConceptDefinition("thing", "Thing", "", []shoal.ID{property.ID()}, nil)
		schema, _ := ontology.NewOntologySchema("reference-fixture", "Reference fixture", "", nil)
		base, err := ontology.NewOntologyVersion(schema, "v1", time.Now().UTC(),
			[]ontology.ConceptDefinition{concept}, nil, []ontology.PropertyDefinition{property}, nil)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := discoveryBaseDraft(base)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt, err := discoveryVersionFromDraft(base, draft)
		if err != nil || !reflect.DeepEqual(base, rebuilt) {
			t.Fatalf("reference %q did not round-trip unchanged: %v", id, err)
		}
	}
}
