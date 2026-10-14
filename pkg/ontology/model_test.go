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
	"math"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var testTime = time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)

var (
	_ interface{ Validate() error } = ontology.Value{}
	_ interface{ Validate() error } = ontology.Constraint{}
	_ interface{ Validate() error } = ontology.EvidenceRef{}
	_ interface{ Validate() error } = ontology.ExtractionProvenance{}
	_ interface{ Validate() error } = ontology.Assertion{}
	_ interface{ Validate() error } = ontology.ConceptDefinition{}
	_ interface{ Validate() error } = ontology.RelationshipDefinition{}
	_ interface{ Validate() error } = ontology.PropertyDefinition{}
	_ interface{ Validate() error } = ontology.OntologySchema{}
	_ interface{ Validate() error } = ontology.OntologyVersion{}
	_ interface{ Validate() error } = ontology.ProposalTransition{}
	_ interface{ Validate() error } = ontology.GovernedProposal{}
)

type ontologyFixture struct {
	schema     ontology.OntologySchema
	version    ontology.OntologyVersion
	title      ontology.PropertyDefinition
	score      ontology.PropertyDefinition
	person     ontology.ConceptDefinition
	project    ontology.ConceptDefinition
	worksOn    ontology.RelationshipDefinition
	evidence   ontology.EvidenceRef
	provenance ontology.ExtractionProvenance
	explicit   ontology.Assertion
	inferred   ontology.Assertion
}

func TestTypedIdentityUniquenessAndAssertionOrigin(t *testing.T) {
	fixture := newOntologyFixture(t)
	var sharedID shoal.ID = fixture.title.ID()
	var sharedScore shoal.Score = fixture.explicit.Confidence()
	if sharedID == "" || sharedScore != 0.9 {
		t.Fatal("ontology contract did not expose shared Shoal ID and Score values")
	}

	sameTitle := mustProperty(
		t, "title", "Title", "Canonical display title",
		ontology.ValueString, nil, nil,
	)
	differentTitle := mustProperty(
		t, "title-v2", "Title", "Canonical display title",
		ontology.ValueString, nil, nil,
	)
	if sameTitle.ID() != fixture.title.ID() {
		t.Fatal("equal property keys produced different logical identities")
	}
	if differentTitle.ID() == fixture.title.ID() {
		t.Fatal("distinct property keys produced the same identity")
	}
	if fixture.explicit.ID() == fixture.inferred.ID() {
		t.Fatal("explicit and inferred assertions share an identity")
	}
	if fixture.explicit.Origin() != ontology.AssertionExplicit ||
		fixture.inferred.Origin() != ontology.AssertionInferred {
		t.Fatal("assertion origin was not preserved")
	}

	otherEvidence := mustEvidence(t, 1, 8, "project")
	otherAssertion := mustAssertion(
		t, fixture.person.ID(), fixture.title.ID(), ontology.AssertionExplicit,
		[]ontology.EvidenceRef{otherEvidence}, fixture.provenance,
	)
	if otherAssertion.ID() == fixture.explicit.ID() {
		t.Fatal("assertions with distinct citations share an identity")
	}
}

