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
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var _ interface{ Validate() error } = ontology.OntologyIdentity{}

// successor is a second version of the fixture schema. Its definition keys are
// unchanged but its definitions have moved: title becomes required, Person
// gains a property and a new description, and a filler concept is added whose
// content-addressed ID sorts ahead of everything else, so the concept slice is
// ordered differently than the base version's. Definition IDs are derived from
// namespace and key alone, so this is the drift case -- the same concept and
// property IDs carrying a different meaning.
type successor struct {
	version ontology.OntologyVersion
	person  ontology.ConceptDefinition
	title   ontology.PropertyDefinition
	filler  ontology.ConceptDefinition
}

func successorVersion(t *testing.T, fixture ontologyFixture) successor {
	t.Helper()
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	title := mustProperty(
		t, "title", "Title", "Canonical display title, now mandatory",
		ontology.ValueString, []ontology.Constraint{required}, nil,
	)
	person, err := ontology.NewConceptDefinition(
		"person", "Person", "A person, now scored",
		[]shoal.ID{fixture.score.ID(), title.ID()}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	project, err := ontology.NewConceptDefinition(
		"project", "Project", "A project",
		[]shoal.ID{fixture.score.ID(), title.ID()}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Definitions are sorted by their content-addressed IDs, so a concept that
	// sorts ahead of both of these shifts every later position. The key is
	// searched for rather than hard-coded so the reordering holds whatever the
	// digests happen to be -- including under a mutation that changes how
	// definition IDs are derived.
	filler := firstConceptSortingBefore(t, person.ID(), project.ID())
	worksOn, err := ontology.NewRelationshipDefinition(
		"works-on", "Works on", "Person participates in project",
		[]shoal.ID{person.ID()}, []shoal.ID{project.ID()},
		[]shoal.ID{fixture.score.ID()}, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		fixture.schema, "3.0.0", testTime,
		[]ontology.ConceptDefinition{filler, person, project},
		[]ontology.RelationshipDefinition{worksOn},
		[]ontology.PropertyDefinition{title, fixture.score},
		shoal.Metadata{"owner": "knowledge"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return successor{version: version, person: person, title: title, filler: filler}
}

// firstConceptSortingBefore returns a concept whose ID sorts before every
// supplied ID.
func firstConceptSortingBefore(
	t *testing.T, others ...shoal.ID,
) ontology.ConceptDefinition {
	t.Helper()
	for attempt := 0; attempt < 4096; attempt++ {
		key := fmt.Sprintf("filler-%d", attempt)
		concept, err := ontology.NewConceptDefinition(key, "Filler", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sortsFirst := true
		for _, other := range others {
			if string(concept.ID()) >= string(other) {
				sortsFirst = false
				break
			}
		}
		if sortsFirst {
			return concept
		}
	}
	t.Fatal("no filler concept sorted ahead of the fixture concepts")
	return ontology.ConceptDefinition{}
}

// conceptByKey finds a concept by its key. Positional lookup is never correct
// here: the slice is ordered by content-addressed ID, so a position refers to
// different concepts in different versions.
func conceptByKey(
	t *testing.T, version ontology.OntologyVersion, key string,
) ontology.ConceptDefinition {
	t.Helper()
	for _, concept := range version.Concepts() {
		if concept.Key() == key {
			return concept
		}
	}
	t.Fatalf("ontology version %s has no concept keyed %q", version.Version(), key)
	return ontology.ConceptDefinition{}
}

func propertyByKey(
	t *testing.T, version ontology.OntologyVersion, key string,
) ontology.PropertyDefinition {
	t.Helper()
	for _, property := range version.Properties() {
		if property.Key() == key {
			return property
		}
	}
	t.Fatalf("ontology version %s has no property keyed %q", version.Version(), key)
	return ontology.PropertyDefinition{}
}

func assertionUnder(
	t *testing.T,
	fixture ontologyFixture,
	options ...ontology.AssertionOption,
) ontology.Assertion {
	t.Helper()
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	options = append(
		[]ontology.AssertionOption{
			ontology.WithAssertionSubjectType(fixture.person.ID()),
		},
		options...,
	)
	assertion, err := ontology.NewAssertion(
		"entity:person-1", fixture.title.ID(), value,
		ontology.AssertionExplicit, 0.9,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance,
		shoal.Metadata{"review": "pending"}, options...,
	)
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}

func mustIdentity(t *testing.T, version ontology.OntologyVersion) ontology.OntologyIdentity {
	t.Helper()
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

// TestDefinitionIdentityIsStableAcrossVersions pins the premise the whole
// design rests on: a definition's ID is derived from its namespace and key
// alone, so redefining Person in a later version -- new description, new
// property, tightened constraints -- leaves concept:Person the same ID. That
// stability is why the ontology version is recorded on the assertion rather
// than folded into the definition IDs: version-qualifying the IDs would make
// every version bump, including a purely additive one, look like a deletion.
func TestDefinitionIdentityIsStableAcrossVersions(t *testing.T) {
	fixture := newOntologyFixture(t)
	next := successorVersion(t, fixture)

	basePerson := conceptByKey(t, fixture.version, "person")
	nextPerson := conceptByKey(t, next.version, "person")
	if basePerson.ID() != nextPerson.ID() {
		t.Fatalf("concept key \"person\" is not stable across versions: %s then %s",
			basePerson.ID(), nextPerson.ID())
	}
	if basePerson.Description() == nextPerson.Description() ||
		len(basePerson.Properties()) == len(nextPerson.Properties()) {
		t.Fatal("the successor did not redefine Person, so ID stability is untested")
	}

	baseTitle := propertyByKey(t, fixture.version, "title")
	nextTitle := propertyByKey(t, next.version, "title")
	if nextPerson.ID() != next.person.ID() || nextTitle.ID() != next.title.ID() {
		t.Fatal("lookup by key returned a different definition than the version was built from")
	}
	if baseTitle.ID() != nextTitle.ID() {
		t.Fatalf("property key \"title\" is not stable across versions: %s then %s",
			baseTitle.ID(), nextTitle.ID())
	}
	if len(baseTitle.Constraints()) == len(nextTitle.Constraints()) {
		t.Fatal("the successor did not retighten title, so ID stability is untested")
	}

	// The definitions are the same IDs, but the versions holding them are not,
	// which is exactly why an assertion cannot be read without one.
	if next.version.ID() == fixture.version.ID() {
		t.Fatal("two ontology versions with different definitions share an ID")
	}

	// Guard the lookup itself: definitions are ordered by content-addressed ID,
	// so comparing by slice position compares different concepts in the two
	// versions. If this ever stops holding, the by-key lookups above are the
	// only reason this test is meaningful.
	if next.version.Concepts()[0].Key() == fixture.version.Concepts()[0].Key() {
		t.Fatal("the successor does not reorder the concept slice; " +
			"the positional-comparison hazard is untested")
	}
	if next.version.Concepts()[0].Key() != next.filler.Key() {
		t.Fatalf("filler concept did not sort first: %q",
			next.version.Concepts()[0].Key())
	}
}

// TestAssertionsUnderDifferentOntologyVersionsAreDistinguishable is the
// prerequisite defect from issue #279: concept:Person is the same ID under
// every version, so without a recorded ontology identity two assertions made
// under incompatible meanings are indistinguishable.
func TestAssertionsUnderDifferentOntologyVersionsAreDistinguishable(t *testing.T) {
	fixture := newOntologyFixture(t)
	next := successorVersion(t, fixture)

	if conceptByKey(t, next.version, "person").ID() != fixture.person.ID() {
		t.Fatal("concept keys are not stable across versions")
	}
	if next.version.ID() == fixture.version.ID() {
		t.Fatal("two ontology versions with different definitions share an ID")
	}

	first := mustIdentity(t, fixture.version)
	second := mustIdentity(t, next.version)
	if first == second {
		t.Fatal("identities for different versions compare equal")
	}
	if first.SchemaID() != second.SchemaID() {
		t.Fatal("versions of one schema reported different schema identities")
	}

	underFirst := assertionUnder(t, fixture, ontology.WithAssertionOntology(first))
	underSecond := assertionUnder(t, fixture, ontology.WithAssertionOntology(second))

	if underFirst.ID() == underSecond.ID() {
		t.Fatal("assertions made under different ontology versions share an identity")
	}
	recordedFirst, known := underFirst.Ontology()
	if !known || recordedFirst != first {
		t.Fatalf("assertion did not carry its ontology identity: %v, known=%v",
			recordedFirst, known)
	}
	recordedSecond, known := underSecond.Ontology()
	if !known || recordedSecond != second {
		t.Fatalf("assertion did not carry its ontology identity: %v, known=%v",
			recordedSecond, known)
	}
	if recordedFirst.VersionID() == recordedSecond.VersionID() {
		t.Fatal("assertions recorded the same version ID for different versions")
	}

	// Reading either assertion under the other's version is detectable.
	if reading := underFirst.ReadUnder(first); reading != ontology.OntologySameVersion {
		t.Fatalf("reading under its own version = %q, want same_version", reading)
	}
	if reading := underFirst.ReadUnder(second); reading != ontology.OntologyOtherVersion {
		t.Fatalf("reading under a different version = %q, want other_version", reading)
	}
	if reading := underSecond.ReadUnder(first); reading != ontology.OntologyOtherVersion {
		t.Fatalf("reading under a different version = %q, want other_version", reading)
	}
	if !underFirst.ReadUnder(second).Resolved() {
		t.Fatal("a version mismatch was not reported as resolved")
	}
}

func TestForeignSchemaIsDistinguishedFromAForeignVersion(t *testing.T) {
	fixture := newOntologyFixture(t)
	otherSchema, err := ontology.NewOntologySchema(
		"other-work", "Other work ontology", "A different schema", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherVersion, err := ontology.NewOntologyVersion(
		otherSchema, "1.0.0", testTime,
		[]ontology.ConceptDefinition{fixture.person, fixture.project},
		[]ontology.RelationshipDefinition{fixture.worksOn},
		[]ontology.PropertyDefinition{fixture.title, fixture.score}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertion := assertionUnder(
		t, fixture, ontology.WithAssertionOntology(mustIdentity(t, fixture.version)))
	reading := assertion.ReadUnder(mustIdentity(t, otherVersion))
	if reading != ontology.OntologyOtherSchema {
		t.Fatalf("reading under another schema = %q, want other_schema", reading)
	}
}

// TestAssertionWithoutOntologyReportsUnknown pins the #274 rule: an assertion
// that recorded nothing must say so, and must not be read as having been made
// under whatever version is in hand.
func TestAssertionWithoutOntologyReportsUnknown(t *testing.T) {
	fixture := newOntologyFixture(t)
	next := successorVersion(t, fixture)
	assertion := assertionUnder(t, fixture)

	recorded, known := assertion.Ontology()
	if known {
		t.Fatalf("assertion with no recorded ontology reported %v as known", recorded)
	}
	if recorded != ontology.UnknownOntology() {
		t.Fatal("an unrecorded ontology identity is not the unknown identity")
	}
	if recorded.SchemaID() != "" || recorded.VersionID() != "" {
		t.Fatalf("unknown identity carries fabricated IDs: %v", recorded)
	}
	if recorded.String() != "unknown" {
		t.Fatalf("unknown identity renders as %q", recorded.String())
	}

	// It adopts neither the version it could plausibly have been made under nor
	// any other, and it never reports agreement.
	for name, identity := range map[string]ontology.OntologyIdentity{
		"first":     mustIdentity(t, fixture.version),
		"successor": mustIdentity(t, next.version),
		"unknown":   ontology.UnknownOntology(),
	} {
		reading := assertion.ReadUnder(identity)
		if reading != ontology.OntologyUnresolved {
			t.Fatalf("reading an unrecorded assertion under %s = %q, want unresolved",
				name, reading)
		}
		if reading.Resolved() {
			t.Fatalf("unresolved reading under %s reported itself resolved", name)
		}
	}

	// A reader that holds no ontology cannot resolve a recorded assertion
	// either; the unknown state is absorbing on both sides.
	recordedAssertion := assertionUnder(
		t, fixture, ontology.WithAssertionOntology(mustIdentity(t, fixture.version)))
	if reading := recordedAssertion.ReadUnder(
		ontology.UnknownOntology()); reading != ontology.OntologyUnresolved {
		t.Fatalf("reading under an unknown reader = %q, want unresolved", reading)
	}

	// An unresolved assertion is a different fact from the same assertion made
	// under a known version.
	if assertion.ID() == recordedAssertion.ID() {
		t.Fatal("an unresolved assertion shares an identity with a version-qualified one")
	}
}

func TestOntologyIdentityRejectsPartialAndSwappedIdentities(t *testing.T) {
	fixture := newOntologyFixture(t)
	identity := mustIdentity(t, fixture.version)

	if _, err := ontology.NewOntologyIdentityFromIDs(
		identity.VersionID(), identity.SchemaID()); err == nil {
		t.Fatal("swapped schema and version IDs were accepted")
	} else {
		assertInvalidArgument(t, err)
	}
	for name, ids := range map[string][2]shoal.ID{
		"schema only":  {identity.SchemaID(), ""},
		"version only": {"", identity.VersionID()},
	} {
		partial, err := ontology.NewOntologyIdentityFromIDs(ids[0], ids[1])
		if err == nil {
			t.Fatalf("%s identity was accepted: %v", name, partial)
		}
		assertInvalidArgument(t, err)
	}

	if ontology.UnknownOntology().Known() {
		t.Fatal("the unknown identity reported itself known")
	}
	if ontology.UnknownOntology().Validate() == nil {
		t.Fatal("the unknown identity validated as a resolved identity")
	}
	if !identity.Known() || identity.Validate() != nil {
		t.Fatal("a resolved identity did not validate")
	}
	if ontology.ReadOntologyUnder(identity, identity) != ontology.OntologySameVersion {
		t.Fatal("an identity did not match itself")
	}
}

func TestExtractionResultRejectsAssertionsFromAnotherVersion(t *testing.T) {
	fixture := newOntologyFixture(t)
	next := successorVersion(t, fixture)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	foreign := assertionUnder(
		t, fixture, ontology.WithAssertionOntology(mustIdentity(t, next.version)))
	_, err = ontology.NewExtractionResult(
		request, []ontology.Assertion{foreign}, nil, testTime.Add(time.Minute), nil)
	assertInvalidArgument(t, err)

	matching := assertionUnder(
		t, fixture, ontology.WithAssertionOntology(mustIdentity(t, fixture.version)))
	result, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{matching}, nil, testTime.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("result under the pinned version: %v", err)
	}
	if len(result.UnresolvedOntologyAssertions()) != 0 {
		t.Fatal("a version-qualified assertion was reported as unresolved")
	}
}

func TestExtractionResultReportsUnresolvedAssertionsWithoutStampingThem(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	unresolved := assertionUnder(t, fixture)
	result, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{unresolved}, nil, testTime.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("result with an unresolved assertion: %v", err)
	}
	reported := result.UnresolvedOntologyAssertions()
	if len(reported) != 1 || reported[0] != unresolved.ID() {
		t.Fatalf("unresolved assertions = %v, want [%s]", reported, unresolved.ID())
	}
	stored := result.Assertions()
	if len(stored) != 1 {
		t.Fatalf("result holds %d assertions, want 1", len(stored))
	}
	if recorded, known := stored[0].Ontology(); known {
		t.Fatalf("the result stamped the request's version onto an assertion: %v", recorded)
	}
}
