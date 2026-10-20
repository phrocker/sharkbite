// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package inference

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func newReferenceAssertion(
	t *testing.T,
	subject, predicate, object shoal.ID,
	confidence shoal.Score,
	metadata shoal.Metadata,
) ontology.Assertion {
	t.Helper()
	evidence, err := ontology.NewEvidenceRef(
		document.Citation{
			DocumentID: "document-1", RevisionID: "revision-1",
			SectionID: "section-1", SpanID: "span-1",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0, Page: 1},
				End:   document.SourcePosition{Offset: 5, Page: 1},
			},
		},
		"skill",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue(object)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		subject, predicate, value, ontology.AssertionInferred, confidence,
		[]ontology.EvidenceRef{evidence}, provenance, metadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}

// TestAssertionReferencesByEdgeBindsAsTheExplorersRecorderBinds pins the
// three bindings the Explorer's recorder accepts between an assertion and the
// edge it materialized. The extracted relation is the one that matters in a
// real corpus: its edge carries the relationship's key as its type and a
// canonical weight, while the relationship ID and the extraction's
// confidence stay on the assertion, so a field-for-field comparison would
// refuse every extracted relation the Explorer serves.
func TestAssertionReferencesByEdgeBindsAsTheExplorersRecorderBinds(t *testing.T) {
	concept, err := ontology.NewConceptDefinition(
		"skill", "Skill", "Agent skill", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"provides_capability", "Provides capability", "Skill provides capability",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	relationshipID := relationship.ID()

	extracted := newReferenceAssertion(t, "skill", relationshipID, "capability", 0.8,
		shoal.Metadata{AssertionEdgeIDMetadata: "relation"})
	relation := graph.Edge{
		ID: "relation", From: "skill", To: "capability",
		Type: "provides_capability", Weight: 1,
		Properties: shoal.Metadata{
			"ontology_relationship_id": string(relationshipID),
		},
	}

	named := newReferenceAssertion(t, "skill", relationshipID, "tool", 0.5, nil)
	namingEdge := graph.Edge{
		ID: "latent", From: "skill", To: "tool", Type: "provides_tool", Weight: 1,
		Properties: shoal.Metadata{EdgeAssertionIDProperty: string(named.ID())},
	}

	identical := newReferenceAssertion(t, "skill", relationshipID, "skill-2", 0.25, nil)
	identityEdge := graph.Edge{
		ID: identical.ID(), From: "skill", To: "skill-2",
		Type: string(relationshipID), Weight: 0.25,
	}

	byEdge, err := AssertionReferencesByEdge(
		[]ontology.Assertion{extracted, named, identical},
		[]graph.Edge{relation, namingEdge, identityEdge},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		edge      shoal.ID
		assertion ontology.Assertion
	}{
		{relation.ID, extracted},
		{namingEdge.ID, named},
		{identityEdge.ID, identical},
	} {
		got := byEdge[want.edge]
		if len(got) != 1 || got[0] != (interaction.AssertionReference{
			AssertionID: want.assertion.ID(), EdgeID: want.edge,
			Origin: ontology.AssertionInferred,
		}) {
			t.Fatalf("references for %s = %+v, want %s", want.edge, got, want.assertion.ID())
		}
	}
	if len(byEdge) != 3 {
		t.Fatalf("edges bound = %d, want 3: %+v", len(byEdge), byEdge)
	}

	// Sharing the edge's identity binds only when the assertion reproduces
	// the edge; a different weight leaves it bound to nothing.
	heavier := identityEdge
	heavier.Weight = 0.75
	if _, err := AssertionReferencesByEdge(
		[]ontology.Assertion{identical}, []graph.Edge{heavier},
	); err == nil {
		t.Fatal("an assertion sharing an edge's ID without reproducing it was bound")
	}

	// Metadata naming an edge the response does not carry binds nothing.
	if _, err := AssertionReferencesByEdge(
		[]ontology.Assertion{extracted}, []graph.Edge{namingEdge},
	); err == nil {
		t.Fatal("an assertion naming an absent edge was bound")
	}
}