func TestEvidenceReusesAndClonesGraphPath(t *testing.T) {
	path := graph.Path{
		Nodes: []graph.Node{
			{
				ID: "person-1", Kind: "person", Labels: []string{"person", "entity"},
				Properties: shoal.Metadata{"name": "Ada"},
			},
			{
				ID: "project-1", Kind: "project", Labels: []string{"project"},
				Properties: shoal.Metadata{"name": "Shoal"},
			},
		},
		Edges: []graph.Edge{
			{
				ID: "edge-1", From: "person-1", To: "project-1",
				Type: "works-on", Weight: 0.75,
				Properties: shoal.Metadata{"source": "citation"},
			},
		},
	}
	evidence, err := ontology.NewEvidenceRef(
		validCitation(0, 7), "subject", nil, ontology.WithEvidencePath(path))
	if err != nil {
		t.Fatalf("create graph-backed evidence: %v", err)
	}
	returned, present := evidence.Path()
	if !present || len(returned.Nodes) != 2 || len(returned.Edges) != 1 {
		t.Fatal("evidence graph path was not preserved")
	}
	if returned.Nodes[0].Labels[0] != "entity" {
		t.Fatalf("node labels = %v, want canonical order", returned.Nodes[0].Labels)
	}

	path.Nodes[0].Labels[0] = "mutated"
	path.Nodes[0].Properties["name"] = "mutated"
	path.Edges[0].Properties["source"] = "mutated"
	returned.Nodes[0].Labels[0] = "returned-mutation"
	returned.Nodes[0].Properties["name"] = "returned-mutation"
	returned.Edges[0].Properties["source"] = "returned-mutation"
	if err := evidence.Validate(); err != nil {
		t.Fatalf("graph path mutation escaped defensive copies: %v", err)
	}
	again, _ := evidence.Path()
	if again.Nodes[0].Labels[0] != "entity" ||
		again.Nodes[0].Properties["name"] != "Ada" ||
		again.Edges[0].Properties["source"] != "citation" {
		t.Fatal("evidence returned mutable graph path values")
	}

	reorderedPath := clonePublicPath(again)
	reorderedPath.Nodes[0].Labels[0], reorderedPath.Nodes[0].Labels[1] =
		reorderedPath.Nodes[0].Labels[1], reorderedPath.Nodes[0].Labels[0]
	reordered, err := ontology.NewEvidenceRef(
		validCitation(0, 7), "subject", nil,
		ontology.WithEvidencePath(reorderedPath),
	)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.ID() != evidence.ID() {
		t.Fatal("graph label input order changed evidence identity")
	}

	withoutPath := mustEvidence(t, 0, 7, "subject")
	if withoutPath.ID() == evidence.ID() {
		t.Fatal("optional graph path presence did not affect evidence identity")
	}
}

func TestEvidenceGraphPathPreservesNestedErrorsAndValidatesFloats(t *testing.T) {
	disconnected := graph.Path{
		Nodes: []graph.Node{
			{ID: "one", Kind: "concept"},
			{ID: "two", Kind: "concept"},
		},
	}
	_, err := ontology.NewEvidenceRef(
		validCitation(0, 1), "quote", nil,
		ontology.WithEvidencePath(disconnected),
	)
	assertInvalidArgument(t, err)

	nonFinite := graph.Path{
		Nodes: []graph.Node{
			{ID: "one", Kind: "concept"},
			{ID: "two", Kind: "concept"},
		},
		Edges: []graph.Edge{
			{
				ID: "edge", From: "one", To: "two", Type: "related",
				Weight: shoal.Score(math.Inf(1)),
			},
		},
	}
	_, err = ontology.NewEvidenceRef(
		validCitation(0, 1), "quote", nil,
		ontology.WithEvidencePath(nonFinite),
	)
	assertInvalidArgument(t, err)
}

