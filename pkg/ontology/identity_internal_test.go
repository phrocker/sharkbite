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

package ontology

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A half-populated ontology identity cannot be built through the package's
// constructors, so this test is in-package: it covers the guard that keeps such
// a value from being read as the unresolved state, which would let an assertion
// carry a schema with no version and report itself as recording nothing.
func TestPartialOntologyIdentityIsRefusedRatherThanReadAsUnknown(t *testing.T) {
	schemaID, err := deriveID("schema", "work")
	if err != nil {
		t.Fatal(err)
	}
	versionID, err := deriveID("ontology-version", "work", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for name, partial := range map[string]OntologyIdentity{
		"schema only":  {schemaID: schemaID},
		"version only": {versionID: versionID},
	} {
		if !partial.Known() {
			t.Fatalf("%s: a half-populated identity was read as unresolved", name)
		}
		if partial.Validate() == nil {
			t.Fatalf("%s: a half-populated identity validated", name)
		}
		if _, err := newTestAssertion(t, WithAssertionOntology(partial)); err == nil {
			t.Fatalf("%s: an assertion carrying it was accepted", name)
		}
	}

	whole := OntologyIdentity{schemaID: schemaID, versionID: versionID}
	if !whole.Known() || whole.Validate() != nil {
		t.Fatal("a whole identity was refused")
	}
	if _, err := newTestAssertion(t, WithAssertionOntology(whole)); err != nil {
		t.Fatalf("an assertion carrying a whole identity was refused: %v", err)
	}
}

func newTestAssertion(t *testing.T, options ...AssertionOption) (Assertion, error) {
	t.Helper()
	property, err := NewPropertyDefinition(
		"title", "Title", "", ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	citation := document.Citation{
		DocumentID: "document-1", RevisionID: "revision-1",
		SectionID: "section-1", SpanID: "span-1",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 7, Page: 1},
		},
	}
	evidence, err := NewEvidenceRef(citation, "subject", nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := NewExtractionProvenance(
		"test-provider", "test-model", "2026-08", "ontology-v1", "3",
		"fake-extractor", "1.2.0", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	return NewAssertion(
		shoal.ID("entity:person-1"), property.ID(), value, AssertionExplicit, 0.9,
		[]EvidenceRef{evidence}, provenance, nil, options...,
	)
}