func TestOntologyVersionCanonicalOrderingAndImmutableExposure(t *testing.T) {
	fixture := newOntologyFixture(t)
	reordered, err := ontology.NewOntologyVersion(
		fixture.schema,
		"1.0.0",
		testTime,
		[]ontology.ConceptDefinition{fixture.project, fixture.person},
		[]ontology.RelationshipDefinition{fixture.worksOn},
		[]ontology.PropertyDefinition{fixture.score, fixture.title},
		shoal.Metadata{"owner": "knowledge"},
	)
	if err != nil {
		t.Fatalf("create reordered version: %v", err)
	}
	if reordered.ID() != fixture.version.ID() {
		t.Fatalf("input order changed version ID: %q != %q", reordered.ID(), fixture.version.ID())
	}
	assertSortedIDs(t, reordered.Properties()[0].ID(), reordered.Properties()[1].ID())
	assertSortedIDs(t, reordered.Concepts()[0].ID(), reordered.Concepts()[1].ID())

	properties := reordered.Properties()
	properties[0] = ontology.PropertyDefinition{}
	metadata := reordered.Metadata()
	metadata["owner"] = "changed"
	conceptProperties := reordered.Concepts()[0].Properties()
	if len(conceptProperties) > 0 {
		conceptProperties[0] = ""
	}
	if err := reordered.Validate(); err != nil {
		t.Fatalf("returned values mutated ontology version: %v", err)
	}
	if reordered.Metadata()["owner"] != "knowledge" {
		t.Fatal("ontology version returned mutable metadata")
	}

	constraints := fixture.title.Constraints()
	if len(constraints) != 0 {
		constraints[0] = ontology.Constraint{}
	}
	propertyMetadata := fixture.title.Metadata()
	propertyMetadata["changed"] = "true"
	if err := fixture.title.Validate(); err != nil {
		t.Fatalf("returned values mutated property definition: %v", err)
	}

	evidenceMetadata := fixture.evidence.Metadata()
	evidenceMetadata["source"] = "changed"
	assertionEvidence := fixture.explicit.Evidence()
	assertionEvidence[0] = ontology.EvidenceRef{}
	provenanceMetadata := fixture.provenance.Metadata()
	provenanceMetadata["temperature"] = "1"
	if err := fixture.explicit.Validate(); err != nil {
		t.Fatalf("returned values mutated assertion: %v", err)
	}
	if fixture.evidence.Metadata()["source"] != "test" {
		t.Fatal("evidence returned mutable metadata")
	}
	if fixture.provenance.Metadata()["temperature"] != "0" {
		t.Fatal("provenance returned mutable metadata")
	}
}

func TestOntologyVersionRejectsDuplicateMembers(t *testing.T) {
	fixture := newOntologyFixture(t)
	tests := map[string]struct {
		concepts      []ontology.ConceptDefinition
		relationships []ontology.RelationshipDefinition
		properties    []ontology.PropertyDefinition
	}{
		"concept": {
			concepts:      []ontology.ConceptDefinition{fixture.person, fixture.person},
			relationships: []ontology.RelationshipDefinition{fixture.worksOn},
			properties:    []ontology.PropertyDefinition{fixture.title, fixture.score},
		},
		"relationship": {
			concepts: []ontology.ConceptDefinition{fixture.person, fixture.project},
			relationships: []ontology.RelationshipDefinition{
				fixture.worksOn, fixture.worksOn,
			},
			properties: []ontology.PropertyDefinition{fixture.title, fixture.score},
		},
		"property": {
			concepts:      []ontology.ConceptDefinition{fixture.person, fixture.project},
			relationships: []ontology.RelationshipDefinition{fixture.worksOn},
			properties:    []ontology.PropertyDefinition{fixture.title, fixture.title},
		},
	}

	t.Run("schema content identity", func(t *testing.T) {
		fixture := newOntologyFixture(t)
		changedSchema, err := ontology.NewOntologySchema(
			fixture.schema.Key(), "Renamed schema", "Changed description",
			shoal.Metadata{"owner": "different"},
		)
		if err != nil {
			t.Fatal(err)
		}
		if changedSchema.ID() != fixture.schema.ID() {
			t.Fatal("test requires schemas to share logical identity")
		}
		changedVersion, err := ontology.NewOntologyVersion(
			changedSchema, fixture.version.Version(), fixture.version.CreatedAt(),
			fixture.version.Concepts(), fixture.version.Relationships(),
			fixture.version.Properties(), fixture.version.Metadata(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if changedVersion.ID() == fixture.version.ID() {
			t.Fatal("ontology version identity omitted schema snapshot content")
		}
	})
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ontology.NewOntologyVersion(
				fixture.schema, "duplicate", testTime, test.concepts,
				test.relationships, test.properties, nil,
			)
			assertInvalidArgument(t, err)
		})
	}
}

func TestInvalidUTF8IsRejectedAcrossWireContracts(t *testing.T) {
	fixture := newOntologyFixture(t)
	invalidUTF8 := string([]byte{0xff})

	tests := map[string]func() error{
		"definition": func() error {
			_, err := ontology.NewPropertyDefinition(
				invalidUTF8, "Name", "", ontology.ValueString, nil, nil)
			return err
		},
		"metadata key": func() error {
			_, err := ontology.NewOntologySchema(
				"schema", "Schema", "", shoal.Metadata{invalidUTF8: "value"})
			return err
		},
		"metadata value": func() error {
			_, err := ontology.NewOntologySchema(
				"schema", "Schema", "", shoal.Metadata{"key": invalidUTF8})
			return err
		},
		"citation ID": func() error {
			citation := validCitation(0, 5)
			citation.DocumentID = shoal.ID(invalidUTF8)
			_, err := ontology.NewEvidenceRef(citation, "quote", nil)
			return err
		},
		"evidence quote": func() error {
			_, err := ontology.NewEvidenceRef(validCitation(0, 5), invalidUTF8, nil)
			return err
		},
		"string value": func() error {
			_, err := ontology.NewStringValue(invalidUTF8)
			return err
		},
		"provenance": func() error {
			_, err := ontology.NewExtractionProvenance(
				"provider", invalidUTF8, "1", "prompt", "1", "extractor", "1", nil)
			return err
		},
		"assertion subject": func() error {
			value, err := ontology.NewStringValue("subject")
			if err != nil {
				return err
			}
			_, err = ontology.NewAssertion(
				shoal.ID(invalidUTF8), fixture.title.ID(), value,
				ontology.AssertionExplicit, 1, []ontology.EvidenceRef{fixture.evidence},
				fixture.provenance, nil,
			)
			return err
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assertInvalidArgument(t, test())
		})
	}
}

func TestRequiredStringsAreExactAndNeverTrimmed(t *testing.T) {
	fixture := newOntologyFixture(t)
	tests := map[string]func() error{
		"definition key": func() error {
			_, err := ontology.NewPropertyDefinition(
				" title ", "Title", "", ontology.ValueString, nil, nil)
			return err
		},
		"definition name": func() error {
			_, err := ontology.NewPropertyDefinition(
				"title", " Title ", "", ontology.ValueString, nil, nil)
			return err
		},
		"metadata key": func() error {
			_, err := ontology.NewOntologySchema(
				"schema", "Schema", "", shoal.Metadata{" owner ": "value"})
			return err
		},
		"provenance model": func() error {
			_, err := ontology.NewExtractionProvenance(
				"provider", " model ", "1", "prompt", "1", "extractor", "1", nil)
			return err
		},
		"assertion subject": func() error {
			value, err := ontology.NewStringValue("subject")
			if err != nil {
				return err
			}
			_, err = ontology.NewAssertion(
				" entity ", fixture.title.ID(), value,
				ontology.AssertionExplicit, 1,
				[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
			)
			return err
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assertInvalidArgument(t, test())
		})
	}
}

func TestEvidenceRejectsInvalidCitations(t *testing.T) {
	tests := map[string]document.Citation{
		"missing immutable IDs": {
			SectionID: "section", Range: validCitation(0, 1).Range,
		},
		"missing location": {
			DocumentID: "document", RevisionID: "revision",
			Range: validCitation(0, 1).Range,
		},
		"negative offset": {
			DocumentID: "document", RevisionID: "revision", SpanID: "span",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: -1},
				End:   document.SourcePosition{Offset: 1},
			},
		},
		"reverse range": {
			DocumentID: "document", RevisionID: "revision", SpanID: "span",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 2},
				End:   document.SourcePosition{Offset: 1},
			},
		},
	}
	for name, citation := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ontology.NewEvidenceRef(citation, "quote", nil)
			assertInvalidArgument(t, err)
		})
	}
}

func TestAssertionRejectsNonFiniteConfidence(t *testing.T) {
	fixture := newOntologyFixture(t)
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	for _, confidence := range []shoal.Score{
		shoal.Score(math.NaN()), shoal.Score(math.Inf(1)),
		shoal.Score(math.Inf(-1)), -0.01, 1.01,
	} {
		_, err := ontology.NewAssertion(
			"entity:person-1", fixture.title.ID(), value,
			ontology.AssertionExplicit, confidence,
			[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
		)
		assertInvalidArgument(t, err)
	}
}

func TestPropertyRejectsInvalidConstraints(t *testing.T) {
	minimumTen, err := ontology.NewValueConstraint(
		ontology.ConstraintMinimumValue, ontology.NewIntegerValue(10))
	if err != nil {
		t.Fatal(err)
	}
	maximumOne, err := ontology.NewValueConstraint(
		ontology.ConstraintMaximumValue, ontology.NewIntegerValue(1))
	if err != nil {
		t.Fatal(err)
	}
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	invalidPattern, patternErr := ontology.NewPatternConstraint("[")
	assertInvalidArgument(t, patternErr)
	if invalidPattern.Kind() != "" {
		t.Fatal("failed pattern construction returned a populated value")
	}

	tests := map[string]func() error{
		"unknown kind": func() error {
			_, err := ontology.NewFlagConstraint(ontology.ConstraintKind("unknown"))
			return err
		},
		"wrong constructor": func() error {
			_, err := ontology.NewFlagConstraint(ontology.ConstraintMinimumCount)
			return err
		},
		"minimum exceeds maximum": func() error {
			_, err := ontology.NewPropertyDefinition(
				"age", "Age", "", ontology.ValueInteger,
				[]ontology.Constraint{minimumTen, maximumOne}, nil)
			return err
		},
		"large integer minimum exceeds maximum": func() error {
			minimum, err := ontology.NewValueConstraint(
				ontology.ConstraintMinimumValue,
				ontology.NewIntegerValue(9007199254740993),
			)
			if err != nil {
				return err
			}
			maximum, err := ontology.NewValueConstraint(
				ontology.ConstraintMaximumValue,
				ontology.NewIntegerValue(9007199254740992),
			)
			if err != nil {
				return err
			}
			_, err = ontology.NewPropertyDefinition(
				"sequence", "Sequence", "", ontology.ValueInteger,
				[]ontology.Constraint{minimum, maximum}, nil)
			return err
		},
		"duplicate kind": func() error {
			_, err := ontology.NewPropertyDefinition(
				"name", "Name", "", ontology.ValueString,
				[]ontology.Constraint{required, required}, nil)
			return err
		},
		"required with zero maximum count": func() error {
			maximum, err := ontology.NewCountConstraint(
				ontology.ConstraintMaximumCount, 0)
			if err != nil {
				return err
			}
			_, err = ontology.NewPropertyDefinition(
				"name", "Name", "", ontology.ValueString,
				[]ontology.Constraint{required, maximum}, nil)
			return err
		},
		"allowed value below minimum": func() error {
			allowed, err := ontology.NewAllowedValuesConstraint(
				[]ontology.Value{ontology.NewIntegerValue(1)})
			if err != nil {
				return err
			}
			_, err = ontology.NewPropertyDefinition(
				"age", "Age", "", ontology.ValueInteger,
				[]ontology.Constraint{minimumTen, allowed}, nil)
			return err
		},
		"allowed value fails pattern": func() error {
			pattern, err := ontology.NewPatternConstraint(`^[A-Z]+$`)
			if err != nil {
				return err
			}
			lower, err := ontology.NewStringValue("lower")
			if err != nil {
				return err
			}
			allowed, err := ontology.NewAllowedValuesConstraint(
				[]ontology.Value{lower})
			if err != nil {
				return err
			}
			_, err = ontology.NewPropertyDefinition(
				"code", "Code", "", ontology.ValueString,
				[]ontology.Constraint{pattern, allowed}, nil)
			return err
		},
		"pattern type mismatch": func() error {
			pattern, err := ontology.NewPatternConstraint("[0-9]+")
			if err != nil {
				return err
			}
			_, err = ontology.NewPropertyDefinition(
				"age", "Age", "", ontology.ValueInteger,
				[]ontology.Constraint{pattern}, nil)
			return err
		},
		"duplicate allowed value": func() error {
			value, err := ontology.NewStringValue("same")
			if err != nil {
				return err
			}
			_, err = ontology.NewAllowedValuesConstraint([]ontology.Value{value, value})
			return err
		},
		"non-finite number": func() error {
			_, err := ontology.NewNumberValue(math.NaN())
			return err
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assertInvalidArgument(t, test())
		})
	}
}

func TestGovernedProposalLifecycleAndInvalidStates(t *testing.T) {
	fixture := newOntologyFixture(t)
	nextVersion, err := ontology.NewOntologyVersion(
		fixture.schema, "2.0.0", testTime.Add(time.Hour),
		[]ontology.ConceptDefinition{fixture.person, fixture.project},
		[]ontology.RelationshipDefinition{fixture.worksOn},
		[]ontology.PropertyDefinition{fixture.title, fixture.score}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposal(
		fixture.schema, fixture.version, nextVersion,
		"author", "add ontology coverage", testTime.Add(2*time.Hour), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := proposal.Transition(
		ontology.ProposalSubmitted, "author", "ready for review",
		testTime.Add(3*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := submitted.Transition(
		ontology.ProposalApproved, "reviewer", "meets governance policy",
		testTime.Add(4*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if approved.ID() != proposal.ID() {
		t.Fatal("proposal identity changed across lifecycle transitions")
	}
	if approved.State() != ontology.ProposalApproved ||
		len(approved.Transitions()) != 2 {
		t.Fatal("proposal lifecycle was not retained")
	}
	published, err := approved.Transition(
		ontology.ProposalPublished, "publisher", "publish immutable version",
		testTime.Add(5*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if published.State() != ontology.ProposalPublished ||
		len(published.Transitions()) != 3 {
		t.Fatal("published proposal lifecycle was not retained")
	}
	if proposal.State() != ontology.ProposalDraft || len(proposal.Transitions()) != 0 {
		t.Fatal("proposal transition mutated the original proposal")
	}

	if _, err := proposal.Transition(
		ontology.ProposalApproved, "reviewer", "skip review",
		testTime.Add(3*time.Hour),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid draft-to-approved transition error = %v", err)
	}
	if _, err := approved.Transition(
		ontology.ProposalRejected, "reviewer", "too late",
		testTime.Add(5*time.Hour),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid approved-to-rejected transition error = %v", err)
	}
	if _, err := published.Transition(
		ontology.ProposalWithdrawn, "author", "too late",
		testTime.Add(6*time.Hour),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid published-to-withdrawn transition error = %v", err)
	}
	if _, err := submitted.Transition(
		ontology.ProposalApproved, "reviewer", "out of order",
		testTime.Add(2*time.Hour),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("out-of-order transition error = %v", err)
	}
	if _, err := ontology.NewProposalTransition(
		ontology.ProposalState("unknown"), ontology.ProposalSubmitted,
		"actor", "note", testTime,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("unknown state transition error = %v", err)
	}
}

func TestWireTimestampsAreCanonicalizedAndRequired(t *testing.T) {
	schema, err := ontology.NewOntologySchema("schema", "Schema", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewOntologyVersion(
		schema, "1", time.Time{}, nil, nil, nil, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing version timestamp error = %v", err)
	}

	location := time.FixedZone("offset", -4*60*60)
	local := time.Date(2026, 8, 25, 16, 0, 0, 0, location)
	version, err := ontology.NewOntologyVersion(
		schema, "1", local, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("canonicalize timestamp: %v", err)
	}
	if version.CreatedAt().Location() != time.UTC ||
		!version.CreatedAt().Equal(local) {
		t.Fatalf("created at = %v, want canonical UTC equivalent of %v", version.CreatedAt(), local)
	}
	if _, err := ontology.NewTimestampValue(time.Time{}); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("missing value timestamp error = %v", err)
	}
}

func newOntologyFixture(t *testing.T) ontologyFixture {
	t.Helper()
	title := mustProperty(
		t, "title", "Title", "Canonical display title",
		ontology.ValueString, nil, shoal.Metadata{"searchable": "true"},
	)
	score := mustProperty(
		t, "score", "Score", "Confidence score",
		ontology.ValueNumber, nil, nil,
	)
	person, err := ontology.NewConceptDefinition(
		"person", "Person", "A person", []shoal.ID{title.ID()}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	project, err := ontology.NewConceptDefinition(
		"project", "Project", "A project",
		[]shoal.ID{score.ID(), title.ID()}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	worksOn, err := ontology.NewRelationshipDefinition(
		"works-on", "Works on", "Person participates in project",
		[]shoal.ID{person.ID()}, []shoal.ID{project.ID()},
		[]shoal.ID{score.ID()}, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"work", "Work ontology", "People and projects",
		shoal.Metadata{"owner": "knowledge"},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1.0.0", testTime,
		[]ontology.ConceptDefinition{person, project},
		[]ontology.RelationshipDefinition{worksOn},
		[]ontology.PropertyDefinition{title, score},
		shoal.Metadata{"owner": "knowledge"},
	)
	if err != nil {
		t.Fatal(err)
	}
	evidence := mustEvidence(t, 0, 7, "subject")
	provenance, err := ontology.NewExtractionProvenance(
		"test-provider", "test-model", "2026-08", "ontology-v1", "3",
		"fake-extractor", "1.2.0", shoal.Metadata{"temperature": "0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	explicit := mustAssertion(
		t, person.ID(), title.ID(), ontology.AssertionExplicit,
		[]ontology.EvidenceRef{evidence}, provenance,
	)
	inferred := mustAssertion(
		t, person.ID(), title.ID(), ontology.AssertionInferred,
		[]ontology.EvidenceRef{evidence}, provenance,
	)
	return ontologyFixture{
		schema: schema, version: version, title: title, score: score,
		person: person, project: project, worksOn: worksOn,
		evidence: evidence, provenance: provenance,
		explicit: explicit, inferred: inferred,
	}
}

func mustProperty(
	t *testing.T,
	key, name, description string,
	valueType ontology.ValueType,
	constraints []ontology.Constraint,
	metadata shoal.Metadata,
) ontology.PropertyDefinition {
	t.Helper()
	property, err := ontology.NewPropertyDefinition(
		key, name, description, valueType, constraints, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return property
}

func mustEvidence(t *testing.T, start, end int64, quote string) ontology.EvidenceRef {
	t.Helper()
	evidence, err := ontology.NewEvidenceRef(
		validCitation(start, end), quote, shoal.Metadata{"source": "test"})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func validCitation(start, end int64) document.Citation {
	return document.Citation{
		DocumentID: "document-1",
		RevisionID: "revision-1",
		SectionID:  "section-1",
		SpanID:     "span-1",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: start, Page: 1},
			End:   document.SourcePosition{Offset: end, Page: 1},
		},
	}
}

func mustAssertion(
	t *testing.T,
	subjectType shoal.ID,
	predicate shoal.ID,
	origin ontology.AssertionOrigin,
	evidence []ontology.EvidenceRef,
	provenance ontology.ExtractionProvenance,
) ontology.Assertion {
	t.Helper()
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"entity:person-1", predicate, value, origin, 0.9,
		evidence, provenance, shoal.Metadata{"review": "pending"},
		ontology.WithAssertionSubjectType(subjectType),
	)
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}

func assertSortedIDs(t *testing.T, first, second shoal.ID) {
	t.Helper()
	if string(first) >= string(second) {
		t.Fatalf("IDs are not canonically ordered: %q >= %q", first, second)
	}
}

func assertInvalidArgument(t *testing.T, err error) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v, want invalid argument", err)
	}
}

func clonePublicPath(path graph.Path) graph.Path {
	cloned := graph.Path{
		Nodes: append([]graph.Node(nil), path.Nodes...),
		Edges: append([]graph.Edge(nil), path.Edges...),
	}
	for index := range cloned.Nodes {
		cloned.Nodes[index].Labels = append([]string(nil), path.Nodes[index].Labels...)
		cloned.Nodes[index].Properties = clonePublicMetadata(path.Nodes[index].Properties)
	}
	for index := range cloned.Edges {
		cloned.Edges[index].Properties = clonePublicMetadata(path.Edges[index].Properties)
	}
	return cloned
}

func clonePublicMetadata(metadata shoal.Metadata) shoal.Metadata {
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
